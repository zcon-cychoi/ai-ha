// Package sysinfo collects the system facts a restore needs in order to make a
// backup bootable again: what the disks looked like, which filesystem was
// mounted where, how the system booted and how it was addressed on the network.
//
// None of this is derivable from the archive contents, and it cannot be
// collected after the fact — by the time a restore runs, the source system may
// be gone. Collection therefore happens with every backup, and it never fails
// the backup: an item that cannot be read is recorded in Errors and the rest
// proceeds.
//
// Facts are kept close to the form the system reported them in. Output that a
// restore only has to hand back to a tool (a partition table dump, a netplan
// file) is stored verbatim rather than modelled, so that nothing is lost to a
// parser that did not anticipate a field.
package sysinfo

import (
	"encoding/json"
	"time"
)

// Firmware is how the system was booted.
type Firmware string

const (
	// FirmwareUEFI means /sys/firmware/efi was present.
	FirmwareUEFI Firmware = "uefi"
	// FirmwareBIOS means it was not.
	FirmwareBIOS Firmware = "bios"
)

// SystemInfo is everything collected for one system at one point in time.
type SystemInfo struct {
	CollectedAt time.Time `json:"collectedAt"`

	// Kernel is what uname reports.
	Kernel Kernel `json:"kernel"`
	// OSRelease is /etc/os-release, both parsed and verbatim.
	OSRelease OSRelease `json:"osRelease"`
	// Firmware distinguishes a UEFI system from a BIOS one. A restore has to
	// install the bootloader the same way the source booted.
	Firmware Firmware `json:"firmware"`

	// BlockDevices is "lsblk -J -O" verbatim: every device with its UUID,
	// PARTUUID, filesystem type, size and mount point. Kept as reported
	// because lsblk's column set grows between releases.
	BlockDevices json.RawMessage `json:"blockDevices,omitempty"`
	// PartitionTables maps a disk device path to its "sfdisk -d" dump, the
	// input format sfdisk itself accepts to recreate the table.
	PartitionTables map[string]string `json:"partitionTables,omitempty"`

	// Mounts is the mount table at collection time, from /proc/self/mountinfo.
	// This is what a restore matches against the target's layout.
	Mounts []Mount `json:"mounts,omitempty"`
	// Fstab is /etc/fstab, both parsed and verbatim. A restore rewrites it to
	// match the target's identifiers.
	Fstab Fstab `json:"fstab"`

	// NetplanFiles maps a file name under /etc/netplan to its contents.
	//
	// These may hold secrets (a wireless passphrase, for example). They are
	// stored because a restore needs them, and they are never logged.
	NetplanFiles map[string]string `json:"netplanFiles,omitempty"`

	// LVM is present only on a system that has LVM and the tools to report it.
	LVM *LVM `json:"lvm,omitempty"`

	// Errors records the items that could not be collected. A non-empty
	// Errors does not mean the backup failed.
	Errors []CollectError `json:"errors,omitempty"`
}

// Kernel is the running kernel, from uname.
type Kernel struct {
	// Release is "uname -r", e.g. "6.8.0-45-generic". A restore rebuilds the
	// initramfs for the kernels it finds, and this says which one ran.
	Release string `json:"release"`
	Version string `json:"version,omitempty"`
	Machine string `json:"machine,omitempty"`
	Sysname string `json:"sysname,omitempty"`
}

// OSRelease is the contents of /etc/os-release.
type OSRelease struct {
	// ID and VersionID are the fields a restore branches on, e.g. "ubuntu" and
	// "24.04". Fields holds every key, Raw the file as read.
	ID        string            `json:"id,omitempty"`
	VersionID string            `json:"versionId,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
	Raw       string            `json:"raw,omitempty"`
}

// Mount is one line of /proc/self/mountinfo, reduced to what a restore uses.
type Mount struct {
	// MountPoint is where the filesystem was mounted, e.g. "/boot/efi".
	MountPoint string `json:"mountPoint"`
	// Source is the device or pseudo-source, e.g. "/dev/nvme0n1p1".
	Source string `json:"source"`
	// FSType is the filesystem type, e.g. "ext4", "vfat".
	FSType string `json:"fsType"`
	// Options are the per-mount options, e.g. "rw,relatime".
	Options string `json:"options,omitempty"`
	// Root is the subtree of the filesystem that is mounted, usually "/".
	Root string `json:"root,omitempty"`
	// DevNo is the mount's "major:minor" device number. It is what identifies
	// the filesystem: the source name in the mount table can be an alias
	// ("/dev/root" on cloud images), and two mounts of one filesystem share
	// this number where their source strings may not.
	DevNo string `json:"devNo,omitempty"`
}

// Fstab is /etc/fstab.
type Fstab struct {
	Entries []FstabEntry `json:"entries,omitempty"`
	Raw     string       `json:"raw,omitempty"`
}

// FstabEntry is one non-comment line of /etc/fstab.
type FstabEntry struct {
	// Spec is the first field: a device path, "UUID=...", "LABEL=..." and so
	// on. A restore rewrites it to the identifier the target actually has.
	Spec       string `json:"spec"`
	MountPoint string `json:"mountPoint"`
	FSType     string `json:"fsType"`
	Options    string `json:"options"`
	Dump       string `json:"dump,omitempty"`
	Pass       string `json:"pass,omitempty"`
}

// LVM is the LVM configuration, collected only where LVM is in use. The
// year-one scenario has no LVM, so its absence is normal and not an error.
type LVM struct {
	// FullReport is "lvm fullreport --reportformat json" verbatim.
	FullReport json.RawMessage `json:"fullReport,omitempty"`
	// VGConfigs maps a volume group name to its "vgcfgbackup" text, the form
	// vgcfgrestore accepts.
	VGConfigs map[string]string `json:"vgConfigs,omitempty"`
}

// CollectError records one item that could not be collected.
type CollectError struct {
	// Item names what was being collected, e.g. "lsblk" or "fstab".
	Item string `json:"item"`
	// Err is the failure, as text: SystemInfo is written to a manifest.
	Err string `json:"error"`
}
