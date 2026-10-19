//go:build linux

package rescue

import (
	"fmt"
	"syscall"
)

// Filesystem magic numbers from statfs(2) for RAM-backed filesystems.
const (
	tmpfsMagic uint32 = 0x01021994
	ramfsMagic uint32 = 0x858458f6
)

// isRAMBacked reports whether path lies on tmpfs or ramfs, whose contents
// vanish on reboot and never reach a disk.
func isRAMBacked(path string) (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false, fmt.Errorf("statfs %s: %w", path, err)
	}
	fsType := uint32(st.Type)
	return fsType == tmpfsMagic || fsType == ramfsMagic, nil
}
