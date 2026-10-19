package sysinfo

import (
	"fmt"
	"strings"
)

// A partition list built from the mount table knows only what is mounted. That
// is enough to back up, and not enough to decide what to back up: a separate
// /boot or ESP that happens to be unmounted is simply absent from it, and a
// set chosen automatically without one restores to a system that does not
// boot. The gap shows up at restore time, when the original is gone.
//
// fstab is what the system itself says it needs. Comparing the two is cheap
// and catches exactly that case.

// Completeness reports whether an automatic selection of backup targets can be
// trusted.
type Completeness struct {
	// OK is true only when every filesystem the system declares is accounted
	// for. Automatic selection refuses to act when it is false; naming the
	// partitions by hand is still allowed, since an operator may know
	// something fstab does not.
	OK bool
	// Missing names mount points fstab declares that no candidate provides,
	// e.g. "/boot/efi".
	Missing []string
	// Reason is why OK is false, in an operator's terms.
	Reason string
}

// pseudoFSTypes are fstab entries that name no partition to back up.
var pseudoFSTypes = map[string]bool{
	"swap": true, "none": true, "tmpfs": true, "proc": true, "sysfs": true,
	"devpts": true, "devtmpfs": true, "cgroup": true, "cgroup2": true,
	"overlay": true, "squashfs": true, "efivarfs": true, "autofs": true,
	"nfs": true, "nfs4": true, "cifs": true, "fuse": true, "fuse.sshfs": true,
}

// CheckCompleteness compares the candidate partitions against what fstab says
// the system is made of.
func CheckCompleteness(info *SystemInfo, parts []Partition) Completeness {
	mounted := make(map[string]bool)
	candidates := 0
	for _, p := range parts {
		if p.Candidate {
			mounted[p.MountPoint] = true
			candidates++
		}
	}

	switch {
	case candidates == 0:
		return Completeness{Reason: "no partition of this system can be backed up by this agent"}
	case !mounted["/"]:
		return Completeness{
			Missing: []string{"/"},
			Reason:  "the root filesystem was not identified",
		}
	}

	var missing []string
	if info != nil {
		for _, e := range info.Fstab.Entries {
			if !declaresBackupTarget(e) || mounted[e.MountPoint] {
				continue
			}
			missing = append(missing, e.MountPoint)
		}
	}
	if len(missing) > 0 {
		return Completeness{
			Missing: missing,
			Reason: fmt.Sprintf(
				"fstab declares %s, which is not mounted: a backup without it may not boot after a restore",
				strings.Join(missing, ", ")),
		}
	}
	return Completeness{OK: true}
}

// declaresBackupTarget reports whether an fstab entry names a local filesystem
// that a backup of this system ought to include.
//
// Entries marked noauto or nofail are excluded: the system is declaring that
// it boots without them, so their absence is a choice rather than a gap.
func declaresBackupTarget(e FstabEntry) bool {
	if !strings.HasPrefix(e.MountPoint, "/") || pseudoFSTypes[e.FSType] {
		return false
	}
	for opt := range strings.SplitSeq(e.Options, ",") {
		switch strings.TrimSpace(opt) {
		case "noauto", "nofail":
			return false
		}
	}
	return true
}
