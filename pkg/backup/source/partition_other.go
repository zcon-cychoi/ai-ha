//go:build !linux

package source

import (
	"context"
	"fmt"
)

// PartitionSource is unavailable on non-Linux platforms; the read-only bind
// mount workflow relies on Linux mount semantics.
type PartitionSource struct {
	name          string
	agentStateDir string
}

// NewPartition returns an error on non-Linux platforms.
func NewPartition(device, name string) (*PartitionSource, error) {
	return nil, fmt.Errorf("partition backup is only supported on linux")
}

// Name returns the partition label.
func (p *PartitionSource) Name() string { return p.name }

// Prepare always errors on non-Linux platforms.
func (p *PartitionSource) Prepare(ctx context.Context) (string, func() error, error) {
	return "", nil, fmt.Errorf("partition backup is only supported on linux")
}
