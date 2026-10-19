package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestArchiveExcludes verifies that excluded paths (and their subtrees) are
// omitted while sibling entries are preserved.
func TestArchiveExcludes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dev", "null.txt"), "device")                // contents excluded
	writeFile(t, filepath.Join(root, "dev", "sub", "x"), "deep")                  // contents excluded
	writeFile(t, filepath.Join(root, "etc", "hostname"), "host")                  // kept
	writeFile(t, filepath.Join(root, "var", "log", "app.log"), "hi")              // kept
	writeFile(t, filepath.Join(root, "mnt", "aiha-mount-abc", "spooled"), "junk") // excluded whole
	writeFile(t, filepath.Join(root, "mnt", "data", "real"), "keep")              // kept (mnt itself stays)

	excludes := []*regexp.Regexp{
		regexp.MustCompile(`^dev/`),            // contents only; keep the dir entry
		regexp.MustCompile(`^mnt/aiha-mount-`), // whole subtree including the dir
	}

	var buf bytes.Buffer
	a := NewTar()
	if err := a.Archive(context.Background(), root, &buf, excludes); err != nil {
		t.Fatalf("archive: %v", err)
	}

	dest := t.TempDir()
	if err := a.Extract(context.Background(), &buf, dest); err != nil {
		t.Fatalf("extract: %v", err)
	}

	// The /dev directory entry is kept, but its contents are excluded.
	if !exists(filepath.Join(dest, "dev")) {
		t.Error("expected the dev directory itself to be kept")
	}
	for _, p := range []string{"dev/null.txt", "dev/sub", "dev/sub/x", "mnt/aiha-mount-abc"} {
		if exists(filepath.Join(dest, p)) {
			t.Errorf("expected %q to be excluded, but it exists", p)
		}
	}
	// Kept.
	for _, p := range []string{"etc/hostname", "var/log/app.log", "mnt/data/real"} {
		if !exists(filepath.Join(dest, p)) {
			t.Errorf("expected %q to be archived, but it is missing", p)
		}
	}
}

// TestArchiveNoExcludes verifies a nil exclude list archives everything.
func TestArchiveNoExcludes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a", "b.txt"), "content")

	var buf bytes.Buffer
	a := NewTar()
	if err := a.Archive(context.Background(), root, &buf, nil); err != nil {
		t.Fatalf("archive: %v", err)
	}
	dest := t.TempDir()
	if err := a.Extract(context.Background(), &buf, dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !exists(filepath.Join(dest, "a", "b.txt")) {
		t.Fatal("expected a/b.txt to be archived")
	}
}
