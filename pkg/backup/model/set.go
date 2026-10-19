package model

import (
	"encoding/json"
	"time"
)

// ManifestFileName is the manifest inside a backup set's directory. It is the
// source of truth a restore reads: the repository alone is enough to restore
// from, without the metadata index and without the framework.
const ManifestFileName = "manifest.json"

// ManifestVersion is the schema version of a written manifest. A restore
// checks it before trusting the rest, so that an older agent's archive is
// refused rather than half-understood.
const ManifestVersion = 1

// BackupSet is one machine captured at one point in time: the partitions that
// have to be restored together for the system to boot.
//
// Individual BackupRecords cannot express this. A machine whose /boot and /
// live on different partitions is only restorable if both are known to belong
// to the same capture, and nothing in a per-partition record says so.
type BackupSet struct {
	ID       string `json:"id"`
	SystemID string `json:"systemId"`
	// Hostname is for display, as on SystemInfo.
	Hostname string `json:"hostname,omitempty"`

	Items []BackupItem `json:"items"`

	Status      BackupStatus `json:"status"`
	Message     string       `json:"message,omitempty"`
	CreatedAt   time.Time    `json:"createdAt"`
	CompletedAt *time.Time   `json:"completedAt,omitempty"`
}

// BackupItem is one partition within a set.
type BackupItem struct {
	ID string `json:"id"`
	// Partition is the label used in the file name, e.g. "sda1".
	Partition string `json:"partition"`
	// Device is the source device path, e.g. "/dev/sda1". Recorded because a
	// restore has to match this capture against the target's devices, and the
	// path alone is often the only thing tying an item to the system layout.
	Device string `json:"device,omitempty"`
	// MountPoint is where the partition was mounted, e.g. "/boot/efi". This is
	// what a restore matches on: the target's layout is reproduced by mount
	// point, not by reproducing the source's partition table.
	MountPoint string `json:"mountPoint,omitempty"`

	FileName    string          `json:"fileName"`
	Compression CompressionType `json:"compression"`
	SizeBytes   int64           `json:"sizeBytes"`

	Status      BackupStatus `json:"status"`
	Message     string       `json:"message,omitempty"`
	CreatedAt   time.Time    `json:"createdAt"`
	CompletedAt *time.Time   `json:"completedAt,omitempty"`
}

// Manifest is what is written into a backup set's directory. It carries
// everything a restore needs about this capture, so that the repository is
// self-sufficient: losing the metadata index costs a fast listing, not the
// ability to restore.
type Manifest struct {
	// Version is ManifestVersion at the time of writing.
	Version int `json:"version"`
	// AgentVersion records which build produced the set, for diagnosing a
	// capture that turns out to be wrong.
	AgentVersion string `json:"agentVersion,omitempty"`

	Set BackupSet `json:"set"`

	// System is the system's facts at capture time, as collected by
	// pkg/backup/sysinfo. It is held as raw JSON so that this package does not
	// depend on that one and so that a field added there needs no change here.
	System json.RawMessage `json:"system,omitempty"`

	// ExcludedPaths names archive-relative paths deliberately left out of every
	// item, so a restore knows their absence is intended rather than damage.
	// The agent's own credentials are excluded this way.
	ExcludedPaths []string `json:"excludedPaths,omitempty"`
}

// ManifestName returns the manifest file name for a set.
//
// The manifest sits beside the set's archives in the system's directory rather
// than in a directory of its own: every repository backend refuses a path
// separator in a file name, which is what keeps a name from escaping the
// system's directory. The set id in the name carries the grouping instead, and
// the manifest itself lists the archives that belong to it.
func ManifestName(setID string) string {
	return setID + "-" + ManifestFileName
}
