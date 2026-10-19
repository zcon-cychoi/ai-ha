//go:build unix

// Package filelock provides an advisory, cross-process exclusive lock on a file,
// shared by the backup repository (metadata updates) and the agent (one agent
// per recovery).
package filelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// pollInterval is how often a contended lock is retried.
const pollInterval = 50 * time.Millisecond

// ErrLocked reports that the lock stayed held by another process until the
// timeout elapsed.
var ErrLocked = errors.New("lock is held by another process")

// Lock is an advisory, cross-process exclusive lock backed by flock(2). The
// kernel releases it when the holding process exits, so a crashed or killed
// holder never leaves the file locked.
type Lock struct {
	f *os.File
}

// Acquire takes an exclusive lock on path, creating the file if needed. It
// polls with LOCK_NB instead of blocking inside flock so that ctx cancellation
// and timeout are honored. A zero timeout makes a single attempt and fails with
// ErrLocked if the lock is held.
func Acquire(ctx context.Context, path string, timeout time.Duration) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Lock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("timed out after %s waiting for lock %s: %w", timeout, path, ErrLocked)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// Release unlocks the file and closes the descriptor.
func (l *Lock) Release() error {
	if err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN); err != nil {
		_ = l.f.Close()
		return fmt.Errorf("unlock: %w", err)
	}
	return l.f.Close()
}
