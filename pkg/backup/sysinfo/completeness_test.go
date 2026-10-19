package sysinfo

import (
	"slices"
	"strings"
	"testing"
)

func TestCheckCompletenessOnAHealthySystem(t *testing.T) {
	info := awsSystem(t)
	parts, err := Partitions(info, awsResolver())
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}

	got := CheckCompleteness(info, parts)
	if !got.OK {
		t.Errorf("OK = false (%s), missing %v", got.Reason, got.Missing)
	}
}

// The case this check exists for: an ESP that is not mounted is simply absent
// from the mount table. Backing up only what is mounted would succeed and
// restore to a system that does not boot.
func TestCheckCompletenessCatchesAnUnmountedESP(t *testing.T) {
	info := awsSystem(t)
	// Same system, with /boot/efi unmounted. fstab still declares it.
	info.Mounts = mountsFrom(t, strings.ReplaceAll(awsMountinfo,
		"139 28 259:16 / /boot/efi rw,relatime shared:60 - vfat /dev/nvme0n1p16 rw,fmask=0077,dmask=0077\n", ""))

	parts, err := Partitions(info, awsResolver())
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	got := CheckCompleteness(info, parts)

	if got.OK {
		t.Fatal("an unmounted ESP passed as complete")
	}
	if !slices.Contains(got.Missing, "/boot/efi") {
		t.Errorf("Missing = %v, want it to name /boot/efi", got.Missing)
	}
	if !strings.Contains(got.Reason, "/boot/efi") {
		t.Errorf("Reason = %q, want it to name the filesystem", got.Reason)
	}
}

func TestCheckCompletenessWithoutARootFilesystem(t *testing.T) {
	const mountinfo = `139 28 259:16 / /boot/efi rw,relatime shared:60 - vfat /dev/nvme0n1p16 rw
`
	info := &SystemInfo{Mounts: mountsFrom(t, mountinfo)}
	parts, err := Partitions(info, awsResolver())
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}

	got := CheckCompleteness(info, parts)
	if got.OK {
		t.Fatal("a system with no root filesystem passed as complete")
	}
	if !slices.Contains(got.Missing, "/") {
		t.Errorf("Missing = %v, want it to name /", got.Missing)
	}
}

func TestCheckCompletenessWithNoCandidates(t *testing.T) {
	got := CheckCompleteness(&SystemInfo{}, nil)
	if got.OK {
		t.Fatal("a system with nothing to back up passed as complete")
	}
	if got.Reason == "" {
		t.Error("no reason was given")
	}
}

// An entry the system itself says it boots without is not a gap.
func TestCheckCompletenessIgnoresOptionalAndPseudoEntries(t *testing.T) {
	info := awsSystem(t)
	info.Fstab.Entries = append(info.Fstab.Entries,
		FstabEntry{Spec: "/swap.img", MountPoint: "none", FSType: "swap", Options: "sw"},
		FstabEntry{Spec: "UUID=dead", MountPoint: "/mnt/scratch", FSType: "ext4", Options: "defaults,nofail"},
		FstabEntry{Spec: "UUID=beef", MountPoint: "/mnt/archive", FSType: "ext4", Options: "noauto,user"},
		FstabEntry{Spec: "tmpfs", MountPoint: "/tmp", FSType: "tmpfs", Options: "defaults"},
	)

	parts, err := Partitions(info, awsResolver())
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	got := CheckCompleteness(info, parts)
	if !got.OK {
		t.Errorf("OK = false (%s), missing %v; none of these entries is required to boot", got.Reason, got.Missing)
	}
}

// A required filesystem that is mounted but on something this agent cannot
// capture is still a gap: it is not in the backup either way.
func TestCheckCompletenessCatchesARequiredFilesystemThatIsNotACandidate(t *testing.T) {
	const mountinfo = `28 1 259:1 / / rw,relatime shared:1 - ext4 /dev/root rw
101 28 259:3 / /data rw,relatime shared:71 - btrfs /dev/nvme0n1p3 rw
`
	info := &SystemInfo{
		Mounts: mountsFrom(t, mountinfo),
		Fstab: Fstab{Entries: []FstabEntry{
			{Spec: "LABEL=root", MountPoint: "/", FSType: "ext4", Options: "defaults"},
			{Spec: "UUID=cafe", MountPoint: "/data", FSType: "btrfs", Options: "defaults"},
		}},
	}
	parts, err := Partitions(info, fakeResolver{"259:1": "/dev/nvme0n1p1", "259:3": "/dev/nvme0n1p3"})
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}

	got := CheckCompleteness(info, parts)
	if got.OK {
		t.Fatal("a required filesystem this agent cannot capture passed as complete")
	}
	if !slices.Contains(got.Missing, "/data") {
		t.Errorf("Missing = %v, want it to name /data", got.Missing)
	}
}
