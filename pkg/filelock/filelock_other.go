//go:build !unix

package filelock

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"
)

// ErrLocked reports that the lock stayed held by another process until the
// timeout elapsed.
var ErrLocked = errors.New("lock is held by another process")

// Lock is unavailable on non-Unix platforms: it relies on flock(2).
type Lock struct{}

// Acquire always errors on non-Unix platforms.
func Acquire(_ context.Context, _ string, _ time.Duration) (*Lock, error) {
	return nil, fmt.Errorf("cross-process file locking is only supported on unix, not %s", runtime.GOOS)
}

// Release is a no-op on non-Unix platforms.
func (l *Lock) Release() error { return nil }
