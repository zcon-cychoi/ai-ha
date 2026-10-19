package sysinfo

import (
	"strings"
	"testing"
)

func TestParseOSRelease(t *testing.T) {
	const in = `PRETTY_NAME="Ubuntu 24.04.1 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
VERSION="24.04.1 LTS (Noble Numbat)"
ID=ubuntu
ID_LIKE=debian
# a comment

UBUNTU_CODENAME=noble
`
	got, err := parseOSRelease(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.ID != "ubuntu" {
		t.Errorf("ID = %q, want ubuntu", got.ID)
	}
	if got.VersionID != "24.04" {
		t.Errorf("VersionID = %q, want 24.04", got.VersionID)
	}
	if got.Fields["PRETTY_NAME"] != "Ubuntu 24.04.1 LTS" {
		t.Errorf("PRETTY_NAME = %q (quotes not stripped?)", got.Fields["PRETTY_NAME"])
	}
	if got.Fields["UBUNTU_CODENAME"] != "noble" {
		t.Errorf("a key after a blank line and a comment was lost")
	}
	if !strings.Contains(got.Raw, "# a comment") {
		t.Error("Raw does not preserve the file verbatim")
	}
}

func TestParseFstab(t *testing.T) {
	const in = `# /etc/fstab: static file system information.
UUID=8f3a1c2e-0b7d-4e55-9a1f-2c6d8e0b7a31 /               ext4    defaults        0 1
UUID=A1B2-C3D4  /boot/efi       vfat    umask=0077      0 1
/dev/disk/by-id/x /mnt/my\040data ext4  defaults 0 2
tmpfs /tmp tmpfs defaults
badline
`
	got, err := parseFstab(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.Entries) != 4 {
		t.Fatalf("got %d entries, want 4: %+v", len(got.Entries), got.Entries)
	}
	if got.Entries[0].Spec != "UUID=8f3a1c2e-0b7d-4e55-9a1f-2c6d8e0b7a31" || got.Entries[0].MountPoint != "/" {
		t.Errorf("root entry = %+v", got.Entries[0])
	}
	if got.Entries[1].MountPoint != "/boot/efi" || got.Entries[1].FSType != "vfat" {
		t.Errorf("efi entry = %+v", got.Entries[1])
	}
	if got.Entries[2].MountPoint != "/mnt/my data" {
		t.Errorf("escaped space not decoded: %q", got.Entries[2].MountPoint)
	}
	// A four-field line is valid; dump and pass are optional.
	if got.Entries[3].FSType != "tmpfs" || got.Entries[3].Dump != "" {
		t.Errorf("four-field entry = %+v", got.Entries[3])
	}
}

func TestParseMountinfo(t *testing.T) {
	// Real Ubuntu 24.04 lines. Note the varying number of optional fields
	// before the "-" separator, which is why the tail is found by separator.
	const in = `25 30 259:1 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p1 rw,discard,errors=remount-ro
26 25 0:23 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw
31 25 259:16 / /boot/efi rw,relatime - vfat /dev/nvme0n1p16 rw,fmask=0077
36 25 0:35 / /mnt/my\040data rw,relatime shared:2 master:3 - ext4 /dev/sdb1 rw
short line here
`
	got, err := parseMountinfo(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d mounts, want 4: %+v", len(got), got)
	}
	if got[0].MountPoint != "/" || got[0].FSType != "ext4" || got[0].Source != "/dev/nvme0n1p1" {
		t.Errorf("root mount = %+v", got[0])
	}
	// No optional fields at all before "-".
	if got[2].MountPoint != "/boot/efi" || got[2].FSType != "vfat" || got[2].Source != "/dev/nvme0n1p16" {
		t.Errorf("efi mount (no optional fields) = %+v", got[2])
	}
	// Two optional fields before "-".
	if got[3].MountPoint != "/mnt/my data" || got[3].Source != "/dev/sdb1" {
		t.Errorf("mount with two optional fields = %+v", got[3])
	}
}

// The mount table of a plain Ubuntu server is mostly the running kernel: the
// pseudo filesystems outnumber the real ones many times over, and none of them
// is a fact a restore can use. What is left has to be the system's storage and
// nothing else.
func TestDiskMounts(t *testing.T) {
	// An Ubuntu 24.04 machine on LVM, with the pseudo filesystems it actually
	// reports and a snap's squashfs, which passes for a real device.
	const in = `23 28 0:22 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw
24 28 0:23 / /sys rw,nosuid,nodev,noexec,relatime shared:2 - sysfs sysfs rw
25 28 0:5 / /dev rw,nosuid,relatime shared:3 - devtmpfs udev rw,size=1963376k
26 25 0:24 / /dev/pts rw,nosuid,noexec,relatime shared:4 - devpts devpts rw,gid=5
28 1 252:0 / / rw,relatime shared:1 - ext4 /dev/mapper/ubuntu--vg-ubuntu--lv rw
31 28 0:29 / /run rw,nosuid,nodev,noexec,relatime shared:5 - tmpfs tmpfs rw,size=400988k
33 24 0:07 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime shared:9 - cgroup2 cgroup2 rw
35 28 0:30 / /dev/shm rw,nosuid,nodev shared:6 - tmpfs tmpfs rw
39 28 0:31 / /sys/kernel/tracing rw,nosuid,nodev,noexec,relatime shared:8 - tracefs tracefs rw
44 28 8:2 / /boot rw,relatime shared:24 - ext4 /dev/sda2 rw
92 28 7:1 / /snap/core22/1122 ro,nodev,relatime shared:47 - squashfs /dev/loop1 ro
95 28 0:55 / /proc/sys/fs/binfmt_misc rw,relatime shared:60 - autofs systemd-1 rw
`
	parsed, err := parseMountinfo(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := diskMounts(parsed)
	var points []string
	for _, m := range got {
		points = append(points, m.MountPoint)
	}
	if len(points) != 2 || points[0] != "/" || points[1] != "/boot" {
		t.Errorf("mount points = %v, want exactly [/ /boot]", points)
	}
	if len(got) > 0 && got[0].Source != "/dev/mapper/ubuntu--vg-ubuntu--lv" {
		t.Errorf("root source = %q, want the mapper path as the kernel reported it", got[0].Source)
	}
}

// A filesystem this agent cannot capture is still a partition of the system.
// Dropping it here would leave an operator unable to tell an exclusion the
// inventory explains from a partition that is simply missing.
func TestDiskMountsKeepWhatCannotBeBackedUp(t *testing.T) {
	for _, fsType := range []string{"btrfs", "ext2", "ext3", "ntfs3", "f2fs", "exfat"} {
		if !diskFSTypes[fsType] {
			t.Errorf("%s is not treated as an on-disk filesystem", fsType)
		}
	}
	// Every filesystem this agent captures must survive the filter, or a
	// backup target would vanish from the inventory that offers it.
	for fsType := range backupableFSTypes {
		if !diskFSTypes[fsType] {
			t.Errorf("%s is a backup candidate but is filtered out of the mount table", fsType)
		}
	}
}

func TestDiskOf(t *testing.T) {
	tests := []struct {
		source string
		want   string
		ok     bool
	}{
		{"/dev/sda1", "/dev/sda", true},
		{"/dev/sda", "/dev/sda", true},
		// An NVMe disk's own name ends in a digit, so its partitions must be
		// recognised by the "p" and not by trailing digits.
		{"/dev/nvme0n1p2", "/dev/nvme0n1", true},
		{"/dev/nvme0n1p16", "/dev/nvme0n1", true},
		{"/dev/nvme0n1", "/dev/nvme0n1", true},
		{"/dev/nvme1n2", "/dev/nvme1n2", true},
		{"/dev/mmcblk0p1", "/dev/mmcblk0", true},
		{"/dev/mmcblk0", "/dev/mmcblk0", true},
		{"/dev/xvda1", "/dev/xvda", true},
		{"/dev/vda3", "/dev/vda", true},
		{"/dev/mapper/rl-root", "", false},
		{"/dev/dm-0", "", false},
		{"/dev/loop3", "", false},
		{"/dev/md0", "", false},
		{"proc", "", false},
		{"tmpfs", "", false},
		{"/dev/", "", false},
	}
	for _, tt := range tests {
		got, ok := diskOf(tt.source)
		if ok != tt.ok || got != tt.want {
			t.Errorf("diskOf(%q) = %q,%v; want %q,%v", tt.source, got, ok, tt.want, tt.ok)
		}
	}
}

func TestDiskDevicesDeduplicates(t *testing.T) {
	mounts := []Mount{
		{Source: "/dev/nvme0n1p1", MountPoint: "/"},
		{Source: "/dev/nvme0n1p16", MountPoint: "/boot/efi"},
		{Source: "proc", MountPoint: "/proc"},
		{Source: "/dev/sdb1", MountPoint: "/data"},
	}
	got := diskDevices(mounts)
	want := []string{"/dev/nvme0n1", "/dev/sdb"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestVolumeGroupNames(t *testing.T) {
	const report = `{"report":[{"vg":[{"vg_name":"rl","vg_size":"<19.00g"},{"vg_name":"data"}],
	"pv":[{"pv_name":"/dev/sda2"}]}]}`
	got := volumeGroupNames([]byte(report))
	if len(got) != 2 || got[0] != "data" || got[1] != "rl" {
		t.Fatalf("got %v, want [data rl]", got)
	}
}

// A machine without LVM, and any document that does not match, must yield no
// names rather than an error: the caller records no per-group configuration.
func TestVolumeGroupNamesTolerAtesUnexpectedShapes(t *testing.T) {
	for _, in := range []string{`{"report":[]}`, `{"report":[{"vg":[]}]}`, `not json`, `{}`, `[]`} {
		if got := volumeGroupNames([]byte(in)); len(got) != 0 {
			t.Errorf("volumeGroupNames(%q) = %v, want none", in, got)
		}
	}
}
