//go:build !rescue

package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/cloud-barista/ai-ha/pkg/backup/source"
)

// stubSource stands in for a partition source. The real one bind mounts, which
// a test cannot do; what matters here is what the check does with what a
// source reports about itself.
type stubSource struct {
	device     string
	mountPoint string
}

func (s stubSource) Name() string   { return strings.TrimPrefix(s.device, "/dev/") }
func (s stubSource) Device() string { return s.device }
func (s stubSource) MountPoint(context.Context) string {
	return s.mountPoint
}

func (s stubSource) Prepare(context.Context) (string, func() error, error) {
	return "", func() error { return nil }, nil
}

// plainSource has no device to check, standing in for a source that is just a
// directory.
type plainSource struct{}

func (plainSource) Name() string { return "plain" }
func (plainSource) Prepare(context.Context) (string, func() error, error) {
	return "", func() error { return nil }, nil
}

func TestCheckDevicesAcceptsMountedPartitions(t *testing.T) {
	sources := []source.Source{
		stubSource{device: "/dev/nvme0n1p1", mountPoint: "/"},
		stubSource{device: "/dev/nvme0n1p16", mountPoint: "/boot/efi"},
	}

	if err := checkDevices(context.Background(), sources, nil); err != nil {
		t.Fatalf("checkDevices: %v", err)
	}
}

// A device that is gone must be refused here rather than partway through the
// set, after the partitions before it have already been uploaded.
func TestCheckDevicesRefusesAnUnmountedPartition(t *testing.T) {
	sources := []source.Source{
		stubSource{device: "/dev/nvme0n1p1", mountPoint: "/"},
		stubSource{device: "/dev/nvme0n1p16", mountPoint: ""},
	}

	err := checkDevices(context.Background(), sources, nil)
	if err == nil {
		t.Fatal("an unmounted partition was accepted")
	}
	if !strings.Contains(err.Error(), "/dev/nvme0n1p16") {
		t.Errorf("err = %q, want it to name the partition", err)
	}
}

// A source with no device of its own is not what this check is about.
func TestCheckDevicesIgnoresASourceWithNoDevice(t *testing.T) {
	if err := checkDevices(context.Background(), []source.Source{plainSource{}}, nil); err != nil {
		t.Fatalf("checkDevices: %v", err)
	}
}

// With nothing to compare against, the identity check does not run at all —
// so it cannot fail a backup an operator asked for by hand.
func TestCheckDevicesSkipsTheIdentityCheckWithoutExpectations(t *testing.T) {
	sources := []source.Source{stubSource{device: "/dev/nvme0n1p1", mountPoint: "/"}}

	if err := checkDevices(context.Background(), sources, map[string]string{}); err != nil {
		t.Fatalf("checkDevices: %v", err)
	}
}

func TestCheckFilesystemIdentity(t *testing.T) {
	sources := []source.Source{stubSource{device: "/dev/nvme0n1p1", mountPoint: "/"}}
	current := map[string]string{"/dev/nvme0n1p1": "uuid-root"}

	t.Run("matching", func(t *testing.T) {
		err := compareFilesystems(sources, current, map[string]string{"/dev/nvme0n1p1": "uuid-root"})
		if err != nil {
			t.Fatalf("compareFilesystems: %v", err)
		}
	})

	// The case that produces no error of its own: the path was reused, so the
	// backup would succeed and be a backup of something else.
	t.Run("the partition was replaced", func(t *testing.T) {
		err := compareFilesystems(sources, current, map[string]string{"/dev/nvme0n1p1": "uuid-from-the-old-disk"})
		if err == nil {
			t.Fatal("a replaced partition was accepted")
		}
		for _, want := range []string{"/dev/nvme0n1p1", "uuid-root", "uuid-from-the-old-disk"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, want it to mention %q", err, want)
			}
		}
	})

	// The check failing is not the device failing.
	t.Run("the current uuid could not be read", func(t *testing.T) {
		err := compareFilesystems(sources, map[string]string{}, map[string]string{"/dev/nvme0n1p1": "uuid-root"})
		if err != nil {
			t.Fatalf("a backup was refused because the check could not run: %v", err)
		}
	})

	// A device the framework had no identity for is not guessed at.
	t.Run("no expectation for this device", func(t *testing.T) {
		err := compareFilesystems(sources, current, map[string]string{"/dev/other": "uuid-other"})
		if err != nil {
			t.Fatalf("compareFilesystems: %v", err)
		}
	})
}
