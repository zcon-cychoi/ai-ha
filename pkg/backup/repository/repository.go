// Package repository abstracts backup storage backends.
//
// A Repository is both the storage location and the live-streaming target for a
// backup: its Writer streams the backup bytes to the backend in real time (the
// "real-time transfer" described in the backup design). Each target system gets
// its own directory and its own SQLite metadata store.
//
// Only the local backend is implemented for now. SFTP, HTTP and S3 backends are
// planned; Open returns ErrNotImplemented for those schemes.
package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/cloud-barista/ai-ha/pkg/backup/meta"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
)

// ErrNotImplemented is returned for backends that are defined but not yet built.
var ErrNotImplemented = errors.New("repository backend not implemented")

// DefaultLockDir holds the cross-process metadata lock files when no other
// directory is configured.
const DefaultLockDir = "/run/lock"

// options holds backend-specific settings supplied to Open.
type options struct {
	sftp    SFTPAuth
	smb     SMBAuth
	s3      S3Auth
	lockDir string
}

// Option configures Open for a specific backend.
type Option func(*options)

// WithSFTPAuth supplies the SSH credentials used by the SFTP backend.
func WithSFTPAuth(a SFTPAuth) Option {
	return func(o *options) { o.sftp = a }
}

// WithSMBAuth supplies the credentials used by the SMB backend.
func WithSMBAuth(a SMBAuth) Option {
	return func(o *options) { o.smb = a }
}

// WithS3Auth supplies the connection settings used by the S3 backend.
func WithS3Auth(a S3Auth) Option {
	return func(o *options) { o.s3 = a }
}

// WithLockDir sets the directory holding the cross-process metadata lock files
// used by the remote backends. Defaults to DefaultLockDir.
func WithLockDir(dir string) Option {
	return func(o *options) { o.lockDir = dir }
}

// Repository is a backup storage backend.
type Repository interface {
	// Scheme reports the backend type.
	Scheme() model.TransportScheme
	// EnsureSystem prepares the per-system directory/metadata location.
	EnsureSystem(ctx context.Context, system model.SystemInfo) error
	// NewWriter opens a streaming writer for a new backup file under the
	// system's directory. Closing it finalizes the transfer.
	NewWriter(ctx context.Context, system model.SystemInfo, filename string) (io.WriteCloser, error)
	// NewReader opens an existing backup file for restore.
	NewReader(ctx context.Context, system model.SystemInfo, filename string) (io.ReadCloser, error)
	// Meta opens a read-only view of the system's metadata. Changes made
	// through it are not persisted; use UpdateMeta to record them. The caller
	// closes it.
	Meta(ctx context.Context, system model.SystemInfo) (meta.Store, error)
	// UpdateMeta applies fn to the system's metadata and persists the result,
	// serialized against concurrent updates from other backup processes on the
	// same host. The stored metadata is left untouched when fn returns an error.
	UpdateMeta(ctx context.Context, system model.SystemInfo, fn func(context.Context, meta.Store) error) error
	// Close releases backend-level resources.
	Close() error
}

// Open constructs a Repository from a URI. Accepted forms:
//
//	/var/lib/aiha/repo          (local path)
//	file:///var/lib/aiha/repo   (local)
//	sftp://host/path            (sftp)
//	smb://host/share/path       (smb / cifs)
//	http(s)://host/path         (not yet implemented)
//	s3://bucket/prefix          (not yet implemented)
func Open(ctx context.Context, uri string, opts ...Option) (Repository, error) {
	var o options
	for _, fn := range opts {
		fn(&o)
	}

	scheme, location, err := parseURI(uri)
	if err != nil {
		return nil, err
	}
	switch scheme {
	case model.SchemeLocal:
		return newLocal(location)
	case model.SchemeSFTP:
		return newSFTP(ctx, location, o.sftp, o.lockDir)
	case model.SchemeSMB:
		return newSMB(ctx, location, o.smb, o.lockDir)
	case model.SchemeS3:
		return newS3(ctx, location, o.s3, o.lockDir)
	case model.SchemeHTTP:
		return nil, fmt.Errorf("%s repository: %w", scheme, ErrNotImplemented)
	default:
		return nil, fmt.Errorf("unsupported repository scheme %q", scheme)
	}
}

// parseURI splits a repository URI into a scheme and a backend-specific
// location. A bare path (no scheme) is treated as local.
func parseURI(uri string) (model.TransportScheme, string, error) {
	if uri == "" {
		return "", "", fmt.Errorf("repository URI is required")
	}
	if !strings.Contains(uri, "://") {
		return model.SchemeLocal, uri, nil
	}
	u, err := url.Parse(uri)
	if err != nil {
		return "", "", fmt.Errorf("parse repository URI: %w", err)
	}
	switch u.Scheme {
	case "file":
		return model.SchemeLocal, u.Path, nil
	case "sftp":
		return model.SchemeSFTP, uri, nil
	case "smb":
		return model.SchemeSMB, uri, nil
	case "http", "https":
		return model.SchemeHTTP, uri, nil
	case "s3":
		return model.SchemeS3, uri, nil
	default:
		return "", "", fmt.Errorf("unsupported repository scheme %q", u.Scheme)
	}
}
