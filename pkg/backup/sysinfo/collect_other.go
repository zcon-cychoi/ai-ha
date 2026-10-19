//go:build !linux

package sysinfo

import (
	"context"
	"fmt"
	"runtime"
)

// Collect reports that system information can only be gathered on Linux. The
// facts it collects — mountinfo, sysfs, partition tables — have no equivalent
// elsewhere, and the systems this project protects are Linux.
func Collect(context.Context, Runner) (*SystemInfo, error) {
	return nil, fmt.Errorf("system information is only collected on linux, not %s", runtime.GOOS)
}
