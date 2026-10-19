//go:build linux

package source

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/rs/zerolog/log"
)

// PartitionSource backs up a Linux partition through a temporary read-only bind
// mount, following this sequence (see docs/backup.md):
//
//	mkdir /mnt/<name>-<randomID>
//	mount -B  <partition mount point> /mnt/<name>-<randomID>
//	mount -o remount,ro,bind          /mnt/<name>-<randomID>
//	# archive the temporary directory, then unmount + remove it
type PartitionSource struct {
	device string // e.g. /dev/sda1
	name   string // label used in the backup file name
	// agentStateDir is the agent's state directory, excluded from an OS root
	// backup so that credentials never travel inside an archive. Empty means
	// DefaultAgentStateDir.
	agentStateDir string
}

// NewPartition creates a partition source for the given device. When name is
// empty it is derived from the device (e.g. "/dev/sda1" -> "sda1",
// "/dev/mapper/rl-root" -> "rl-root").
func NewPartition(device, name string) (*PartitionSource, error) {
	if device == "" {
		return nil, fmt.Errorf("partition device is required")
	}
	if name == "" {
		name = labelFromDevice(device)
	}
	return &PartitionSource{device: device, name: name}, nil
}

// Name returns the partition label.
func (p *PartitionSource) Name() string { return p.name }

// Device returns the partition device path. It implements Describer.
func (p *PartitionSource) Device() string { return p.device }

// MountPoint returns where the partition is mounted, or "" if that cannot be
// determined. It implements Describer.
//
// A missing mount point is not reported as an error here: Prepare fails on it
// soon enough with a better message, and a describer that returned an error
// would make recording metadata harder than not recording it.
func (p *PartitionSource) MountPoint(context.Context) string {
	mountPoint, err := mountPointOf(p.device)
	if err != nil {
		return ""
	}
	return mountPoint
}

// Prepare resolves the partition's current mount point, bind mounts it
// read-only into a temporary directory and returns that directory.
func (p *PartitionSource) Prepare(ctx context.Context) (string, func() error, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}

	mountPoint, err := mountPointOf(p.device)
	if err != nil {
		return "", nil, err
	}

	// A distinctive dir under /mnt (e.g. /mnt/aiha-mount-sda1-<randomID>). The
	// prefix lets a root backup recognize and exclude our own mount dirs (see
	// Excludes).
	tmp := filepath.Join("/mnt", MountPrefix+fmt.Sprintf("%s-%s", p.name, model.NewID()))
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return "", nil, fmt.Errorf("create temp mount dir: %w", err)
	}

	log.Info().Str("device", p.device).Str("mountPoint", mountPoint).Str("tempDir", tmp).Msg("Bind mounting partition read-only")

	// Bind mount the partition's mount point into the temp directory.
	if err := syscall.Mount(mountPoint, tmp, "", syscall.MS_BIND, ""); err != nil {
		_ = os.Remove(tmp)
		return "", nil, fmt.Errorf("bind mount %s -> %s: %w", mountPoint, tmp, err)
	}
	// Remount the bind read-only.
	if err := syscall.Mount("", tmp, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
		_ = syscall.Unmount(tmp, 0)
		_ = os.Remove(tmp)
		return "", nil, fmt.Errorf("remount read-only %s: %w", tmp, err)
	}

	cleanup := func() error {
		if err := syscall.Unmount(tmp, 0); err != nil {
			return fmt.Errorf("unmount %s: %w", tmp, err)
		}
		return os.Remove(tmp)
	}
	return tmp, cleanup, nil
}

// labelFromDevice derives a file-name label from a device path.
func labelFromDevice(device string) string {
	return filepath.Base(strings.TrimSuffix(device, "/"))
}

// mountPointOf resolves the current mount point of a block device by scanning
// /proc/self/mountinfo. Matching is by the mount's device number, so a device
// still resolves when the mount table names it differently (e.g. the root
// filesystem listed as "/dev/root" on cloud images); the listed source name is
// matched as well, for filesystems whose mount reports an anonymous device
// number (btrfs and other multi-device filesystems).
func mountPointOf(device string) (string, error) {
	resolved := device
	if r, err := filepath.EvalSymlinks(device); err == nil {
		resolved = r
	}

	devNo := ""
	var st syscall.Stat_t
	if err := syscall.Stat(resolved, &st); err == nil && st.Mode&syscall.S_IFMT == syscall.S_IFBLK {
		devNo = fmt.Sprintf("%d:%d", majorOf(uint64(st.Rdev)), minorOf(uint64(st.Rdev)))
	}

	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", fmt.Errorf("read /proc/self/mountinfo: %w", err)
	}
	defer f.Close()

	mountPoint, err := findMountPoint(f, device, resolved, devNo)
	if err != nil {
		return "", err
	}
	if mountPoint == "" {
		return "", fmt.Errorf("device %s is not mounted", device)
	}
	return mountPoint, nil
}

// findMountPoint scans mountinfo-formatted lines for the mount of a device,
// given the device path as requested, with its symlinks resolved, and its
// "major:minor" device number (empty when it could not be determined). It
// returns an empty mount point when the device has no mount.
func findMountPoint(r io.Reader, device, resolved, devNo string) (string, error) {
	byDevNo := ""
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		// 36 35 98:0 / /mnt rw,noatime shared:1 - ext4 /dev/root rw
		// ^id ^parent ^major:minor ^fs root ^mount point   ^type ^source
		fields := strings.Fields(scanner.Text())
		sep := slices.Index(fields, "-")
		if sep < 5 || sep+2 >= len(fields) {
			continue
		}
		// Mount points in mountinfo are octal-escaped (e.g. \040 = space).
		mountPoint := unescapeMount(fields[4])

		src := fields[sep+2]
		if src == device {
			return mountPoint, nil
		}
		if r, err := filepath.EvalSymlinks(src); err == nil && r == resolved {
			return mountPoint, nil
		}
		// Remember the first mount of this device number, restricted to mounts
		// of the whole filesystem ("/") so a bind mount of a subdirectory is
		// never mistaken for the partition itself.
		if byDevNo == "" && devNo != "" && fields[2] == devNo && fields[3] == "/" {
			byDevNo = mountPoint
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return byDevNo, nil
}

// majorOf and minorOf decode a Linux dev_t into the major:minor pair used by
// /proc/self/mountinfo.
func majorOf(dev uint64) uint64 { return (dev>>8)&0xfff | (dev>>32)&^0xfff }

func minorOf(dev uint64) uint64 { return dev&0xff | (dev>>12)&^0xff }

// unescapeMount decodes the octal escapes used in mount table paths.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var r int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%o", &r); err == nil {
				b.WriteByte(byte(r))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
