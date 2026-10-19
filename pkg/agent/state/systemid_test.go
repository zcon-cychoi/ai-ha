package state

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestEnsureSystemIDIsStable(t *testing.T) {
	dir := t.TempDir()

	first, err := EnsureSystemID(dir)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first == "" {
		t.Fatal("no identifier generated")
	}

	second, err := EnsureSystemID(dir)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second != first {
		t.Fatalf("identifier changed between calls: %q then %q", first, second)
	}
}

func TestEnsureSystemIDCreatesThePrivateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "aiha")

	if _, err := EnsureSystemID(dir); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	fi, err := os.Stat(filepath.Join(dir, systemIDFile))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// The file will hold agent credentials' neighbour state; keep it private.
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("system id mode = %v, want 0600", perm)
	}
}

func TestReadSystemIDAbsent(t *testing.T) {
	_, err := ReadSystemID(t.TempDir())
	if !errors.Is(err, ErrNoSystemID) {
		t.Fatalf("err = %v, want ErrNoSystemID", err)
	}
}

// The identifier becomes a directory name in the repository, so a value that
// could escape a path must be refused rather than used.
func TestReadSystemIDRejectsUnusableValues(t *testing.T) {
	for _, bad := range []string{"../escape", "has/slash", "", "  ", "with space", "x" + string(make([]byte, 100))} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, systemIDFile), []byte(bad), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if id, err := ReadSystemID(dir); err == nil {
			t.Errorf("ReadSystemID accepted %q as %q", bad, id)
		}
	}
}

// Two agents starting at once must agree on one identifier rather than each
// believing it created its own.
func TestEnsureSystemIDConcurrent(t *testing.T) {
	dir := t.TempDir()

	const n = 8
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			ids[i], errs[i] = EnsureSystemID(dir)
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	for i, id := range ids {
		if id != ids[0] {
			t.Fatalf("goroutine %d got %q, goroutine 0 got %q", i, id, ids[0])
		}
	}
}
