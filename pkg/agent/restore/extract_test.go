package restore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// stubRclone puts an "rclone" on PATH that writes the file named by
// AIHA_TEST_ARCHIVE, or only its first bytes and then fails when
// AIHA_TEST_CUT is set, as a connection dropped mid-transfer would.
func stubRclone(t *testing.T, archive string, cut bool) {
	t.Helper()
	for _, tool := range []string{"tar", "gzip"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nexec cat \"$AIHA_TEST_ARCHIVE\"\n"
	if cut {
		script = "#!/bin/sh\nhead -c 64 \"$AIHA_TEST_ARCHIVE\"\necho 'connection reset' >&2\nexit 1\n"
	}
	if err := os.WriteFile(filepath.Join(bin, "rclone"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AIHA_TEST_ARCHIVE", archive)
}

// gzipTar writes a gzip-compressed tar of files and returns its path and size.
func gzipTar(t *testing.T, files map[string]string) (string, int64) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o640, Size: int64(len(content)), Uid: 12345, Gid: 12345, Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "backup.aha")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, int64(buf.Len())
}

func TestExtractUnpacksAndCountsRepositoryBytes(t *testing.T) {
	archive, size := gzipTar(t, map[string]string{"etc/hostname": "web01\n", "README": strings.Repeat("x", 10000)})
	stubRclone(t, archive, false)
	dest := t.TempDir()

	var read atomic.Int64
	src := source{rcloneArgs: []string{"cat", "aiha:b/f"}, decompress: []string{"gzip", "-d", "-c"}}
	if err := extract(context.Background(), src, dest, &read); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if read.Load() != size {
		t.Fatalf("counted %d bytes, the file is %d", read.Load(), size)
	}
	data, err := os.ReadFile(filepath.Join(dest, "etc/hostname"))
	if err != nil || string(data) != "web01\n" {
		t.Fatalf("hostname = %q, %v", data, err)
	}
}

func TestExtractReportsACutStream(t *testing.T) {
	archive, _ := gzipTar(t, map[string]string{"README": strings.Repeat("x", 10000)})
	stubRclone(t, archive, true)

	var read atomic.Int64
	src := source{rcloneArgs: []string{"cat", "aiha:b/f"}, decompress: []string{"gzip", "-d", "-c"}}
	err := extract(context.Background(), src, t.TempDir(), &read)
	if err == nil {
		t.Fatal("extract succeeded on a stream cut short")
	}
	if !strings.Contains(err.Error(), "read from the repository") || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err = %v, want the repository read named with rclone's reason", err)
	}
}

func TestExtractReportsACorruptArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.aha")
	if err := os.WriteFile(path, bytes.Repeat([]byte("not a tar archive "), 100), 0o600); err != nil {
		t.Fatal(err)
	}
	stubRclone(t, path, false)

	var read atomic.Int64
	err := extract(context.Background(), source{rcloneArgs: []string{"cat", "aiha:b/f"}}, t.TempDir(), &read)
	if err == nil || !strings.Contains(err.Error(), "unpack") {
		t.Fatalf("err = %v, want tar's failure", err)
	}
}
