package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/framework/inventory"
)

// storedWithBindMounts is a system whose root filesystem is also mounted
// elsewhere, as it is on any host running containers. The agent reports one
// row per mount, with the extra ones explained.
func storedWithBindMounts() inventory.Stored {
	now := time.Now().UTC()
	return inventory.Stored{
		SystemID:    "sys-0123456789abcdef",
		CollectedAt: now.Add(-time.Minute),
		ReportedAt:  now.Add(-time.Minute),
		Now:         now,
		Complete:    true,
		Inventory: inventory.Inventory{
			CollectedAt: now.Add(-time.Minute),
			Complete:    true,
			Partitions: []inventory.Partition{
				{Device: "/dev/sdd", MountPoint: "/", FSType: "ext4", Candidate: true, Selectable: true},
				{Device: "/dev/sdd", MountPoint: "/var/lib/docker", FSType: "ext4",
					Reason: "a bind mount of a filesystem listed elsewhere"},
				{Device: "/dev/sdd", MountPoint: "/mnt/wslg/distro", FSType: "ext4",
					Reason: "a bind mount of a filesystem listed elsewhere"},
			},
		},
	}
}

// The listing has to agree with the count above it. Printing every row for a
// filesystem mounted three times would show three lines under a heading that
// says one, which reads as three filesystems being captured.
func TestPrintChosenPrintsOneLinePerDevice(t *testing.T) {
	var out bytes.Buffer
	printChosen(&out, storedWithBindMounts(), []string{"/dev/sdd"})

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var devices int
	for _, line := range lines {
		if strings.Contains(line, "/dev/sdd") {
			devices++
		}
	}
	if devices != 1 {
		t.Errorf("printed %d lines for one device:\n%s", devices, out.String())
	}

	// The representative mount is the one worth showing, not a bind mount.
	if !strings.Contains(out.String(), " / ") {
		t.Errorf("the root mount point is not shown:\n%s", out.String())
	}
	for _, bind := range []string{"/var/lib/docker", "/mnt/wslg/distro"} {
		if strings.Contains(out.String(), bind) {
			t.Errorf("a bind mount is listed as a backup target:\n%s", out.String())
		}
	}
}

// What an operator is offered after a refusal must also be one line per device.
func TestPrintSelectableListsOnlyWhatCanBeBackedUp(t *testing.T) {
	var out bytes.Buffer
	printSelectable(&out, storedWithBindMounts())

	if n := strings.Count(out.String(), "/dev/sdd"); n != 1 {
		t.Errorf("listed the device %d times:\n%s", n, out.String())
	}
}

func TestPrintSelectableWithNothingToOffer(t *testing.T) {
	var out bytes.Buffer
	printSelectable(&out, inventory.Stored{})

	if out.Len() == 0 {
		t.Error("a system with nothing to back up produced no explanation")
	}
}
