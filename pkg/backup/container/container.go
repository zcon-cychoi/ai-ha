// Package container will provide container-workload backup using Velero.
//
// It is a placeholder for now: the interface fixes the shape the CLI/engine
// will use, while the Velero-backed implementation is future work (see
// docs/backup.md, "컨테이너 백업").
package container

import (
	"context"
	"errors"
)

// ErrNotImplemented is returned until the Velero backend is built.
var ErrNotImplemented = errors.New("container backup via velero is not implemented yet")

// Backuper backs up and restores container workloads (e.g. Kubernetes
// namespaces) via Velero.
type Backuper interface {
	// Backup triggers a backup of the given namespace.
	Backup(ctx context.Context, namespace string) error
	// Restore restores a previously created backup by name.
	Restore(ctx context.Context, backupName string) error
}
