//go:build !rescue

package cmd

import (
	"fmt"

	"github.com/cloud-barista/ai-ha/pkg/backup/compress"
	"github.com/cloud-barista/ai-ha/pkg/backup/engine"
)

// This is the full agent: it protects a running system and can be driven by
// hand against a repository.
const (
	binaryName = "aiha-agent"
	rootShort  = "AI-HA agent"
	rootLong   = `aiha-agent carries out AI-HA work on an instance: it backs up Linux partitions
to a backup repository and, in a rescue environment booted for a restore,
claims the instance's restore job from the framework and restores it.

Backups are archived with tar, compressed (zstd by default) and streamed into
the repository. Backup metadata is kept in a per-system SQLite database inside
the repository.`
)

func init() {
	// Every manifest records the build that produced it, so that a capture
	// which turns out to be wrong can be traced back.
	engine.AgentVersion = binaryName + " " + version

	// Only this build links the codecs, so only it can report them.
	versionDetails = append(versionDetails, func() string {
		return fmt.Sprintf("compression codecs: %v", compress.Available())
	})
}
