package sysinfo

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// This file reduces collected system facts to the partitions that make up the
// system, so that a backup can be asked for without naming them by hand.
//
// Two things make that harder than reading a list of mounts:
//
//   - The mount table's source name is not always a usable device path. A
//     cloud image reports its root filesystem as "/dev/root", which cannot be
//     handed back as a device to back up. The device number is the reliable
//     identity, so the canonical path is resolved from that.
//   - The mount table only knows what is mounted. A separate /boot or ESP that
//     happens to be unmounted is simply absent, and a backup chosen from that
//     list would restore to a system that does not boot. Completeness is
//     therefore checked against fstab and reported alongside.

// ErrNoMountTable reports that the mount table could not be read at all.
//
// It is deliberately distinct from finding partitions and judging them
// incomplete. A system whose mount table is unreadable must not be reported as
// an inventory: an empty list would overwrite a good one and look just as
// fresh.
var ErrNoMountTable = errors.New("the mount table could not be read")

// backupableFSTypes are the filesystems this agent captures as a partition
// backup. The year-one scenario is Ubuntu 24.04 on a single disk, which means
// ext4 and vfat; xfs archives the same way and costs nothing to allow.
//
// btrfs is deliberately absent. A partition source bind mounts the mount
// point, which on btrfs captures only the mounted subvolume, and subvolume
// handling is out of scope for year one (roadmap.md 3.2). btrfs also reports
// an anonymous device number, which the resolution below cannot turn into a
// device path.
var backupableFSTypes = map[string]bool{
	"ext4": true,
	"xfs":  true,
	"vfat": true,
}

// Partition is one mounted filesystem of the system, as an inventory reports it.
type Partition struct {
	// Device is the canonical device path, e.g. "/dev/nvme0n1p1". It is
	// resolved from the device number, never taken from the mount table's
	// source name, which can be an alias.
	Device string `json:"device,omitempty"`
	// MountPoint is where the filesystem was mounted, e.g. "/boot/efi".
	MountPoint string `json:"mountPoint"`
	FSType     string `json:"fsType,omitempty"`
	FSUUID     string `json:"fsUuid,omitempty"`
	PartUUID   string `json:"partUuid,omitempty"`
	Label      string `json:"label,omitempty"`
	// DevNo is the "major:minor" device number of the mount.
	DevNo string `json:"devNo,omitempty"`

	// FSSizeBytes and FSUsedBytes are the filesystem's size and usage, not the
	// device's. A nil pointer means the value could not be determined, which
	// is not the same as a filesystem that is empty.
	FSSizeBytes *int64 `json:"fsSizeBytes,omitempty"`
	FSUsedBytes *int64 `json:"fsUsedBytes,omitempty"`

	// Candidate says this partition is included when backup targets are chosen
	// automatically.
	Candidate bool `json:"candidate"`
	// Selectable says this partition may be named by hand. The two differ: a
	// partition can be selectable without being a candidate, while one that is
	// not selectable cannot be captured by this agent at all and must be
	// refused even when an operator asks for it by name.
	Selectable bool `json:"selectable"`
	// Reason explains a false in either flag, in an operator's terms.
	Reason string `json:"reason,omitempty"`
}

// DeviceResolver turns a "major:minor" device number into a canonical device
// path.
type DeviceResolver interface {
	Resolve(devNo string) (string, error)
}

// SysfsResolver resolves device numbers through sysfs, which names every block
// device the kernel knows under its own number.
//
// Reading beats shelling out here for the same reasons as elsewhere in this
// package: it is faster, it cannot fail on a missing tool, and it keeps
// os/exec confined to runner.go.
type SysfsResolver struct {
	// Root is the sysfs directory holding block devices by number. Empty means
	// /sys/dev/block. Tests point it at a directory of their own.
	Root string
}

// Resolve reads the device's name from sysfs.
//
// A device-mapper device is named by its mapper path rather than by its kernel
// node. The kernel calls an LVM volume "dm-0", whose minor number is assigned
// in activation order and so need not name the same volume after a reboot;
// /dev/mapper/<vg>-<lv> is stable, and is the name an operator types.
func (r SysfsResolver) Resolve(devNo string) (string, error) {
	root := r.Root
	if root == "" {
		root = "/sys/dev/block"
	}
	// The device number comes from the mount table. Rejecting anything that is
	// not two numbers keeps it from reaching into the filesystem as a path.
	if !validDevNo(devNo) {
		return "", fmt.Errorf("%q is not a device number", devNo)
	}

	// Only a device-mapper device has this file, so its absence is the test
	// for "not device-mapper" and falls through to the ordinary path.
	if device, err := mapperName(filepath.Join(root, devNo, "dm", "name")); err == nil {
		return device, nil
	}

	data, err := os.ReadFile(filepath.Join(root, devNo, "uevent"))
	if err != nil {
		return "", fmt.Errorf("read the device name for %s: %w", devNo, err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		name, ok := strings.CutPrefix(strings.TrimSpace(line), "DEVNAME=")
		if !ok || name == "" {
			continue
		}
		// DEVNAME is relative to /dev, and may hold a subdirectory
		// ("mapper/vg-root").
		return filepath.Join("/dev", name), nil
	}
	return "", fmt.Errorf("sysfs names no device for %s", devNo)
}

// mapperName reads a device-mapper device's name from the sysfs file that
// holds it and returns the path udev gives it under /dev/mapper.
//
// The name becomes a path element, so it is held to the same standard as the
// device number: anything that could reach outside /dev/mapper is refused
// rather than resolved. A refusal costs the caller a friendly device name and
// it falls back to the kernel node; resolving it could name a file that is not
// a device at all.
func mapperName(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	name := strings.TrimSpace(string(b))
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return "", fmt.Errorf("%q is not a device-mapper name", name)
	}
	return filepath.Join("/dev/mapper", name), nil
}

// validDevNo reports whether s has the "major:minor" shape.
func validDevNo(s string) bool {
	major, minor, ok := strings.Cut(s, ":")
	if !ok {
		return false
	}
	for _, part := range []string{major, minor} {
		if part == "" {
			return false
		}
		if _, err := strconv.ParseUint(part, 10, 32); err != nil {
			return false
		}
	}
	return true
}

// Partitions reduces collected system facts to the mounted local filesystems
// of this system.
//
// Every mount that names a block device is listed, including ones that cannot
// be backed up: an operator looking at the inventory needs to see why a
// partition was left out, and a device that is missing reads very differently
// from one that is present and explained.
//
// resolve may be nil, in which case SysfsResolver is used.
func Partitions(info *SystemInfo, resolve DeviceResolver) ([]Partition, error) {
	if info == nil {
		return nil, ErrNoMountTable
	}
	// A running Linux system always has mounts. None means the table could not
	// be read, since Collect records that failure and carries on.
	if len(info.Mounts) == 0 {
		return nil, ErrNoMountTable
	}
	if resolve == nil {
		resolve = SysfsResolver{}
	}

	enrich := blockDeviceFacts(info.BlockDevices)

	// One filesystem can be mounted more than once. The representative mount
	// is the one of the whole filesystem ("/") at the shortest mount point;
	// the others are bind mounts of a filesystem already described, and are
	// dropped below. pkg/backup/source applies the same rule when it resolves
	// a device's mount point.
	primary := make(map[string]int) // devNo -> index into out
	var out []Partition

	for _, m := range info.Mounts {
		if !strings.HasPrefix(m.Source, "/dev/") {
			continue
		}

		p := Partition{
			MountPoint: m.MountPoint,
			FSType:     m.FSType,
			DevNo:      m.DevNo,
		}
		if device, err := resolve.Resolve(m.DevNo); err == nil {
			p.Device = device
		}
		if facts, ok := enrich[m.DevNo]; ok {
			p.FSUUID = facts.fsUUID
			p.PartUUID = facts.partUUID
			p.Label = facts.label
			p.FSSizeBytes = facts.fsSize
			p.FSUsedBytes = facts.fsUsed
			if p.Device == "" {
				p.Device = facts.path
			}
		}

		index := len(out)
		out = append(out, p)

		if m.Root != "/" || m.DevNo == "" {
			continue
		}
		if first, seen := primary[m.DevNo]; !seen || len(m.MountPoint) < len(out[first].MountPoint) {
			primary[m.DevNo] = index
		}
	}

	// An inventory is a list of partitions, so one partition appears once. A
	// second mount of a filesystem already described is not another thing to
	// back up, and listing it invites an operator to look for a device that is
	// not there: an agent running in a mount namespace of its own reports its
	// sandbox's bind mounts, which exist nowhere on the host.
	//
	// Only a duplicate is dropped — an entry whose filesystem is represented
	// elsewhere in the list. A mount that is nobody's representative, because
	// its device number could not be read or because the filesystem is mounted
	// as a subtree and nowhere in full, stays and is explained by judge. That
	// is a partition this agent cannot offer, not one it has already offered.
	kept := out[:0]
	for i := range out {
		first, represented := primary[out[i].DevNo]
		if represented && first != i {
			continue
		}
		judge(&out[i], represented)
		kept = append(kept, out[i])
	}
	return kept, nil
}

// judge fills in a partition's flags and the reason behind them. isPrimary says
// this is the representative mount of its filesystem.
func judge(p *Partition, isPrimary bool) {
	switch {
	case strings.HasPrefix(filepath.Base(p.MountPoint), agentMountPrefix):
		p.Reason = "a temporary mount this agent makes while backing up"
	case p.Device == "":
		// Listed rather than dropped: a partition whose path could not be read
		// is not the same as one that is not there. This is judged before the
		// mount is called a bind mount, because a mount whose device number
		// could not be read is never anyone's representative and would
		// otherwise be explained as a duplicate of a filesystem that is not in
		// the list at all.
		p.Reason = "the device path could not be resolved"
	case !isPrimary:
		// A duplicate mount of a filesystem that is in the list has already
		// been dropped, so what reaches here is a bind mount of a subtree
		// whose filesystem is mounted nowhere in full. The wording holds for
		// both, and claims nothing about a mount the list may not contain.
		p.Reason = "a bind mount rather than a filesystem of its own"
	case p.FSType == "btrfs":
		p.Reason = "btrfs subvolumes are not captured yet"
	case !backupableFSTypes[p.FSType]:
		p.Reason = fmt.Sprintf("%s is not a filesystem this agent captures", p.FSType)
	default:
		p.Candidate = true
		p.Selectable = true
	}
}

// agentMountPrefix mirrors source.MountPrefix, the temporary bind mounts a
// backup makes under /mnt.
//
// It is repeated rather than imported to keep the direction of dependency
// right: this package reads what a system is, and pkg/backup/source acts on
// it. A test holds the two values equal, so the copy cannot drift into
// silently listing the agent's own mounts as partitions to back up.
const agentMountPrefix = "aiha-mount-"

// deviceFacts is what lsblk adds to a mount: the identifiers and sizes that
// the mount table does not carry.
type deviceFacts struct {
	path     string
	fsUUID   string
	partUUID string
	label    string
	fsSize   *int64
	fsUsed   *int64
}

// blockDeviceFacts indexes "lsblk -J -O --bytes" output by device number.
//
// lsblk is an enrichment, never the list itself. Its column set changes
// between releases — MOUNTPOINT and MOUNTPOINTS are different columns, not a
// rename — so a partition list built on it would go empty on a version that
// spells something differently. Missing enrichment costs a UUID and a size;
// a missing partition costs a system.
func blockDeviceFacts(raw json.RawMessage) map[string]deviceFacts {
	if len(raw) == 0 {
		return nil
	}
	var doc struct {
		BlockDevices []lsblkDevice `json:"blockdevices"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}

	out := make(map[string]deviceFacts)
	var walk func(devices []lsblkDevice)
	walk = func(devices []lsblkDevice) {
		for _, d := range devices {
			if d.MajMin != "" {
				path := d.Path
				if path == "" && d.Name != "" {
					path = filepath.Join("/dev", d.Name)
				}
				out[d.MajMin] = deviceFacts{
					path:     path,
					fsUUID:   d.UUID,
					partUUID: d.PartUUID,
					label:    d.Label,
					fsSize:   d.FSSize.value(),
					fsUsed:   d.FSUsed.value(),
				}
			}
			walk(d.Children)
		}
	}
	walk(doc.BlockDevices)
	return out
}

// lsblkDevice is the handful of lsblk fields an inventory uses. Everything else
// lsblk reports is kept verbatim in SystemInfo.BlockDevices.
type lsblkDevice struct {
	Name     string        `json:"name"`
	Path     string        `json:"path"`
	MajMin   string        `json:"maj:min"`
	UUID     string        `json:"uuid"`
	PartUUID string        `json:"partuuid"`
	Label    string        `json:"label"`
	FSSize   lsblkNumber   `json:"fssize"`
	FSUsed   lsblkNumber   `json:"fsused"`
	Children []lsblkDevice `json:"children"`
}

// lsblkNumber reads a size that lsblk may report as a number, as a string or
// as null. "--bytes" makes current lsblk emit numbers, but the same field was
// a string in older releases, and an unreadable size must come back as absent
// rather than as zero.
type lsblkNumber struct {
	n     int64
	known bool
}

func (v *lsblkNumber) UnmarshalJSON(data []byte) error {
	text := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if text == "" || text == "null" {
		return nil
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		// A size this package cannot read is reported as unknown. It is not
		// worth failing an inventory over.
		return nil
	}
	v.n, v.known = n, true
	return nil
}

func (v lsblkNumber) value() *int64 {
	if !v.known {
		return nil
	}
	n := v.n
	return &n
}
