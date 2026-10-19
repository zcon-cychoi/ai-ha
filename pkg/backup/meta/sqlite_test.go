package meta_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/backup/meta"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
)

func openStore(t *testing.T) (meta.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	store, err := meta.OpenSQLite(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, ctx
}

func sampleRecord() model.BackupRecord {
	return model.BackupRecord{
		ID:          "b1",
		Hostname:    "web01",
		Partition:   "sda1",
		FileName:    "web01-sda1-deadbeef.aha",
		Compression: model.CompressionZstd,
		Scheme:      model.SchemeS3,
		Status:      model.BackupRunning,
		CreatedAt:   time.Now().UTC().Truncate(time.Millisecond),
	}
}

// TestUpdateBackupWithoutInsert covers a backup whose "running" row never made
// it into the database: the completed record must still be stored in full
// rather than silently dropped.
func TestUpdateBackupWithoutInsert(t *testing.T) {
	store, ctx := openStore(t)

	rec := sampleRecord()
	completed := time.Now().UTC().Truncate(time.Millisecond)
	rec.Status = model.BackupCompleted
	rec.SizeBytes = 4096
	rec.CompletedAt = &completed

	if err := store.UpdateBackup(ctx, rec); err != nil {
		t.Fatalf("update backup: %v", err)
	}

	got, err := store.GetBackup(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get backup: %v", err)
	}
	if got.FileName != rec.FileName || got.Partition != rec.Partition || got.SizeBytes != rec.SizeBytes {
		t.Fatalf("record = %+v, want the full record %+v", got, rec)
	}
	if got.Status != model.BackupCompleted {
		t.Fatalf("status = %s, want completed", got.Status)
	}
}

// TestUpdateBackupAfterInsert verifies the normal path still updates in place
// instead of duplicating the row.
func TestUpdateBackupAfterInsert(t *testing.T) {
	store, ctx := openStore(t)

	rec := sampleRecord()
	if err := store.InsertBackup(ctx, rec); err != nil {
		t.Fatalf("insert backup: %v", err)
	}

	completed := time.Now().UTC().Truncate(time.Millisecond)
	rec.Status = model.BackupCompleted
	rec.SizeBytes = 8192
	rec.CompletedAt = &completed
	if err := store.UpdateBackup(ctx, rec); err != nil {
		t.Fatalf("update backup: %v", err)
	}

	records, err := store.ListBackups(ctx)
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(records), records)
	}
	if records[0].Status != model.BackupCompleted || records[0].SizeBytes != 8192 {
		t.Fatalf("record = %+v, want the completed outcome", records[0])
	}
	if records[0].CompletedAt == nil {
		t.Fatal("completedAt not recorded")
	}
	// The creation time belongs to the original record, not the update.
	if !records[0].CreatedAt.Equal(rec.CreatedAt) {
		t.Fatalf("createdAt = %s, want %s", records[0].CreatedAt, rec.CreatedAt)
	}
}
