package repository

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cloud-barista/ai-ha/pkg/backup/meta"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
)

// metaFileName is the per-system SQLite metadata file.
const metaFileName = "meta.db"

// localRepository stores backups on the local filesystem: one directory per
// system under base, each containing the backup files and the SQLite metadata.
type localRepository struct {
	base string
}

func newLocal(base string) (Repository, error) {
	if base == "" {
		return nil, fmt.Errorf("local repository path is required")
	}
	abs, err := filepath.Abs(base)
	if err != nil {
		return nil, fmt.Errorf("resolve repository path: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("create repository dir: %w", err)
	}
	return &localRepository{base: abs}, nil
}

func (r *localRepository) Scheme() model.TransportScheme { return model.SchemeLocal }

// systemDir returns the directory for a system, guarding against path
// traversal via a crafted hostname.
func (r *localRepository) systemDir(system model.SystemInfo) (string, error) {
	name := system.DirName()
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return "", fmt.Errorf("invalid system name %q", name)
	}
	return filepath.Join(r.base, name), nil
}

func (r *localRepository) EnsureSystem(ctx context.Context, system model.SystemInfo) error {
	dir, err := r.systemDir(system)
	if err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o750)
}

func (r *localRepository) NewWriter(ctx context.Context, system model.SystemInfo, filename string) (io.WriteCloser, error) {
	dir, err := r.systemDir(system)
	if err != nil {
		return nil, err
	}
	if filename != filepath.Base(filename) {
		return nil, fmt.Errorf("invalid backup file name %q", filename)
	}
	// O_EXCL guards against overwriting an existing backup file.
	f, err := os.OpenFile(filepath.Join(dir, filename), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("create backup file: %w", err)
	}
	return f, nil
}

func (r *localRepository) NewReader(ctx context.Context, system model.SystemInfo, filename string) (io.ReadCloser, error) {
	dir, err := r.systemDir(system)
	if err != nil {
		return nil, err
	}
	if filename != filepath.Base(filename) {
		return nil, fmt.Errorf("invalid backup file name %q", filename)
	}
	f, err := os.Open(filepath.Join(dir, filename))
	if err != nil {
		return nil, fmt.Errorf("open backup file: %w", err)
	}
	return f, nil
}

func (r *localRepository) Meta(ctx context.Context, system model.SystemInfo) (meta.Store, error) {
	dir, err := r.systemDir(system)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return meta.OpenSQLite(ctx, filepath.Join(dir, metaFileName))
}

// UpdateMeta applies fn to the system's metadata. The local backend edits the
// SQLite file in place, so concurrent updates are serialized by SQLite's own
// file locking rather than by a separate lock file.
func (r *localRepository) UpdateMeta(ctx context.Context, system model.SystemInfo, fn func(context.Context, meta.Store) error) error {
	store, err := r.Meta(ctx, system)
	if err != nil {
		return err
	}
	if err := fn(ctx, store); err != nil {
		_ = store.Close()
		return err
	}
	return store.Close()
}

func (r *localRepository) Close() error { return nil }
