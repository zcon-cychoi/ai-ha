package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloud-barista/ai-ha/pkg/backup/engine"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/repository"
	"github.com/cloud-barista/ai-ha/pkg/backup/spool"
)

// dirSource is a test Source that archives an existing directory, standing in
// for the Linux partition source (which requires root and mount privileges).
type dirSource struct {
	name string
	root string
}

func (d dirSource) Name() string { return d.name }
func (d dirSource) Prepare(context.Context) (string, func() error, error) {
	return d.root, func() error { return nil }, nil
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()

	// Source tree to back up.
	srcDir := t.TempDir()
	writeFile(t, filepath.Join(srcDir, "hello.txt"), "hello world")
	writeFile(t, filepath.Join(srcDir, "sub", "nested.txt"), "nested content")

	repoDir := t.TempDir()
	repo, err := repository.Open(ctx, repoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	eng := engine.New(repo)
	system := model.SystemInfo{ID: "sys-0123456789abcdef", Hostname: "web01"}

	for _, comp := range []model.CompressionType{model.CompressionZstd, model.CompressionNone} {
		t.Run(string(comp), func(t *testing.T) {
			rec, err := eng.Backup(ctx, system, dirSource{name: "sda1", root: srcDir}, comp)
			if err != nil {
				t.Fatalf("backup: %v", err)
			}
			if rec.Status != model.BackupCompleted {
				t.Fatalf("status = %s (%s), want completed", rec.Status, rec.Message)
			}
			if rec.SizeBytes == 0 {
				t.Fatal("size is zero")
			}

			records, err := eng.List(ctx, system.ID)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			found := false
			for _, r := range records {
				if r.ID == rec.ID {
					found = true
				}
			}
			if !found {
				t.Fatalf("backup %s not found in metadata", rec.ID)
			}

			dest := t.TempDir()
			if err := eng.Restore(ctx, model.RestoreRequest{
				SystemID:    system.ID,
				FileName:    rec.FileName,
				Compression: comp,
				Dest:        dest,
			}); err != nil {
				t.Fatalf("restore: %v", err)
			}

			assertFile(t, filepath.Join(dest, "hello.txt"), "hello world")
			assertFile(t, filepath.Join(dest, "sub", "nested.txt"), "nested content")
		})
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

// A backup must be filed under the system's identifier, not its hostname. In a
// cloud the hostname is derived from an address and gets reused, so two
// unrelated machines would otherwise share a directory and a metadata store.
func TestBackupIsFiledUnderTheSystemID(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "hello.txt"), []byte("hello world"), 0o600); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	repoDir := t.TempDir()
	repo, err := repository.Open(ctx, repoDir)
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	defer repo.Close()

	// Two machines that a cloud gave the same recycled hostname.
	const sharedHostname = "ip-10-0-1-5"
	first := model.SystemInfo{ID: "sys-aaaaaaaaaaaaaaaa", Hostname: sharedHostname}
	second := model.SystemInfo{ID: "sys-bbbbbbbbbbbbbbbb", Hostname: sharedHostname}

	eng := engine.New(repo)
	for _, system := range []model.SystemInfo{first, second} {
		if _, err := eng.Backup(ctx, system, dirSource{name: "sda1", root: srcDir}, model.CompressionNone); err != nil {
			t.Fatalf("backup %s: %v", system.ID, err)
		}
	}

	for _, system := range []model.SystemInfo{first, second} {
		if _, err := os.Stat(filepath.Join(repoDir, system.ID)); err != nil {
			t.Errorf("no directory for system %s: %v", system.ID, err)
		}
		records, err := eng.List(ctx, system.ID)
		if err != nil {
			t.Fatalf("list %s: %v", system.ID, err)
		}
		if len(records) != 1 {
			t.Errorf("system %s has %d records, want 1: the two systems share a metadata store",
				system.ID, len(records))
		}
	}
	if _, err := os.Stat(filepath.Join(repoDir, sharedHostname)); err == nil {
		t.Error("a directory was created under the hostname")
	}
}

// An unidentified system must be refused rather than filed somewhere arbitrary.
func TestBackupRefusesASystemWithoutAnIdentifier(t *testing.T) {
	ctx := context.Background()
	repo, err := repository.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	defer repo.Close()

	_, err = engine.New(repo).Backup(ctx, model.SystemInfo{}, dirSource{name: "sda1", root: t.TempDir()}, model.CompressionNone)
	if err == nil {
		t.Fatal("a backup with no system identifier was accepted")
	}
}

// TestBackupLeavesTheSpoolOutOfTheArchive covers backing up the partition that
// holds the spool directory, as a root backup with the default /tmp does: the
// spool's chunks must not end up inside the archive they make up.
func TestBackupLeavesTheSpoolOutOfTheArchive(t *testing.T) {
	ctx := context.Background()
	part := t.TempDir()
	writeFile(t, filepath.Join(part, "etc", "hostname"), "web01\n")
	if err := os.MkdirAll(filepath.Join(part, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(part, "tmp"))

	repo, err := repository.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	eng := engine.New(repo)
	system := model.SystemInfo{ID: "sys-0123456789abcdef", Hostname: "web01"}

	src := describedSource{dirSource: dirSource{name: "root", root: part}, device: "/dev/sda1", mount: part}
	rec, err := eng.Backup(ctx, system, src, model.CompressionNone)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	dest := t.TempDir()
	if err := eng.Restore(ctx, model.RestoreRequest{SystemID: system.ID, FileName: rec.FileName, Compression: model.CompressionNone, Dest: dest}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	assertFile(t, filepath.Join(dest, "etc", "hostname"), "web01\n")
	entries, err := os.ReadDir(filepath.Join(dest, "tmp"))
	if err != nil {
		t.Fatalf("tmp was not archived: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the archive holds the spool: %v", entries)
	}
}

// TestBackupRefusesASpoolLargerThanTheFreeSpace checks that a spool that could
// fill its filesystem is refused before anything reaches the repository.
func TestBackupRefusesASpoolLargerThanTheFreeSpace(t *testing.T) {
	ctx := context.Background()
	repoDir := t.TempDir()
	repo, err := repository.Open(ctx, repoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	eng := engine.New(repo)
	eng.Resume = spool.Config{ChunkSize: 1 << 40, BufferCap: 1 << 60}
	system := model.SystemInfo{ID: "sys-0123456789abcdef", Hostname: "web01"}

	_, err = eng.Backup(ctx, system, dirSource{name: "sda1", root: seedDir(t, "a.txt")}, model.CompressionNone)
	if !errors.Is(err, spool.ErrInsufficientSpace) {
		t.Fatalf("err = %v, want ErrInsufficientSpace", err)
	}
	files, _ := filepath.Glob(filepath.Join(repoDir, system.ID, "*"+model.BackupExtension+"*"))
	if len(files) != 0 {
		t.Fatalf("the repository holds %v after a refused backup", files)
	}
}
