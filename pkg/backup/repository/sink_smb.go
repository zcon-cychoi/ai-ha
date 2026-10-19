package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"

	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/spool"
	"github.com/cloudsoda/go-smb2"
	"github.com/rs/zerolog/log"
)

// NewSink opens a resumable SMB sink for a new backup file. The sink appends
// spooled chunks directly to the target file at its committed offset and can
// re-dial the share on its own after a network interruption, so it satisfies
// repository.ResumableRepository.
func (r *smbRepository) NewSink(_ context.Context, system model.SystemInfo, filename string) (spool.Sink, error) {
	dir, err := r.systemDir(system)
	if err != nil {
		return nil, err
	}
	if filename != path.Base(filename) {
		return nil, fmt.Errorf("invalid backup file name %q", filename)
	}
	if err := r.share.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &smbSink{dialer: r.dialer, path: path.Join(dir, filename)}, nil
}

// smbSink appends ordered chunks to a single file on an SMB/CIFS share. It owns
// a dedicated connection (separate from the repository's metadata connection) so
// it can reconnect independently. On any transfer error the connection is
// dropped and the next Put re-dials; the spool retries the same chunk until it
// succeeds or the disconnect timeout elapses.
//
// Resumability within a session relies on append-at-committed-offset: before
// writing chunk N the target is truncated back to committedBytes, discarding any
// partial tail left by a failed attempt, then the chunk is appended.
type smbSink struct {
	dialer *smbDialer
	path   string // share-relative target path (forward-slash)

	conn    net.Conn
	session *smb2.Session
	share   *smb2.Share

	committedBytes int64
	committedIndex int
}

// connect returns a live share, dialing a fresh connection if the previous one
// was dropped after an error.
func (s *smbSink) connect(ctx context.Context) (*smb2.Share, error) {
	if s.share != nil {
		return s.share, nil
	}
	conn, session, share, err := s.dialer.dial(ctx)
	if err != nil {
		return nil, err
	}
	s.conn, s.session, s.share = conn, session, share
	return share, nil
}

// reset tears down the current connection so the next connect call re-dials.
func (s *smbSink) reset() {
	closeSMB(s.conn, s.session, s.share)
	s.conn, s.session, s.share = nil, nil, nil
}

func (s *smbSink) Put(ctx context.Context, index int, data io.Reader, _ int64) error {
	if index < s.committedIndex {
		return nil // already committed (idempotent retry)
	}
	if index != s.committedIndex {
		return fmt.Errorf("out-of-order chunk %d, expected %d", index, s.committedIndex)
	}

	share, err := s.connect(ctx)
	if err != nil {
		return err
	}
	n, err := s.appendChunk(share, data)
	if err != nil {
		// Drop the (likely broken) connection; the next retry re-dials and
		// truncates back to committedBytes before re-appending.
		s.reset()
		return err
	}

	s.committedBytes += n
	s.committedIndex++
	return nil
}

// appendChunk truncates the target to the committed offset (dropping a partial
// tail from a prior failed attempt), seeks there and appends the chunk.
func (s *smbSink) appendChunk(share *smb2.Share, data io.Reader) (int64, error) {
	f, err := share.OpenFile(s.path, os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	if err := f.Truncate(s.committedBytes); err != nil {
		return 0, err
	}
	if _, err := f.Seek(s.committedBytes, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.Copy(f, data)
	if err != nil {
		return n, err
	}
	return n, nil
}

// Finalize releases the connection. The target file is already complete because
// chunks are appended directly to it.
func (s *smbSink) Finalize(context.Context) error {
	s.reset()
	return nil
}

// Abort removes the partial target file and releases the connection.
func (s *smbSink) Abort(ctx context.Context) error {
	share, err := s.connect(ctx)
	if err != nil {
		s.reset()
		return err
	}
	rmErr := share.Remove(s.path)
	s.reset()
	if rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		log.Warn().Err(rmErr).Str("path", s.path).Msg("Failed to remove aborted SMB backup file")
		return rmErr
	}
	return nil
}
