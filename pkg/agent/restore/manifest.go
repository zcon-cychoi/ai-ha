package restore

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	backupmodel "github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/sysinfo"
)

// restorableFSTypes are the filesystems the executor recreates. They match
// what the agent captures.
var restorableFSTypes = map[string]bool{"ext4": true, "xfs": true, "vfat": true}

// decompressors are the commands that undo each codec, reading the archive on
// standard input. A codec without one is stored uncompressed.
var decompressors = map[backupmodel.CompressionType][]string{
	backupmodel.CompressionZstd: {"zstd", "-d", "-c"},
	backupmodel.CompressionGzip: {"gzip", "-d", "-c"},
	backupmodel.CompressionNone: nil,
}

// checkManifest verifies that m describes the set the plan asked for, in a
// form this executor understands, and that every partition in it was captured.
// A partial set would restore a system missing a partition it needs to boot.
func checkManifest(m backupmodel.Manifest, plan model.RestorePlan) error {
	switch {
	case m.Version != backupmodel.ManifestVersion:
		return fmt.Errorf("manifest version %d is not supported (want %d)", m.Version, backupmodel.ManifestVersion)
	case m.Set.ID != plan.SourceBackupSetID:
		return fmt.Errorf("manifest describes set %q, not %q", m.Set.ID, plan.SourceBackupSetID)
	case m.Set.SystemID != plan.SourceSystemID:
		return fmt.Errorf("manifest belongs to system %q, not %q", m.Set.SystemID, plan.SourceSystemID)
	case m.Set.Status != backupmodel.BackupCompleted:
		return fmt.Errorf("backup set %s did not complete (status %s)", m.Set.ID, m.Set.Status)
	case len(m.Set.Items) == 0:
		return fmt.Errorf("backup set %s holds no partitions", m.Set.ID)
	case len(m.System) == 0:
		return fmt.Errorf("manifest of set %s carries no system information", m.Set.ID)
	}
	for _, item := range m.Set.Items {
		if item.Status != backupmodel.BackupCompleted {
			return fmt.Errorf("partition %s in set %s did not complete (status %s)", item.Partition, m.Set.ID, item.Status)
		}
		// The file name becomes part of a repository path.
		if item.FileName == "" || item.FileName != path.Base(item.FileName) || strings.HasPrefix(item.FileName, ".") {
			return fmt.Errorf("partition %s has an unusable file name %q", item.Partition, item.FileName)
		}
		if _, ok := decompressors[item.Compression]; !ok {
			return fmt.Errorf("partition %s uses unsupported compression %q", item.Partition, item.Compression)
		}
	}
	return nil
}

// target is one partition to restore and where it goes on this machine.
type target struct {
	item backupmodel.BackupItem
	// partUUID identifies the source partition. It is how the partition is
	// found here: device names follow the order disks are detected in, and an
	// instance with one more volume names its root disk differently, while a
	// partition table made from the same image — or restored from the source's
	// sfdisk dump — carries the same PARTUUIDs.
	partUUID string
	// device is the partition on this machine with partUUID, and mountDir
	// where "mountall --no-bind" mounts it: /mnt/<name>. Both are set once the
	// partition has been found (see resolveDevices).
	device   string
	mountDir string
	fsType   string
	label    string
	// uuid is the source filesystem's UUID. The filesystem is recreated with
	// it, so that everything in the restored system naming it still matches.
	uuid string
}

var (
	// standardUUIDPattern is the UUID form ext4 and xfs use.
	standardUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// vfatVolumeIDPattern is the volume ID a vfat filesystem has instead.
	vfatVolumeIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{4}-[0-9a-fA-F]{4}$`)
)

// validFSUUID reports whether uuid is a UUID mkfs.<fsType> can be given.
func validFSUUID(fsType, uuid string) bool {
	if fsType == "vfat" {
		return vfatVolumeIDPattern.MatchString(uuid)
	}
	return standardUUIDPattern.MatchString(uuid)
}

// noResolver leaves device paths to lsblk's report in the manifest. The
// device numbers in a manifest are the source's; resolving them against this
// machine's sysfs would name whatever happens to have that number here.
type noResolver struct{}

func (noResolver) Resolve(string) (string, error) {
	return "", errors.New("device numbers are not resolved on the restore target")
}

// planTargets matches each item of the set with the filesystem facts the
// manifest recorded for its mount point, and checks that the set is one this
// executor can lay out: a root, one item per device and per mount point, and
// filesystems it knows how to create.
func planTargets(set backupmodel.BackupSet, info *sysinfo.SystemInfo, mntDir string) ([]*target, error) {
	parts, err := sysinfo.Partitions(info, noResolver{})
	if err != nil {
		return nil, fmt.Errorf("read the source's partitions from the manifest: %w", err)
	}
	byMount := make(map[string]sysinfo.Partition, len(parts))
	for _, p := range parts {
		if _, dup := byMount[p.MountPoint]; !dup {
			byMount[p.MountPoint] = p
		}
	}

	var targets []*target
	partUUIDs, mounts := map[string]bool{}, map[string]bool{}
	hasRoot := false
	for _, item := range set.Items {
		if item.MountPoint == "" {
			return nil, fmt.Errorf("partition %s lacks the mount point a restore needs", item.Partition)
		}
		if mounts[item.MountPoint] {
			return nil, fmt.Errorf("partition %s repeats a mount point already in the set", item.Partition)
		}
		mounts[item.MountPoint] = true

		p, ok := byMount[item.MountPoint]
		if !ok {
			return nil, fmt.Errorf("the manifest has no filesystem facts for %s", item.MountPoint)
		}
		partUUID := strings.ToLower(p.PartUUID)
		if partUUID == "" {
			return nil, fmt.Errorf("the manifest records no PARTUUID for %s, so its partition cannot be found here", item.MountPoint)
		}
		if partUUIDs[partUUID] {
			return nil, fmt.Errorf("partition %s repeats a PARTUUID already in the set", item.Partition)
		}
		partUUIDs[partUUID] = true
		if !restorableFSTypes[p.FSType] {
			return nil, fmt.Errorf("%s is %q, which the restore cannot recreate", item.MountPoint, p.FSType)
		}
		if !validFSUUID(p.FSType, p.FSUUID) {
			return nil, fmt.Errorf("the manifest records no usable %s UUID for %s (got %q)", p.FSType, item.MountPoint, p.FSUUID)
		}
		if item.MountPoint == "/" {
			hasRoot = true
		}
		targets = append(targets, &target{
			item:     item,
			partUUID: partUUID,
			fsType:   p.FSType,
			label:    p.Label,
			uuid:     p.FSUUID,
		})
	}
	if !hasRoot {
		return nil, errors.New("the backup set has no root (/) partition")
	}
	// Shallower mount points first, so that a parent is in place before what
	// is mounted inside it.
	slices.SortStableFunc(targets, func(a, b *target) int {
		return depth(a.item.MountPoint) - depth(b.item.MountPoint)
	})
	return targets, nil
}

// resolveDevices finds each target's partition on this machine by PARTUUID.
// listing is "lsblk -r -n -o PATH,PARTUUID": one device per line, the PARTUUID
// missing for a device that has none. A PARTUUID found on no partition, or on
// more than one — as when the source's disk is attached too — is refused: the
// partition to format must be beyond doubt.
func resolveDevices(targets []*target, listing, mntDir string) error {
	found := map[string][]string{}
	for line := range strings.Lines(listing) {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		id := strings.ToLower(fields[1])
		found[id] = append(found[id], fields[0])
	}
	for _, tg := range targets {
		paths := found[tg.partUUID]
		switch len(paths) {
		case 0:
			return fmt.Errorf("no partition on this machine has PARTUUID %s (%s on the source, at %s)",
				tg.partUUID, tg.item.Device, tg.item.MountPoint)
		case 1:
			tg.device = paths[0]
			tg.mountDir = filepath.Join(mntDir, filepath.Base(paths[0]))
		default:
			return fmt.Errorf("more than one partition has PARTUUID %s: %s", tg.partUUID, strings.Join(paths, ", "))
		}
	}
	return nil
}

// depth is how many directories deep a mount point is; "/" is 0.
func depth(mountPoint string) int {
	if mountPoint == "/" {
		return 0
	}
	return strings.Count(mountPoint, "/")
}

// decodeSystem reads the system facts a manifest carries.
func decodeSystem(raw json.RawMessage) (*sysinfo.SystemInfo, error) {
	var info sysinfo.SystemInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("decode the manifest's system information: %w", err)
	}
	return &info, nil
}
