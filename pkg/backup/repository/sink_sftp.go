package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/spool"
	"github.com/pkg/sftp"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"
)

// NewSink opens a resumable SFTP sink for a new backup file. The sink appends
// spooled chunks directly to the target file at its committed offset and can
// re-dial the server on its own after a network interruption, so it satisfies
// repository.ResumableRepository.
func (r *sftpRepository) NewSink(_ context.Context, system model.SystemInfo, filename string) (spool.Sink, error) {
	dir, err := r.systemDir(system)
	if err != nil {
		return nil, err
	}
	if filename != path.Base(filename) {
		return nil, fmt.Errorf("invalid backup file name %q", filename)
	}
	if err := r.client.MkdirAll(dir); err != nil {
		return nil, err
	}
	return &sftpSink{dialer: r.dialer, path: path.Join(dir, filename)}, nil
}

// sftpSink appends ordered chunks to a single remote file over SFTP. It owns a
// dedicated connection (separate from the repository's metadata connection) so
// it can reconnect independently. On any transfer error the connection is
// dropped and the next Put re-dials; the spool retries the same chunk until it
// succeeds or the disconnect timeout elapses.
//
// Resumability within a session relies on append-at-committed-offset: before
// writing chunk N the target is truncated back to committedBytes, discarding any
// partial tail left by a failed attempt, then the chunk is appended.
type sftpSink struct {
	dialer *sftpDialer
	path   string // remote target path (POSIX)

	ssh    *ssh.Client
	client *sftp.Client

	committedBytes int64
	committedIndex int
}

// conn returns a live SFTP client, dialing a fresh connection if the previous
// one was dropped after an error.
func (s *sftpSink) conn(ctx context.Context) (*sftp.Client, error) {
	if s.client != nil {
		return s.client, nil
	}
	sshClient, client, err := s.dialer.dial(ctx)
	if err != nil {
		return nil, err
	}
	s.ssh, s.client = sshClient, client
	return client, nil
}

// reset tears down the current connection so the next conn call re-dials.
func (s *sftpSink) reset() {
	if s.client != nil {
		_ = s.client.Close()
	}
	if s.ssh != nil {
		_ = s.ssh.Close()
	}
	s.client, s.ssh = nil, nil
}

func (s *sftpSink) Put(ctx context.Context, index int, data io.Reader, _ int64) error {
	if index < s.committedIndex {
		return nil // already committed (idempotent retry)
	}
	if index != s.committedIndex {
		return fmt.Errorf("out-of-order chunk %d, expected %d", index, s.committedIndex)
	}

	client, err := s.conn(ctx)
	if err != nil {
		return err
	}
	n, err := s.appendChunk(client, data)
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
func (s *sftpSink) appendChunk(client *sftp.Client, data io.Reader) (int64, error) {
	f, err := client.OpenFile(s.path, os.O_CREATE|os.O_WRONLY)
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
func (s *sftpSink) Finalize(context.Context) error {
	s.reset()
	return nil
}

// Abort removes the partial target file and releases the connection.
func (s *sftpSink) Abort(ctx context.Context) error {
	client, err := s.conn(ctx)
	if err != nil {
		s.reset()
		return err
	}
	rmErr := client.Remove(s.path)
	s.reset()
	if rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		log.Warn().Err(rmErr).Str("path", s.path).Msg("Failed to remove aborted SFTP backup file")
		return rmErr
	}
	return nil
}
