//go:build linux

package spool

import "syscall"

// freeBytes returns the space available to an unprivileged writer on dir's
// filesystem.
func freeBytes(dir string) (int64, bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false, err
	}
	return int64(st.Bavail) * st.Bsize, true, nil
}
