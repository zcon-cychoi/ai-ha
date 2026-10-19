// Package model defines the shared request/response and record types used
// across the AI-HA backup framework (CLI and, later, the REST layer).
package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// BackupExtension is the file extension for AI-HA backup archives.
const BackupExtension = ".aha"

// CompressionType selects the archive compression codec. The set is designed
// to be extensible; only Zstd (default) and None are implemented for now.
type CompressionType string

const (
	CompressionZstd CompressionType = "zstd"
	CompressionGzip CompressionType = "gzip"
	CompressionNone CompressionType = "none"
)

// DefaultCompression is used when a request does not specify one.
const DefaultCompression = CompressionZstd

// TransportScheme identifies a backup storage / streaming backend. Only Local
// is implemented for now; SFTP/HTTP/S3 are defined for extensibility.
type TransportScheme string

const (
	SchemeLocal TransportScheme = "local"
	SchemeSFTP  TransportScheme = "sftp"
	SchemeSMB   TransportScheme = "smb"
	SchemeHTTP  TransportScheme = "http"
	SchemeS3    TransportScheme = "s3"
)

// BackupStatus is the lifecycle state of a backup record.
type BackupStatus string

const (
	BackupRunning   BackupStatus = "running"
	BackupCompleted BackupStatus = "completed"
	BackupFailed    BackupStatus = "failed"
)

// SystemInfo identifies a backup target system. One repository directory and
// one SQLite metadata file are kept per system.
type SystemInfo struct {
	// ID is the system's stable identifier, issued by the framework or, until
	// registration exists, generated once and kept by the agent. It is what
	// the repository files a system's backups under.
	ID string `json:"id"`
	// Hostname is for display. It is deliberately not the storage key: in a
	// cloud it is neither unique nor stable, since names like "ip-10-0-1-5"
	// are derived from an address and get reused.
	Hostname string `json:"hostname"`
}

// DirName returns the path component a repository files this system under.
//
// Backends must use this rather than picking a field, so that every backend
// agrees on where a system's data lives. It prefers the stable identifier and
// falls back to the hostname only for a SystemInfo that predates one.
func (s SystemInfo) DirName() string {
	if s.ID != "" {
		return s.ID
	}
	return s.Hostname
}

// BackupRecord is one entry in a system's metadata store.
type BackupRecord struct {
	ID          string          `json:"id"`
	Hostname    string          `json:"hostname"`
	Partition   string          `json:"partition"`
	FileName    string          `json:"fileName"`
	Compression CompressionType `json:"compression"`
	Scheme      TransportScheme `json:"scheme"`
	SizeBytes   int64           `json:"sizeBytes"`
	Status      BackupStatus    `json:"status"`
	Message     string          `json:"message,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
	CompletedAt *time.Time      `json:"completedAt,omitempty"`
}

// BackupRequest describes a Linux partition backup to perform.
type BackupRequest struct {
	// Device is the partition device path, e.g. "/dev/sda1".
	Device string `json:"device"`
	// PartitionName overrides the label used in the file name (e.g. "sda1",
	// "rl-root", "lv-data"). Derived from Device when empty.
	PartitionName string `json:"partitionName,omitempty"`
	// Compression selects the codec; DefaultCompression when empty.
	Compression CompressionType `json:"compression,omitempty"`
}

// RestoreRequest describes a restore of a previously created backup.
type RestoreRequest struct {
	// SystemID is the system whose backup is wanted: the directory the
	// repository files it under, not the source machine's hostname.
	SystemID    string          `json:"systemId"`
	FileName    string          `json:"fileName"`
	Compression CompressionType `json:"compression,omitempty"`
	// Dest is the target directory the archive is extracted into.
	Dest string `json:"dest"`
}

// NewID returns a short random hex identifier.
func NewID() string {
	b := make([]byte, 8)
	// crypto/rand.Read never returns a short read on supported platforms.
	if _, err := rand.Read(b); err != nil {
		// Fall back to a time-based value; collisions are acceptable for an ID.
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// BackupFileName builds a backup file name from the host, partition label and a
// random id, e.g. "web01-sda1-1a2b3c4d5e6f7081.aha".
func BackupFileName(hostname, partition string) string {
	return fmt.Sprintf("%s-%s-%s%s", hostname, partition, NewID(), BackupExtension)
}
