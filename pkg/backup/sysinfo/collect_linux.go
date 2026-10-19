//go:build linux

package sysinfo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Paths read directly. Reading beats shelling out: it is faster, it cannot
// fail on a missing tool, and it keeps os/exec confined to runner.go.
const (
	osReleasePath = "/etc/os-release"
	fstabPath     = "/etc/fstab"
	mountinfoPath = "/proc/self/mountinfo"
	efiPath       = "/sys/firmware/efi"
	netplanDir    = "/etc/netplan"
)

// maxFileBytes caps a single file read into the manifest.
const maxFileBytes = 1 << 20

// Collect gathers the system facts a restore needs.
//
// It does not fail on a missing item. Each failure is appended to Errors and
// collection continues, because a backup that aborts over an unreadable
// netplan file is worse than a backup whose manifest is missing one field.
// It returns an error only if nothing at all could be determined.
func Collect(ctx context.Context, runner Runner) (*SystemInfo, error) {
	if runner == nil {
		runner = ExecRunner{}
	}
	info := &SystemInfo{CollectedAt: time.Now().UTC()}

	info.Kernel = collectKernel(info)
	info.OSRelease = collectOSRelease(info)
	info.Firmware = collectFirmware()
	info.Mounts = collectMounts(info)
	info.Fstab = collectFstab(info)
	info.NetplanFiles = collectNetplan(info)
	info.BlockDevices = collectBlockDevices(ctx, runner, info)
	info.PartitionTables = collectPartitionTables(ctx, runner, info, info.Mounts, SysfsResolver{})
	info.LVM = collectLVM(ctx, runner, info)

	return info, nil
}

// record appends a collection failure. A missing tool is normal on a system
// that lacks the subsystem, so it is not recorded as an error.
func (s *SystemInfo) record(item string, err error) {
	if err == nil || errors.Is(err, ErrCommandNotFound) || errors.Is(err, fs.ErrNotExist) {
		return
	}
	s.Errors = append(s.Errors, CollectError{Item: item, Err: err.Error()})
}

func collectKernel(info *SystemInfo) Kernel {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		info.record("uname", fmt.Errorf("uname: %w", err))
		return Kernel{}
	}
	return Kernel{
		Release: utsToString(u.Release[:]),
		Version: utsToString(u.Version[:]),
		Machine: utsToString(u.Machine[:]),
		Sysname: utsToString(u.Sysname[:]),
	}
}

// utsToString converts a NUL-terminated utsname field to a string.
func utsToString(field []int8) string {
	b := make([]byte, 0, len(field))
	for _, c := range field {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

func collectOSRelease(info *SystemInfo) OSRelease {
	b, err := readFileLimit(osReleasePath)
	if err != nil {
		info.record("osRelease", err)
		return OSRelease{}
	}
	out, err := parseOSRelease(bytes.NewReader(b))
	if err != nil {
		info.record("osRelease", err)
		return OSRelease{}
	}
	return out
}

// collectFirmware reports how the system booted. The presence of
// /sys/firmware/efi is the standard test: the kernel creates it only when it
// booted through UEFI.
func collectFirmware() Firmware {
	if fi, err := os.Stat(efiPath); err == nil && fi.IsDir() {
		return FirmwareUEFI
	}
	return FirmwareBIOS
}

func collectMounts(info *SystemInfo) []Mount {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		info.record("mounts", err)
		return nil
	}
	defer f.Close()

	mounts, err := parseMountinfo(f)
	if err != nil {
		info.record("mounts", err)
		return nil
	}
	// Only the mounts that describe this system's storage are kept; see
	// diskMounts. The pseudo filesystems are not facts a restore can use, and
	// they are the bulk of the table.
	return diskMounts(mounts)
}

func collectFstab(info *SystemInfo) Fstab {
	b, err := readFileLimit(fstabPath)
	if err != nil {
		info.record("fstab", err)
		return Fstab{}
	}
	out, err := parseFstab(bytes.NewReader(b))
	if err != nil {
		info.record("fstab", err)
		return Fstab{}
	}
	return out
}

// collectNetplan reads the network configuration verbatim. The contents may
// hold secrets and are never logged.
func collectNetplan(info *SystemInfo) map[string]string {
	entries, err := os.ReadDir(netplanDir)
	if err != nil {
		info.record("netplan", err)
		return nil
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if ext := filepath.Ext(name); ext != ".yaml" && ext != ".yml" {
			continue
		}
		b, err := readFileLimit(filepath.Join(netplanDir, name))
		if err != nil {
			info.record("netplan:"+name, err)
			continue
		}
		out[name] = string(b)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveMountSources returns the device-backed mounts with each source
// replaced by the device its number names. A mount whose number cannot be
// resolved is left out and recorded: its source string may be an alias, and
// guessing from it is how "/dev/root" reached sfdisk.
func resolveMountSources(info *SystemInfo, mounts []Mount, resolve DeviceResolver) []Mount {
	var out []Mount
	for _, m := range mounts {
		if !strings.HasPrefix(m.Source, "/dev/") {
			continue
		}
		device, err := resolve.Resolve(m.DevNo)
		if err != nil {
			info.record("device:"+m.MountPoint, fmt.Errorf("resolve the device of %s (%s): %w", m.MountPoint, m.Source, err))
			continue
		}
		m.Source = device
		out = append(out, m)
	}
	return out
}

// collectBlockDevices runs lsblk for every column it knows. This supersedes
// blkid: the output already carries UUID, PARTUUID, FSTYPE and LABEL.
func collectBlockDevices(ctx context.Context, runner Runner, info *SystemInfo) json.RawMessage {
	out, err := runner.Run(ctx, "lsblk", "--json", "--output-all", "--bytes")
	if err != nil {
		info.record("lsblk", err)
		return nil
	}
	if !json.Valid(out) {
		info.record("lsblk", errors.New("lsblk did not return valid JSON"))
		return nil
	}
	return json.RawMessage(out)
}

// collectPartitionTables dumps the partition table of every disk backing a
// mount, in the format sfdisk itself reads back.
//
// Disks are found from each mount's device number, not its source string: the
// mount table can name a device by an alias. Cloud images that boot without an
// initramfs mount the root filesystem as "/dev/root", a name no device has, and
// reducing that to a disk would ask sfdisk about a device that does not exist
// while the real root disk went unrecorded.
func collectPartitionTables(ctx context.Context, runner Runner, info *SystemInfo, mounts []Mount, resolve DeviceResolver) map[string]string {
	disks := diskDevices(resolveMountSources(info, mounts, resolve))
	if len(disks) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, disk := range disks {
		dump, err := runner.Run(ctx, "sfdisk", "--dump", disk)
		if err != nil {
			info.record("sfdisk:"+disk, err)
			continue
		}
		out[disk] = string(dump)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// collectLVM reports the LVM configuration when the system has one. A machine
// without LVM has no lvm binary and no volume groups; both are normal and
// neither is recorded as an error.
func collectLVM(ctx context.Context, runner Runner, info *SystemInfo) *LVM {
	report, err := runner.Run(ctx, "lvm", "fullreport", "--reportformat", "json")
	if err != nil {
		info.record("lvm", err)
		return nil
	}
	if !json.Valid(report) {
		info.record("lvm", errors.New("lvm fullreport did not return valid JSON"))
		return nil
	}

	lvm := &LVM{FullReport: json.RawMessage(report)}
	for _, vg := range volumeGroupNames(report) {
		cfg, err := volumeGroupConfig(ctx, runner, vg)
		if err != nil {
			info.record("vgcfgbackup:"+vg, err)
			continue
		}
		if lvm.VGConfigs == nil {
			lvm.VGConfigs = map[string]string{}
		}
		lvm.VGConfigs[vg] = cfg
	}
	return lvm
}

// volumeGroupConfig returns one volume group's configuration in the form
// vgcfgrestore accepts.
//
// vgcfgbackup has no standard-output mode: -f names a file, and it writes only
// a success line to standard output. The configuration is therefore taken
// through a temporary file of our own rather than the default location under
// /etc/lvm, so that collecting a backup does not disturb the source system's
// own LVM backups.
func volumeGroupConfig(ctx context.Context, runner Runner, vg string) (string, error) {
	dir, err := os.MkdirTemp("", "aiha-vgcfg-")
	if err != nil {
		return "", fmt.Errorf("create a temporary directory for the %s configuration: %w", vg, err)
	}
	defer os.RemoveAll(dir)

	// vgcfgbackup expands "%s" in the file name to the volume group's name.
	// The directory comes from MkdirTemp and holds no percent sign, and one
	// group is named per run, so a fixed name is unambiguous.
	path := filepath.Join(dir, "vg.cfg")
	if _, err := runner.Run(ctx, "vgcfgbackup", "-f", path, vg); err != nil {
		return "", err
	}

	b, err := readFileLimit(path)
	if err != nil {
		return "", fmt.Errorf("read the %s configuration: %w", vg, err)
	}
	return string(b), nil
}

// readFileLimit reads a file, refusing one larger than maxFileBytes so that a
// stray huge file cannot bloat the manifest.
func readFileLimit(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Read one byte past the limit so that hitting it is distinguishable from
	// a file that happens to be exactly that size.
	b, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(b) > maxFileBytes {
		return nil, fmt.Errorf("read %s: larger than %d bytes", path, maxFileBytes)
	}
	return b, nil
}
