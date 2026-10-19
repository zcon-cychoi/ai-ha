package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cloud-barista/ai-ha/pkg/backup/engine"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/repository"
	"github.com/cloud-barista/ai-ha/pkg/backup/source"
)

// describedSource reports a device and mount point, as a real partition source
// does, so the manifest can be checked for them.
type describedSource struct {
	dirSource
	device string
	mount  string
}

func (d describedSource) Device() string                    { return d.device }
func (d describedSource) MountPoint(context.Context) string { return d.mount }

func seedDir(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("content of "+name), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return dir
}

func readManifest(t *testing.T, repoDir, systemID, setID string) model.Manifest {
	t.Helper()
	path := filepath.Join(repoDir, systemID, model.ManifestName(setID))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest %s: %v", path, err)
	}
	var m model.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return m
}

func TestBackupSetWritesARestorableManifest(t *testing.T) {
	ctx := context.Background()
	repoDir := t.TempDir()
	repo, err := repository.Open(ctx, repoDir)
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	defer repo.Close()

	system := model.SystemInfo{ID: "sys-0123456789abcdef", Hostname: "web01"}
	sources := []source.Source{
		describedSource{dirSource{name: "nvme0n1p1", root: seedDir(t, "root.txt")}, "/dev/nvme0n1p1", "/"},
		describedSource{dirSource{name: "nvme0n1p16", root: seedDir(t, "efi.txt")}, "/dev/nvme0n1p16", "/boot/efi"},
	}

	set, err := engine.New(repo).BackupSet(ctx, system, sources, model.CompressionNone)
	if err != nil {
		t.Fatalf("backup set: %v", err)
	}
	if set.Status != model.BackupCompleted {
		t.Fatalf("set status = %s (%s)", set.Status, set.Message)
	}
	if len(set.Items) != 2 {
		t.Fatalf("set has %d items, want 2", len(set.Items))
	}

	m := readManifest(t, repoDir, system.ID, set.ID)

	if m.Version != model.ManifestVersion {
		t.Errorf("manifest version = %d, want %d", m.Version, model.ManifestVersion)
	}
	if m.Set.SystemID != system.ID {
		t.Errorf("manifest systemId = %q, want %q", m.Set.SystemID, system.ID)
	}

	// The mount points are the whole point: a restore matches the capture to
	// the target by them, not by reproducing the partition table.
	byMount := map[string]model.BackupItem{}
	for _, item := range m.Set.Items {
		byMount[item.MountPoint] = item
	}
	for _, want := range []string{"/", "/boot/efi"} {
		item, ok := byMount[want]
		if !ok {
			t.Fatalf("manifest has no item mounted at %q: %+v", want, m.Set.Items)
		}
		if item.Device == "" {
			t.Errorf("item at %q records no device", want)
		}
		if item.FileName == "" {
			t.Errorf("item at %q records no archive file", want)
		}
		if _, err := os.Stat(filepath.Join(repoDir, system.ID, item.FileName)); err != nil {
			t.Errorf("archive for %q missing: %v", want, err)
		}
	}

	// A directory source excludes nothing, so nothing is declared here. What a
	// partition source declares is covered in TestManifestDeclaresExclusions.
	if len(m.ExcludedPaths) != 0 {
		t.Errorf("a source that excludes nothing declared %v", m.ExcludedPaths)
	}

	// System facts are collected on Linux; the CI and dev hosts are Linux.
	if len(m.System) == 0 {
		t.Error("manifest carries no system information")
	}
}

// A set is also recorded in the index, so listing does not have to read every
// manifest. The manifest stays the source of truth.
func TestBackupSetIsListed(t *testing.T) {
	ctx := context.Background()
	repo, err := repository.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	defer repo.Close()

	system := model.SystemInfo{ID: "sys-0123456789abcdef", Hostname: "web01"}
	eng := engine.New(repo)

	set, err := eng.BackupSet(ctx, system,
		[]source.Source{dirSource{name: "sda1", root: seedDir(t, "a.txt")}}, model.CompressionNone)
	if err != nil {
		t.Fatalf("backup set: %v", err)
	}

	sets, err := eng.ListSets(ctx, system.ID)
	if err != nil {
		t.Fatalf("list sets: %v", err)
	}
	if len(sets) != 1 || sets[0].ID != set.ID {
		t.Fatalf("listed %+v, want the set %s", sets, set.ID)
	}
	if len(sets[0].Items) != 1 || sets[0].Items[0].Partition != "sda1" {
		t.Errorf("listed set lost its items: %+v", sets[0].Items)
	}
}

// One partition failing must not abandon the others: a partial capture that
// says so is worth more than none.
func TestBackupSetContinuesAfterAFailedItem(t *testing.T) {
	ctx := context.Background()
	repoDir := t.TempDir()
	repo, err := repository.Open(ctx, repoDir)
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	defer repo.Close()

	system := model.SystemInfo{ID: "sys-0123456789abcdef", Hostname: "web01"}
	sources := []source.Source{
		dirSource{name: "good", root: seedDir(t, "a.txt")},
		dirSource{name: "bad", root: filepath.Join(t.TempDir(), "does-not-exist")},
		dirSource{name: "good2", root: seedDir(t, "b.txt")},
	}

	set, err := engine.New(repo).BackupSet(ctx, system, sources, model.CompressionNone)
	if err == nil {
		t.Fatal("a set with a failed item reported success")
	}
	if set.Status != model.BackupFailed {
		t.Errorf("set status = %s, want failed", set.Status)
	}
	if len(set.Items) != 3 {
		t.Fatalf("set has %d items, want all 3 recorded", len(set.Items))
	}

	var completed, failed int
	for _, item := range set.Items {
		switch item.Status {
		case model.BackupCompleted:
			completed++
		case model.BackupFailed:
			failed++
		}
	}
	if completed != 2 || failed != 1 {
		t.Errorf("completed=%d failed=%d, want 2 and 1: the set stopped early", completed, failed)
	}

	// The manifest is written even for a failed set — it is the only record
	// of what was attempted.
	m := readManifest(t, repoDir, system.ID, set.ID)
	if m.Set.Status != model.BackupFailed || m.Set.Message == "" {
		t.Errorf("manifest does not record the failure: status=%s message=%q", m.Set.Status, m.Set.Message)
	}
}

// A job reporter reads Progress while the backup runs. It has to rise to what
// was actually written, across every partition of the set.
func TestBackupSetReportsProgress(t *testing.T) {
	ctx := context.Background()
	repo, err := repository.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	defer repo.Close()

	eng := engine.New(repo)
	eng.Progress = new(atomic.Int64)

	system := model.SystemInfo{ID: "sys-0123456789abcdef", Hostname: "web01"}
	set, err := eng.BackupSet(ctx, system, []source.Source{
		dirSource{name: "a", root: seedDir(t, "a.txt")},
		dirSource{name: "b", root: seedDir(t, "b.txt")},
	}, model.CompressionNone)
	if err != nil {
		t.Fatalf("backup set: %v", err)
	}

	var written int64
	for _, item := range set.Items {
		written += item.SizeBytes
	}
	if got := eng.Progress.Load(); got == 0 || got != written {
		t.Fatalf("progress = %d, want the %d bytes written across the set", got, written)
	}
}
