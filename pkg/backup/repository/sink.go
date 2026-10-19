package repository

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/spool"
)

// ResumableRepository is an optional capability: a backend that can accept a
// backup as an ordered sequence of chunks and assemble them into the target
// file, so an interrupted upload can resume from the last committed chunk.
//
// Backends that do not implement this fall back to the direct streaming Writer.
//
// Implemented:
//   - local, sftp, smb: append each chunk at the target's committed offset; on a
//     reconnect, truncate the target back to that offset to drop a partial tail
//     and re-append, re-establishing the connection as needed (localSink,
//     sftpSink, smbSink).
//   - s3: one chunk = one multipart part; a retried part re-uploads under the
//     same number; Finalize = CompleteMultipartUpload (s3Sink).
//
// Planned:
//   - http: needs a server that supports resumable/append semantics (Range/tus).
type ResumableRepository interface {
	Repository
	// NewSink opens a resumable sink for a new backup file under the system's
	// directory.
	NewSink(ctx context.Context, system model.SystemInfo, filename string) (spool.Sink, error)
}

// ChunkSizeConstrainer is an optional capability of a ResumableRepository whose
// backend requires a minimum chunk size (e.g. S3 multipart parts must be at
// least 5 MiB). The engine raises the spool chunk size to this minimum.
type ChunkSizeConstrainer interface {
	MinChunkSize() int64
}

// localSink appends ordered chunks to a single file on the local filesystem.
// It is the reference implementation of spool.Sink and also exercises the
// resumable path end-to-end in tests.
type localSink struct {
	path           string
	committedBytes int64
	committedIndex int
}

func (r *localRepository) NewSink(_ context.Context, system model.SystemInfo, filename string) (spool.Sink, error) {
	dir, err := r.systemDir(system)
	if err != nil {
		return nil, err
	}
	if filename != filepath.Base(filename) {
		return nil, fmt.Errorf("invalid backup file name %q", filename)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &localSink{path: filepath.Join(dir, filename)}, nil
}

func (s *localSink) Put(_ context.Context, index int, data io.Reader, _ int64) error {
	if index < s.committedIndex {
		return nil // already committed (idempotent retry)
	}
	if index != s.committedIndex {
		return fmt.Errorf("out-of-order chunk %d, expected %d", index, s.committedIndex)
	}

	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()

	// Repair any partial tail left by a previous failed attempt, then append.
	if err := f.Truncate(s.committedBytes); err != nil {
		return err
	}
	if _, err := f.Seek(s.committedBytes, io.SeekStart); err != nil {
		return err
	}
	n, err := io.Copy(f, data)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}

	s.committedBytes += n
	s.committedIndex++
	return nil
}

func (s *localSink) Finalize(_ context.Context) error { return nil }

func (s *localSink) Abort(_ context.Context) error {
	err := os.Remove(s.path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
