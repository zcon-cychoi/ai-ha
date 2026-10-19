// Package state reads the agent's local state directory: the files that
// identify this installation to the framework and record an in-flight
// recovery. They live outside the repository because they describe the agent,
// not a backup.
//
// The directory is excluded from a root-partition backup (see
// source.PartitionSource.Excludes). A restored system must not come up holding
// the original system's identity, so nothing written here may travel inside a
// backup archive.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Dir is where the agent keeps its local state.
const Dir = "/var/lib/aiha"

// recoveryPendingFile records a restore whose boot has not been verified yet.
// The restore executor writes it onto the restored disk; the agent booting
// from that disk finds it and refuses to act as the original system.
const recoveryPendingFile = "recovery-pending.json"

// RecoveryPending describes a restored system awaiting boot verification.
//
// The system it describes booted from a backup image, so it carries a copy of
// the original system's identity and credentials. Until the framework has
// verified the boot and issued new credentials, this agent must not take work
// that belongs to the original system.
type RecoveryPending struct {
	// RecoveryID identifies the restore that produced this system.
	RecoveryID string `json:"recoveryId"`
	// RestoredAt is when the restore executor finished writing the disk.
	RestoredAt time.Time `json:"restoredAt"`
	// BootVerifyToken authenticates the boot verification report. It is a
	// secret: never log it. Reserved until the boot-verify endpoint exists.
	BootVerifyToken string `json:"bootVerifyToken,omitempty"`
}

// ReadRecoveryPending returns the pending recovery recorded under dir, or nil
// when the system is not awaiting boot verification. A dir of "" means Dir.
func ReadRecoveryPending(dir string) (*RecoveryPending, error) {
	if dir == "" {
		dir = Dir
	}
	path := filepath.Join(dir, recoveryPendingFile)

	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read recovery marker %s: %w", path, err)
	}

	var pending RecoveryPending
	if err := json.Unmarshal(b, &pending); err != nil {
		// A marker we cannot parse still means a recovery happened. Report the
		// error rather than treating the system as a normal one.
		return nil, fmt.Errorf("parse recovery marker %s: %w", path, err)
	}
	return &pending, nil
}

// WriteRecoveryPending records p under dir, replacing an earlier marker. The
// restore executor calls it with the state directory of the disk it restored,
// never of the system it runs on. dir is created if missing.
func WriteRecoveryPending(dir string, p RecoveryPending) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("encode recovery marker: %w", err)
	}
	tmp, err := writeTemp(dir, recoveryPendingFile, append(data, '\n'))
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // no-op once renamed

	path := filepath.Join(dir, recoveryPendingFile)
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write recovery marker %s: %w", path, err)
	}
	return nil
}
