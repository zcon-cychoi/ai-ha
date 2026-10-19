package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/backup/meta"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/filelock"
	"github.com/rs/zerolog/log"
)

// metaLockTimeout bounds how long a metadata update waits for another process
// to release the lock. The locked section is one download/modify/upload round
// trip, so exceeding this means something is stuck rather than merely busy.
const metaLockTimeout = 2 * time.Minute

// lockSubDir groups the backup lock files inside the configured lock directory.
const lockSubDir = "aiha-backup"

// metaBackend supplies the backend-specific transfer of a system's metadata
// file. It is implemented by the remote repositories (s3, sftp, smb), which
// cannot run SQLite against remote storage and must work on a local copy.
type metaBackend interface {
	// metaLocation returns the backend location (object key / remote path) of
	// the system's metadata file.
	metaLocation(system model.SystemInfo) (string, error)
	// downloadMeta copies the metadata at remote into the existing localPath.
	// A missing remote file is not an error: localPath is left empty so a fresh
	// database is created.
	downloadMeta(ctx context.Context, remote, localPath string) error
	// uploadMeta replaces the metadata at remote with the contents of localPath.
	uploadMeta(ctx context.Context, localPath, remote string) error
}

// metaSync implements the shared metadata workflow for the remote backends: a
// read opens a throwaway local copy, and an update holds an exclusive lock
// across the whole download -> modify -> upload cycle.
//
// Keeping that cycle short and locked is what makes concurrent partition
// backups safe. The remote file stays the source of truth, so no backup can
// finish holding a stale copy and overwrite records another backup wrote in the
// meantime.
type metaSync struct {
	cache   string     // process-local directory for working copies
	lockDir string     // directory holding the cross-process lock files
	repoID  string     // stable identifier of the repository, for lock naming
	mu      sync.Mutex // serializes updates within this process
}

// newMetaSync prepares the shared metadata workflow for a repository, creating
// the lock and working-copy directories up front so a misconfigured lock
// directory fails before a long-running backup starts.
//
// Only a hash of uri is kept, so credentials embedded in a repository URI never
// reach a lock file name.
func newMetaSync(uri, cachePrefix, lockDir string) (*metaSync, error) {
	if lockDir == "" {
		lockDir = DefaultLockDir
	}
	dir := filepath.Join(lockDir, lockSubDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create lock dir %q: %w", dir, err)
	}

	cache, err := os.MkdirTemp("", cachePrefix)
	if err != nil {
		return nil, fmt.Errorf("create metadata cache: %w", err)
	}

	return &metaSync{cache: cache, lockDir: dir, repoID: shortHash(uri)}, nil
}

// lockPath returns the lock file guarding a system's metadata. The name is
// derived from hashes so it is filesystem-safe regardless of the repository URI
// and system name.
func (m *metaSync) lockPath(system model.SystemInfo) string {
	name := system.DirName()
	return filepath.Join(m.lockDir, fmt.Sprintf("meta-%s-%s.lock", m.repoID, shortHash(name)))
}

// open returns a read-only view of the system's metadata. Changes made through
// the returned Store are NOT persisted; use update for that. Closing it removes
// the local working copy.
func (m *metaSync) open(ctx context.Context, b metaBackend, system model.SystemInfo) (meta.Store, error) {
	remote, err := b.metaLocation(system)
	if err != nil {
		return nil, err
	}

	localPath, err := m.workingCopy(ctx, b, remote)
	if err != nil {
		return nil, err
	}

	store, err := meta.OpenSQLite(ctx, localPath)
	if err != nil {
		_ = os.Remove(localPath)
		return nil, err
	}
	return &tempStore{Store: store, path: localPath}, nil
}

// update applies fn to the system's metadata and persists the result. The whole
// cycle is serialized against other backup processes by a lock file and against
// other goroutines by mu.
//
// The remote metadata is replaced only when fn succeeds, so a failed update
// never publishes a half-applied database.
func (m *metaSync) update(ctx context.Context, b metaBackend, system model.SystemInfo, fn func(context.Context, meta.Store) error) error {
	remote, err := b.metaLocation(system)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	lock, err := filelock.Acquire(ctx, m.lockPath(system), metaLockTimeout)
	if err != nil {
		return fmt.Errorf("acquire metadata lock: %w", err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			log.Error().Err(err).Msg("Failed to release metadata lock")
		}
	}()

	localPath, err := m.workingCopy(ctx, b, remote)
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(localPath); err != nil {
			log.Error().Err(err).Msg("Failed to remove local metadata copy")
		}
	}()

	store, err := meta.OpenSQLite(ctx, localPath)
	if err != nil {
		return err
	}
	if err := fn(ctx, store); err != nil {
		_ = store.Close()
		return err
	}
	// Close before uploading so every write is flushed to the file.
	if err := store.Close(); err != nil {
		return fmt.Errorf("close metadata: %w", err)
	}
	return b.uploadMeta(ctx, localPath, remote)
}

// cleanup removes the process-local working-copy directory.
func (m *metaSync) cleanup() error {
	return os.RemoveAll(m.cache)
}

// workingCopy downloads remote into a fresh local file and returns its path.
func (m *metaSync) workingCopy(ctx context.Context, b metaBackend, remote string) (string, error) {
	local, err := os.CreateTemp(m.cache, "meta-*.db")
	if err != nil {
		return "", fmt.Errorf("create local metadata copy: %w", err)
	}
	localPath := local.Name()
	if err := local.Close(); err != nil {
		_ = os.Remove(localPath)
		return "", err
	}

	if err := b.downloadMeta(ctx, remote, localPath); err != nil {
		_ = os.Remove(localPath)
		return "", err
	}
	return localPath, nil
}

// tempStore is a metadata Store backed by a local working copy that is removed
// on Close. It is the read-only counterpart to metaSync.update.
type tempStore struct {
	meta.Store
	path string
}

func (s *tempStore) Close() error {
	return errors.Join(s.Store.Close(), os.Remove(s.path))
}

// shortHash returns a filesystem-safe 16-character digest of s.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
