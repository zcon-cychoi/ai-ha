//go:build linux

package source

import (
	"strings"
	"testing"
)

// mountinfo of a cloud image whose root filesystem is listed as "/dev/root",
// plus an unrelated partition and a subdirectory bind mount of the root.
const testMountInfo = `21 26 0:20 / /sys rw,nosuid,nodev,noexec,relatime shared:2 - sysfs sysfs rw
26 1 259:1 / / rw,relatime shared:1 - ext4 /dev/root rw,discard,errors=remount-ro
101 26 259:1 /home /mnt/home-bind rw,relatime shared:1 - ext4 /dev/root rw,discard
110 26 259:3 / /boot/efi rw,relatime shared:3 - vfat /dev/nvme0n1p15 rw
120 26 0:52 / /mnt/backup\040dir rw,relatime shared:4 - ext4 /dev/nvme0n1p9 rw
`

func TestFindMountPoint(t *testing.T) {
	tests := []struct {
		name   string
		device string
		devNo  string
		want   string
	}{
		{"device number matches /dev/root entry", "/dev/nvme0n1p1", "259:1", "/"},
		{"mountinfo source name matches", "/dev/root", "", "/"},
		{"name match without device number", "/dev/nvme0n1p15", "", "/boot/efi"},
		{"escaped mount point is decoded", "/dev/nvme0n1p9", "0:52", "/mnt/backup dir"},
		{"unmounted device", "/dev/nvme0n1p14", "259:2", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := findMountPoint(strings.NewReader(testMountInfo), tt.device, tt.device, tt.devNo)
			if err != nil {
				t.Fatalf("findMountPoint: %v", err)
			}
			if got != tt.want {
				t.Errorf("findMountPoint(%s) = %q, want %q", tt.device, got, tt.want)
			}
		})
	}
}

func TestMajorMinorOf(t *testing.T) {
	// 259:1 encodes as 0x10301 in the kernel dev_t layout.
	const dev = 0x10301
	if maj, min := majorOf(dev), minorOf(dev); maj != 259 || min != 1 {
		t.Errorf("majorOf/minorOf(%#x) = %d:%d, want 259:1", dev, maj, min)
	}
}
