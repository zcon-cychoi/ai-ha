//go:build unix

package filelock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestAcquireExcludes verifies that a second acquisition waits for the first
// holder to release.
func TestAcquireExcludes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.lock")

	first, err := Acquire(ctx, path, time.Second)
	if err != nil {
		t.Fatalf("acquire first: %v", err)
	}

	acquired := make(chan error, 1)
	go func() {
		second, err := Acquire(ctx, path, 5*time.Second)
		if err == nil {
			err = second.Release()
		}
		acquired <- err
	}()

	select {
	case err := <-acquired:
		t.Fatalf("second acquisition succeeded while the lock was held (err=%v)", err)
	case <-time.After(3 * pollInterval):
	}

	if err := first.Release(); err != nil {
		t.Fatalf("release first: %v", err)
	}
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("acquire second after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second acquisition did not proceed after release")
	}
}

// TestAcquireTimeout verifies a contended lock gives up rather than waiting
// forever, reporting ErrLocked.
func TestAcquireTimeout(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.lock")

	held, err := Acquire(ctx, path, time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer held.Release()

	if _, err := Acquire(ctx, path, 100*time.Millisecond); !errors.Is(err, ErrLocked) {
		t.Fatalf("expected ErrLocked while the lock was held, got %v", err)
	}
}

// TestAcquireZeroTimeoutFailsFast verifies a zero timeout makes one attempt and
// returns immediately, which the agent relies on to refuse a duplicate run.
func TestAcquireZeroTimeoutFailsFast(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.lock")

	held, err := Acquire(ctx, path, time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer held.Release()

	start := time.Now()
	_, err = Acquire(ctx, path, 0)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("expected ErrLocked, got %v", err)
	}
	if elapsed := time.Since(start); elapsed >= pollInterval {
		t.Fatalf("zero timeout waited %s; want a single attempt", elapsed)
	}
}

// TestAcquireContextCancel verifies that a caller waiting on a held lock stops
// when its context is cancelled.
func TestAcquireContextCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")

	held, err := Acquire(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer held.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Acquire(ctx, path, time.Minute); err == nil {
		t.Fatal("expected the cancelled context to abort the wait")
	}
}
