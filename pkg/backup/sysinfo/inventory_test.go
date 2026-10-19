package sysinfo

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloud-barista/ai-ha/pkg/backup/source"
)

// fakeResolver answers device-number lookups from a table, so that the tests
// do not depend on the machine they run on.
type fakeResolver map[string]string

func (f fakeResolver) Resolve(devNo string) (string, error) {
	if path, ok := f[devNo]; ok {
		return path, nil
	}
	return "", errors.New("no such device")
}

// awsMountinfo is what an Ubuntu 24.04 AWS instance reports: the root
// filesystem named "/dev/root" rather than by its real path, an ESP on a
// separate partition, and the usual pseudo filesystems.
const awsMountinfo = `23 28 0:22 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw
24 28 0:23 / /sys rw,nosuid,nodev,noexec,relatime shared:2 - sysfs sysfs rw
28 1 259:1 / / rw,relatime shared:1 - ext4 /dev/root rw,discard,errors=remount-ro
31 23 0:29 / /run rw,nosuid,nodev shared:5 - tmpfs tmpfs rw,size=198336k,mode=755
92 28 7:1 / /snap/core22/1122 ro,nodev,relatime shared:47 - squashfs /dev/loop1 ro
139 28 259:16 / /boot/efi rw,relatime shared:60 - vfat /dev/nvme0n1p16 rw,fmask=0077,dmask=0077
`

// awsLsblk is the matching "lsblk -J -O --bytes" fragment, trimmed to the
// fields an inventory reads.
const awsLsblk = `{"blockdevices":[
  {"name":"nvme0n1","maj:min":"259:0","path":"/dev/nvme0n1","children":[
    {"name":"nvme0n1p1","maj:min":"259:1","path":"/dev/nvme0n1p1","fstype":"ext4",
     "uuid":"11111111-2222-3333-4444-555555555555","partuuid":"aaaaaaaa-01",
     "label":"cloudimg-rootfs","fssize":8298131456,"fsused":2147483648},
    {"name":"nvme0n1p16","maj:min":"259:16","path":"/dev/nvme0n1p16","fstype":"vfat",
     "uuid":"ABCD-1234","partuuid":"aaaaaaaa-16","label":"UEFI",
     "fssize":109422592,"fsused":6291456}]}]}`

// mountsFrom builds the mount list as Collect would: parsed, then reduced to
// the mounts of a real filesystem. Feeding Partitions an unfiltered table
// would test a combination that cannot occur.
func mountsFrom(t *testing.T, text string) []Mount {
	t.Helper()
	mounts, err := parseMountinfo(strings.NewReader(text))
	if err != nil {
		t.Fatalf("parse mountinfo: %v", err)
	}
	return diskMounts(mounts)
}

func awsSystem(t *testing.T) *SystemInfo {
	t.Helper()
	return &SystemInfo{
		Mounts:       mountsFrom(t, awsMountinfo),
		BlockDevices: json.RawMessage(awsLsblk),
		Fstab: Fstab{Entries: []FstabEntry{
			{Spec: "LABEL=cloudimg-rootfs", MountPoint: "/", FSType: "ext4", Options: "discard,errors=remount-ro"},
			{Spec: "LABEL=UEFI", MountPoint: "/boot/efi", FSType: "vfat", Options: "umask=0077"},
		}},
	}
}

func awsResolver() fakeResolver {
	return fakeResolver{"259:1": "/dev/nvme0n1p1", "259:16": "/dev/nvme0n1p16"}
}

// byMountPoint indexes a partition list for assertions.
func byMountPoint(parts []Partition) map[string]Partition {
	out := make(map[string]Partition, len(parts))
	for _, p := range parts {
		out[p.MountPoint] = p
	}
	return out
}

// The mount table is the one thing an inventory cannot do without. Reporting
// an empty list would overwrite a good inventory and look just as fresh, so
// the failure has to be distinguishable from "this system has no partitions".
func TestPartitionsWithoutAMountTable(t *testing.T) {
	for _, c := range []struct {
		name string
		info *SystemInfo
	}{
		{"no mounts collected", &SystemInfo{}},
		{"no system info at all", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Partitions(c.info, awsResolver())
			if !errors.Is(err, ErrNoMountTable) {
				t.Fatalf("err = %v, want ErrNoMountTable", err)
			}
		})
	}
}

// "/dev/root" is what a cloud image calls its root filesystem. Handing that
// back as a device to back up would fail, so the path has to come from the
// device number instead.
func TestPartitionsResolvesDevRootToItsRealPath(t *testing.T) {
	parts, err := Partitions(awsSystem(t), awsResolver())
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}

	root, ok := byMountPoint(parts)["/"]
	if !ok {
		t.Fatal("the root filesystem is not in the inventory")
	}
	if root.Device != "/dev/nvme0n1p1" {
		t.Errorf("Device = %q, want /dev/nvme0n1p1 (never /dev/root)", root.Device)
	}
	if !root.Candidate || !root.Selectable {
		t.Errorf("root is not a backup candidate: %+v", root)
	}
}

func TestPartitionsOnAWSUbuntu(t *testing.T) {
	parts, err := Partitions(awsSystem(t), awsResolver())
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	index := byMountPoint(parts)

	esp, ok := index["/boot/efi"]
	if !ok {
		t.Fatal("the ESP is not in the inventory")
	}
	if !esp.Candidate {
		t.Errorf("the ESP is not a candidate: %+v", esp)
	}
	if esp.FSUUID != "ABCD-1234" || esp.PartUUID != "aaaaaaaa-16" || esp.Label != "UEFI" {
		t.Errorf("the ESP was not enriched from lsblk: %+v", esp)
	}
	if esp.FSSizeBytes == nil || *esp.FSSizeBytes != 109422592 {
		t.Errorf("FSSizeBytes = %v, want 109422592", esp.FSSizeBytes)
	}

	var candidates []string
	for _, p := range parts {
		if p.Candidate {
			candidates = append(candidates, p.MountPoint)
		}
	}
	if len(candidates) != 2 {
		t.Errorf("candidates = %v, want exactly / and /boot/efi", candidates)
	}
}

// A partition that cannot be captured must not merely be left off the list:
// an operator naming it by hand has to be refused, and told why.
func TestPartitionsExcludesWhatCannotBeCaptured(t *testing.T) {
	const mountinfo = `28 1 259:1 / / rw,relatime shared:1 - ext4 /dev/root rw
31 23 0:29 / /run rw,nosuid shared:5 - tmpfs tmpfs rw,size=198336k
92 28 7:1 / /snap/core22/1122 ro,relatime shared:47 - squashfs /dev/loop1 ro
95 28 0:52 / /var/lib/docker/overlay ro,relatime shared:48 - overlay overlay rw
99 28 259:2 / /mnt/aiha-mount-sda1-9f3c rw,relatime shared:70 - ext4 /dev/nvme0n1p2 rw
101 28 259:3 / /data rw,relatime shared:71 - btrfs /dev/nvme0n1p3 rw
`
	info := &SystemInfo{Mounts: mountsFrom(t, mountinfo)}
	parts, err := Partitions(info, fakeResolver{
		"259:1": "/dev/nvme0n1p1",
		"259:2": "/dev/nvme0n1p2",
		"259:3": "/dev/nvme0n1p3",
		"7:1":   "/dev/loop1",
	})
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	index := byMountPoint(parts)

	for _, c := range []struct{ mountPoint, wantReason string }{
		{"/mnt/aiha-mount-sda1-9f3c", "temporary mount"},
		{"/data", "btrfs"},
	} {
		p, ok := index[c.mountPoint]
		if !ok {
			t.Errorf("%s is missing from the inventory; it should be listed and explained", c.mountPoint)
			continue
		}
		if p.Candidate || p.Selectable {
			t.Errorf("%s is offered for backup: %+v", c.mountPoint, p)
		}
		if !strings.Contains(p.Reason, c.wantReason) {
			t.Errorf("%s reason = %q, want it to mention %q", c.mountPoint, p.Reason, c.wantReason)
		}
	}

	// tmpfs and overlay name no block device, and a snap's squashfs is not a
	// filesystem of this system at all. None of them reaches the inventory.
	for _, mountPoint := range []string{"/run", "/var/lib/docker/overlay", "/snap/core22/1122"} {
		if _, ok := index[mountPoint]; ok {
			t.Errorf("%s is listed, but it has no partition to back up", mountPoint)
		}
	}
}

// The representative mount is the one of the whole filesystem at the shortest
// mount point — not whichever the kernel happens to list first. The other
// mounts of that filesystem are the same partition seen again, so an inventory
// that is a list of partitions does not repeat them.
func TestPartitionsPrefersTheRepresentativeMount(t *testing.T) {
	const mountinfo = `40 28 259:1 /home /export/home rw,relatime shared:9 - ext4 /dev/root rw
41 28 259:1 / /var/snap/copy rw,relatime shared:10 - ext4 /dev/root rw
28 1 259:1 / / rw,relatime shared:1 - ext4 /dev/root rw
`
	info := &SystemInfo{Mounts: mountsFrom(t, mountinfo)}
	parts, err := Partitions(info, fakeResolver{"259:1": "/dev/nvme0n1p1"})
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}

	if len(parts) != 1 {
		t.Fatalf("one filesystem produced %d partitions: %+v", len(parts), parts)
	}
	if parts[0].MountPoint != "/" || !parts[0].Candidate {
		t.Errorf("/ was not chosen as the representative mount: %+v", parts[0])
	}
}

// systemd gives a unit with ProtectHome or StateDirectory a mount namespace of
// its own and bind mounts those paths inside it. Nothing of the sort exists on
// the host, so an agent that reported them would describe a machine with three
// partitions that no operator can find. The unit no longer asks for the
// namespace; this holds the reporting side to the same answer if one appears
// anyway, from a container or a hand-rolled systemd-run.
func TestPartitionsIgnoreAPrivateNamespacesBindMounts(t *testing.T) {
	const mountinfo = `28 1 252:0 / / rw,relatime shared:1 - ext4 /dev/mapper/ubuntu--vg-ubuntu--lv rw
30 28 8:2 / /boot rw,relatime shared:24 - ext4 /dev/sda2 rw
88 28 252:0 /home /home rw,relatime shared:1 - ext4 /dev/mapper/ubuntu--vg-ubuntu--lv rw
89 28 252:0 /root /root rw,relatime shared:1 - ext4 /dev/mapper/ubuntu--vg-ubuntu--lv rw
90 28 252:0 /var/lib/aiha /var/lib/aiha rw,relatime shared:1 - ext4 /dev/mapper/ubuntu--vg-ubuntu--lv rw
`
	info := &SystemInfo{Mounts: mountsFrom(t, mountinfo)}
	parts, err := Partitions(info, fakeResolver{
		"252:0": "/dev/mapper/ubuntu--vg-ubuntu--lv",
		"8:2":   "/dev/sda2",
	})
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}

	var got []string
	for _, p := range parts {
		got = append(got, p.MountPoint)
	}
	if len(got) != 2 || got[0] != "/" || got[1] != "/boot" {
		t.Errorf("mount points = %v, want exactly [/ /boot]", got)
	}
}

// A device whose path cannot be read is not the same as one that is not there.
func TestPartitionsKeepsAnUnresolvableDevice(t *testing.T) {
	info := &SystemInfo{Mounts: mountsFrom(t, awsMountinfo)}
	parts, err := Partitions(info, fakeResolver{"259:16": "/dev/nvme0n1p16"})
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}

	root, ok := byMountPoint(parts)["/"]
	if !ok {
		t.Fatal("the root mount was dropped rather than explained")
	}
	if root.Candidate || root.Selectable {
		t.Errorf("a partition with no device path is offered for backup: %+v", root)
	}
	if !strings.Contains(root.Reason, "could not be resolved") {
		t.Errorf("Reason = %q, want it to say the path could not be resolved", root.Reason)
	}
}

// lsblk is an enrichment. Its column set changes between releases, and a
// partition list that depended on it would go empty on a version that spells
// something differently.
func TestPartitionsSurviveUnusableLsblk(t *testing.T) {
	for _, c := range []struct {
		name   string
		lsblk  string
		reason string
	}{
		{"absent", "", "lsblk did not run"},
		{"not json", "not json at all", "lsblk output was unreadable"},
		{"older column set", `{"blockdevices":[{"name":"nvme0n1p1","mountpoint":"/"}]}`, "no maj:min to match on"},
	} {
		t.Run(c.name, func(t *testing.T) {
			info := awsSystem(t)
			info.BlockDevices = json.RawMessage(c.lsblk)

			parts, err := Partitions(info, awsResolver())
			if err != nil {
				t.Fatalf("Partitions: %v", err)
			}
			root, ok := byMountPoint(parts)["/"]
			if !ok {
				t.Fatalf("the partition list went empty when %s (%s)", c.name, c.reason)
			}
			if !root.Candidate {
				t.Errorf("/ stopped being a candidate when %s: %+v", c.name, root)
			}
			if root.FSSizeBytes != nil {
				t.Errorf("FSSizeBytes = %v, want nil when the size is unknown", *root.FSSizeBytes)
			}
			if root.FSUUID != "" {
				t.Errorf("FSUUID = %q, want empty when lsblk gave nothing", root.FSUUID)
			}
		})
	}
}

// An unknown size and an empty filesystem are different facts.
func TestPartitionsDistinguishAnUnknownSizeFromZero(t *testing.T) {
	info := awsSystem(t)
	info.BlockDevices = json.RawMessage(`{"blockdevices":[
      {"name":"nvme0n1p1","maj:min":"259:1","path":"/dev/nvme0n1p1","fssize":0,"fsused":0},
      {"name":"nvme0n1p16","maj:min":"259:16","path":"/dev/nvme0n1p16","fssize":null,"fsused":null}]}`)

	parts, err := Partitions(info, awsResolver())
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	index := byMountPoint(parts)

	root := index["/"]
	if root.FSSizeBytes == nil || *root.FSSizeBytes != 0 {
		t.Errorf("a reported zero became unknown: %v", root.FSSizeBytes)
	}
	if esp := index["/boot/efi"]; esp.FSSizeBytes != nil {
		t.Errorf("an unreported size became %d", *esp.FSSizeBytes)
	}
}

// lsblk emitted these as strings before it emitted them as numbers.
func TestPartitionsReadSizesReportedAsStrings(t *testing.T) {
	info := awsSystem(t)
	info.BlockDevices = json.RawMessage(
		`{"blockdevices":[{"name":"nvme0n1p1","maj:min":"259:1","fssize":"8298131456","fsused":"2147483648"}]}`)

	parts, err := Partitions(info, awsResolver())
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	root := byMountPoint(parts)["/"]
	if root.FSSizeBytes == nil || *root.FSSizeBytes != 8298131456 {
		t.Errorf("FSSizeBytes = %v, want 8298131456", root.FSSizeBytes)
	}
}

func TestSysfsResolver(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "259:1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "259:1", "uevent"),
		[]byte("MAJOR=259\nMINOR=1\nDEVNAME=nvme0n1p1\nDEVTYPE=partition\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := SysfsResolver{Root: root}

	got, err := r.Resolve("259:1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "/dev/nvme0n1p1" {
		t.Errorf("Resolve = %q, want /dev/nvme0n1p1", got)
	}

	if _, err := r.Resolve("259:2"); err == nil {
		t.Error("an unknown device number resolved")
	}
	// The device number reaches the filesystem as a path element, so anything
	// that is not two numbers has to be refused.
	for _, bad := range []string{"../../etc", "259", "", "a:b", "259:1/../../.."} {
		if _, err := r.Resolve(bad); err == nil {
			t.Errorf("Resolve(%q) succeeded; it is not a device number", bad)
		}
	}
}

// An LVM volume's kernel node is "dm-0", whose minor number is handed out in
// activation order: it need not name the same volume after a reboot, and it is
// not what an operator types. The mapper name is both.
func TestSysfsResolverNamesDeviceMapperByItsMapperPath(t *testing.T) {
	root := t.TempDir()
	writeDMName := func(devNo, name string) {
		t.Helper()
		dir := filepath.Join(root, devNo, "dm")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// The kernel writes the name with a trailing newline, and DEVNAME is
		// present on a dm device too — it just says "dm-N".
		if err := os.WriteFile(filepath.Join(dir, "name"), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, devNo, "uevent"),
			[]byte("DEVNAME=dm-0\nDEVTYPE=disk\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := SysfsResolver{Root: root}

	writeDMName("252:0", "ubuntu--vg-ubuntu--lv")
	got, err := r.Resolve("252:0")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "/dev/mapper/ubuntu--vg-ubuntu--lv" {
		t.Errorf("Resolve = %q, want /dev/mapper/ubuntu--vg-ubuntu--lv (never /dev/dm-0)", got)
	}

	// The name becomes a path element. One that could reach outside
	// /dev/mapper falls back to the kernel node rather than being resolved.
	for _, bad := range []string{"../../etc/passwd", "..", ".", "", "  "} {
		writeDMName("252:1", bad)
		got, err := r.Resolve("252:1")
		if err != nil {
			t.Fatalf("Resolve with dm name %q: %v", bad, err)
		}
		if got != "/dev/dm-0" {
			t.Errorf("dm name %q resolved to %q, want the kernel node", bad, got)
		}
	}
}

// agentMountPrefix is a copy of the constant pkg/backup/source creates its
// temporary bind mounts with. If the two drift apart, an inventory starts
// listing the agent's own mounts as partitions of the system.
func TestAgentMountPrefixMatchesTheOneBackupsUse(t *testing.T) {
	if agentMountPrefix != source.MountPrefix {
		t.Fatalf("agentMountPrefix = %q, but backups mount under %q",
			agentMountPrefix, source.MountPrefix)
	}
}

// A filesystem mounted only as a subtree, and nowhere in full, is nobody's
// representative. Dropping it would remove a partition from the inventory
// without trace, so it stays and is explained — the one case in which a mount
// is still called a bind mount.
func TestPartitionsKeepsABindMountOfAnUnlistedFilesystem(t *testing.T) {
	const mountinfo = `40 28 259:1 /home /export/home rw,relatime shared:9 - ext4 /dev/nvme0n1p1 rw
28 1 259:9 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p9 rw
`
	info := &SystemInfo{Mounts: mountsFrom(t, mountinfo)}
	parts, err := Partitions(info, fakeResolver{
		"259:1": "/dev/nvme0n1p1",
		"259:9": "/dev/nvme0n1p9",
	})
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}

	p, ok := byMountPoint(parts)["/export/home"]
	if !ok {
		t.Fatal("the mount was dropped rather than explained")
	}
	if p.Candidate || p.Selectable {
		t.Errorf("a subtree bind mount is offered for backup: %+v", p)
	}
	if !strings.Contains(p.Reason, "bind mount") {
		t.Errorf("Reason = %q, want it to say this is a bind mount", p.Reason)
	}
	// It claims nothing about a filesystem the list does not contain.
	if strings.Contains(p.Reason, "listed elsewhere") {
		t.Errorf("Reason = %q, but that filesystem is not in the list", p.Reason)
	}
}
