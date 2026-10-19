// Package meta stores per-system backup metadata. Each target system has its
// own SQLite database file holding the system record and its backup history,
// which is the source of truth for listing and restore.
package meta

import (
	"context"

	"github.com/cloud-barista/ai-ha/pkg/backup/model"
)

// Store is the metadata persistence interface for a single target system.
type Store interface {
	// Init creates the schema if it does not yet exist.
	Init(ctx context.Context) error
	// UpsertSystem inserts or updates the system record.
	UpsertSystem(ctx context.Context, system model.SystemInfo) error
	// UpsertBackupSet records a backup set. The set's manifest in the
	// repository is the source of truth; this is the index that makes listing
	// cheap, and it can be rebuilt from the manifests.
	UpsertBackupSet(ctx context.Context, set model.BackupSet) error
	// ListBackupSets returns the system's backup sets, newest first.
	ListBackupSets(ctx context.Context) ([]model.BackupSet, error)
	// InsertBackup records a new backup (typically in "running" state).
	InsertBackup(ctx context.Context, rec model.BackupRecord) error
	// UpdateBackup updates an existing backup record (status/size/message).
	UpdateBackup(ctx context.Context, rec model.BackupRecord) error
	// ListBackups returns all backup records, newest first.
	ListBackups(ctx context.Context) ([]model.BackupRecord, error)
	// GetBackup returns a single backup record by id.
	GetBackup(ctx context.Context, id string) (model.BackupRecord, error)
	// Close releases the underlying database handle.
	Close() error
}
