//go:build linux

package sysinfo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// fakeRunner answers from a fixed table, so collection can be driven through
// the cases that matter without the host's real tools.
type fakeRunner struct {
	out   map[string][]byte
	err   map[string]error
	calls []string
	// before, when set, answers first. It receives the arguments, which the
	// table does not, so that a tool whose real output is a file rather than
	// its standard output can be imitated.
	before func(name string, args ...string) ([]byte, error)
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := name
	if len(args) > 0 {
		key = fmt.Sprintf("%s %v", name, args)
	}
	f.calls = append(f.calls, key)
	if err, ok := f.err[name]; ok {
		return nil, err
	}
	if f.before != nil {
		out, err := f.before(name, args...)
		if err != nil || out != nil {
			return out, err
		}
	}
	if out, ok := f.out[name]; ok {
		return out, nil
	}
	return nil, fmt.Errorf("%s: %w", name, ErrCommandNotFound)
}

// A system without LVM has no lvm binary. That is normal, not a failure: the
// backup must proceed and nothing may be recorded as an error.
func TestCollectTreatsMissingToolsAsAbsentSubsystems(t *testing.T) {
	runner := &fakeRunner{
		out: map[string][]byte{
			"lsblk": []byte(`{"blockdevices":[{"name":"nvme0n1","uuid":null}]}`),
		},
		err: map[string]error{
			"lvm":    fmt.Errorf("lvm: %w", ErrCommandNotFound),
			"sfdisk": fmt.Errorf("sfdisk: %w", ErrCommandNotFound),
		},
	}

	info, err := Collect(context.Background(), runner)
	if err != nil {
		t.Fatalf("Collect returned an error for a system missing optional tools: %v", err)
	}
	if info.LVM != nil {
		t.Errorf("LVM reported on a system with no lvm binary: %+v", info.LVM)
	}
	for _, e := range info.Errors {
		if e.Item == "lvm" || e.Item == "sfdisk" {
			t.Errorf("a missing tool was recorded as an error: %+v", e)
		}
	}
	if len(info.BlockDevices) == 0 {
		t.Error("lsblk output was not captured")
	}
}

// A tool that exists but fails is a real problem and is recorded — but it
// still must not fail the backup.
func TestCollectRecordsToolFailuresWithoutFailing(t *testing.T) {
	runner := &fakeRunner{
		err: map[string]error{
			"lsblk": errors.New("lsblk: permission denied"),
		},
	}

	info, err := Collect(context.Background(), runner)
	if err != nil {
		t.Fatalf("Collect failed because a tool failed: %v", err)
	}
	if info.BlockDevices != nil {
		t.Error("block devices reported despite lsblk failing")
	}
	var found bool
	for _, e := range info.Errors {
		if e.Item == "lsblk" {
			found = true
		}
	}
	if !found {
		t.Errorf("lsblk failure not recorded: %+v", info.Errors)
	}
}

// Invalid JSON must not reach the manifest: a restore would fail on it later,
// far from the cause.
func TestCollectRejectsInvalidJSON(t *testing.T) {
	runner := &fakeRunner{
		out: map[string][]byte{"lsblk": []byte("not json at all")},
	}

	info, err := Collect(context.Background(), runner)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if info.BlockDevices != nil {
		t.Errorf("invalid JSON stored as block devices: %s", info.BlockDevices)
	}
	if len(info.Errors) == 0 {
		t.Error("invalid lsblk JSON was not recorded as an error")
	}
}

// vgcfgbackup writes a file and prints only a success line. A collector that
// kept its standard output would fill every manifest with "Volume group ...
// successfully backed up." where the configuration that vgcfgrestore needs
// should be — a backup that looks complete and cannot rebuild the volume
// group it describes.
func TestCollectCapturesTheVolumeGroupConfigurationNotItsSuccessMessage(t *testing.T) {
	const vgConfig = `ubuntu-vg {
	id = "aaaaaa-bbbb-cccc-dddd-eeee-ffff-000000"
	seqno = 2
	physical_volumes { pv0 { device = "/dev/sda3" } }
}
`
	var wrote string
	runner := &fakeRunner{
		out: map[string][]byte{
			"lvm": []byte(`{"report":[{"vg":[{"vg_name":"ubuntu-vg"}]}]}`),
		},
		// vgcfgbackup names the output file with -f and reports success on
		// standard output; the fake does the same.
		before: func(name string, args ...string) ([]byte, error) {
			if name != "vgcfgbackup" {
				return nil, nil
			}
			if len(args) != 3 || args[0] != "-f" || args[2] != "ubuntu-vg" {
				return nil, fmt.Errorf("unexpected vgcfgbackup args: %v", args)
			}
			wrote = args[1]
			if err := os.WriteFile(wrote, []byte(vgConfig), 0o600); err != nil {
				return nil, err
			}
			return []byte(`  Volume group "ubuntu-vg" successfully backed up.` + "\n"), nil
		},
	}

	info, err := Collect(context.Background(), runner)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if info.LVM == nil {
		t.Fatal("no LVM section collected")
	}
	if got := info.LVM.VGConfigs["ubuntu-vg"]; got != vgConfig {
		t.Errorf("VGConfigs[ubuntu-vg] = %q,\nwant the configuration itself", got)
	}
	if len(info.Errors) != 0 {
		t.Errorf("errors recorded for a clean collection: %+v", info.Errors)
	}

	// The temporary file is ours, so it has to go. The old "-f -" spelling left
	// a file literally named "-" in the agent's working directory.
	if wrote == "" {
		t.Fatal("vgcfgbackup was never asked for a file")
	}
	if _, err := os.Stat(filepath.Dir(wrote)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the temporary directory %s outlived collection", filepath.Dir(wrote))
	}
	if _, err := os.Stat("-"); err == nil {
		t.Error(`a file named "-" was created in the working directory`)
	}
}

// A volume group whose configuration cannot be captured is recorded and the
// rest of the collection proceeds, as every other item does.
func TestCollectRecordsAFailedVolumeGroupBackup(t *testing.T) {
	runner := &fakeRunner{
		out: map[string][]byte{
			"lvm": []byte(`{"report":[{"vg":[{"vg_name":"ubuntu-vg"}]}]}`),
		},
		err: map[string]error{"vgcfgbackup": errors.New("vgcfgbackup: volume group not found")},
	}

	info, err := Collect(context.Background(), runner)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if info.LVM == nil || len(info.LVM.FullReport) == 0 {
		t.Error("the full report was dropped because one volume group failed")
	}
	if len(info.LVM.VGConfigs) != 0 {
		t.Errorf("a failed capture stored something: %+v", info.LVM.VGConfigs)
	}
	var found bool
	for _, e := range info.Errors {
		if e.Item == "vgcfgbackup:ubuntu-vg" {
			found = true
		}
	}
	if !found {
		t.Errorf("the failure was not recorded: %+v", info.Errors)
	}
}

// Collection runs against the host, which is a Linux machine in CI and in
// development. It must produce the facts that do not depend on privileges.
func TestCollectOnHost(t *testing.T) {
	info, err := Collect(context.Background(), &fakeRunner{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if info.Kernel.Release == "" {
		t.Error("kernel release not collected")
	}
	if info.Firmware != FirmwareUEFI && info.Firmware != FirmwareBIOS {
		t.Errorf("firmware = %q", info.Firmware)
	}
	if len(info.Mounts) == 0 {
		t.Error("no mounts collected from /proc/self/mountinfo")
	}
	if info.CollectedAt.IsZero() {
		t.Error("CollectedAt not set")
	}
	t.Logf("kernel=%s firmware=%s mounts=%d osID=%s",
		info.Kernel.Release, info.Firmware, len(info.Mounts), info.OSRelease.ID)
}

// TestCollectPartitionTablesResolvesRootAlias covers an Ubuntu cloud image,
// which mounts its root as "/dev/root": the disk must be found from the
// mount's device number, not reduced from the alias.
func TestCollectPartitionTablesResolvesRootAlias(t *testing.T) {
	mounts := []Mount{
		{MountPoint: "/", Source: "/dev/root", FSType: "ext4", DevNo: "259:1"},
		{MountPoint: "/boot", Source: "/dev/nvme0n1p16", FSType: "ext4", DevNo: "259:3"},
		{MountPoint: "/boot/efi", Source: "/dev/nvme0n1p15", FSType: "vfat", DevNo: "259:2"},
		{MountPoint: "/data", Source: "/dev/vanished", FSType: "xfs", DevNo: "8:17"},
		{MountPoint: "/run", Source: "tmpfs", FSType: "tmpfs", DevNo: "0:25"},
	}
	resolve := fakeResolver{"259:1": "/dev/nvme0n1p1", "259:2": "/dev/nvme0n1p15", "259:3": "/dev/nvme0n1p16"}
	runner := &fakeRunner{out: map[string][]byte{"sfdisk": []byte("label: gpt\n")}}
	info := &SystemInfo{}

	got := collectPartitionTables(context.Background(), runner, info, mounts, resolve)

	if len(got) != 1 || got["/dev/nvme0n1"] != "label: gpt\n" {
		t.Fatalf("partition tables = %v, want only /dev/nvme0n1", got)
	}
	for _, call := range runner.calls {
		if call != "sfdisk [--dump /dev/nvme0n1]" {
			t.Fatalf("sfdisk asked about something else: %v", runner.calls)
		}
	}
	// The mount whose number names no device is recorded, not guessed at.
	if len(info.Errors) != 1 || info.Errors[0].Item != "device:/data" {
		t.Fatalf("errors = %+v, want one for /data", info.Errors)
	}
}
