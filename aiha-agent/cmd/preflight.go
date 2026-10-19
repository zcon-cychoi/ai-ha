//go:build !rescue

package cmd

import (
	"context"
	"fmt"

	"github.com/cloud-barista/ai-ha/pkg/backup/source"
	"github.com/cloud-barista/ai-ha/pkg/backup/sysinfo"
)

// checkDevices refuses a backup whose partitions are not what was asked for,
// before anything is read or written.
//
// A job is created from an inventory and run later, and a system can be
// repartitioned in between. Two things go wrong then, and neither announces
// itself:
//
//   - A device that is gone fails when the engine reaches it, which is after
//     the partitions ahead of it have already been uploaded. The set is marked
//     failed and the transfer is wasted.
//   - A device path that has been reused now names a different filesystem. That
//     produces no error at all: the backup succeeds, and it is a backup of
//     something else.
//
// The first is caught by asking whether each partition is mounted. The second
// needs the filesystem's identity, so it is checked only when the framework
// had an identity to compare against.
func checkDevices(ctx context.Context, sources []source.Source, expectedFSUUIDs map[string]string) error {
	for _, src := range sources {
		// Only a partition source has a device to check. Nothing else reaches
		// here today; skipping rather than failing keeps a future source that
		// is just a directory from being refused for having no device.
		d, ok := src.(source.Describer)
		if !ok {
			continue
		}
		if d.MountPoint(ctx) == "" {
			return fmt.Errorf(
				"%s is not mounted, so there is nothing to back up from it: "+
					"the system may have changed since this backup was asked for", d.Device())
		}
	}

	if len(expectedFSUUIDs) == 0 {
		return nil
	}
	return checkFilesystemIdentity(ctx, sources, expectedFSUUIDs)
}

// checkFilesystemIdentity refuses a device that no longer holds the filesystem
// the backup was asked for.
//
// It reads the system the same way the inventory did, so that what the
// framework validated against and what the agent re-checks are derived
// identically rather than by two rules that can drift apart.
func checkFilesystemIdentity(ctx context.Context, sources []source.Source, expected map[string]string) error {
	info, err := sysinfo.Collect(ctx, nil)
	if err != nil {
		return fmt.Errorf("read this system's configuration to verify the partitions: %w", err)
	}
	parts, err := sysinfo.Partitions(info, nil)
	if err != nil {
		return fmt.Errorf("list this system's partitions to verify them: %w", err)
	}

	current := make(map[string]string, len(parts))
	for _, p := range parts {
		if p.Device != "" && p.FSUUID != "" {
			current[p.Device] = p.FSUUID
		}
	}
	return compareFilesystems(sources, current, expected)
}

// compareFilesystems refuses a device whose filesystem is not the one the
// backup was asked for.
//
// A device whose identity could not be read is left alone. Failing a backup
// because the check itself could not run would trade a rare wrong capture for
// a common missing one, and the missing one is the worse of the two: a backup
// that did not happen is discovered at restore time just the same.
func compareFilesystems(sources []source.Source, current, expected map[string]string) error {
	for _, src := range sources {
		d, ok := src.(source.Describer)
		if !ok {
			continue
		}
		want, asked := expected[d.Device()]
		if !asked {
			continue
		}
		got, known := current[d.Device()]
		if !known {
			continue
		}
		if got != want {
			return fmt.Errorf(
				"%s holds filesystem %s, but this backup was asked for %s: "+
					"the partition has been replaced since the job was created", d.Device(), got, want)
		}
	}
	return nil
}
