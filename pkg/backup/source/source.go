// Package source provides backup sources. A Source prepares the filesystem
// tree that should be archived and returns a cleanup function.
//
// The current implementation is the Linux partition source, which performs a
// read-only bind mount of a partition before archiving so that the on-disk
// state is not modified while the backup runs.
package source

import "context"

// Describer is an optional Source capability: a source that can say where in
// the system it came from. A restore matches a captured item against the
// target by mount point, so a source that knows its own has to report it.
//
// Like ExcludeProvider, it is an assertion rather than part of Source: a
// source that is just a directory has no device or mount point to give.
type Describer interface {
	// Device is the source device path, e.g. "/dev/sda1".
	Device() string
	// MountPoint is where the device was mounted when the backup ran, e.g.
	// "/boot/efi". It is empty if that could not be determined.
	MountPoint(ctx context.Context) string
}

// ExcludedPathProvider is an optional Source capability: a source that can
// name, in absolute form, the paths it deliberately leaves out.
//
// A restore has to be able to tell a deliberate omission from damage, so the
// manifest records these. They come from the source rather than from a
// constant because what is excluded depends on how the agent is configured.
type ExcludedPathProvider interface {
	ExcludedPaths() []string
}

// Source prepares a directory tree to be archived.
type Source interface {
	// Name returns the partition label used in the backup file name
	// (e.g. "sda1", "rl-root", "lv-data").
	Name() string
	// Prepare makes the source available and returns the root path to
	// archive together with a cleanup function that must be called by the
	// caller (typically via defer) once archiving completes.
	Prepare(ctx context.Context) (root string, cleanup func() error, err error)
}
