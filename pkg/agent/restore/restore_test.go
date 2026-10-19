package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/agent/rescue"
	"github.com/cloud-barista/ai-ha/pkg/agent/state"
	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	backupmodel "github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/sysinfo"
)

const (
	testSystemID = "web01-3f2a9c1e"
	testSetID    = "9c0d1e2f3a4b5c6d"
	sourceMAC    = "0a:1b:2c:3d:4e:5f"
	targetMAC    = "02:aa:bb:cc:dd:ee"
	espVolumeID  = "AAAA-0001"
	rootPartUUID = "5f1c0b2a-0000-4000-8000-000000000001"
	// strangerUUID is on no disk of the test machine.
	strangerUUID = "deadbeef-dead-beef-dead-beefdeadbeef"
)

// partition describes one partition of the test system, an Ubuntu cloud image
// with separate /boot and EFI system partitions.
type partition struct {
	device, mountPoint, fsType, label, uuid, partUUID, devNo string
	size                                                     int64
}

var testPartitions = []partition{
	{"/dev/nvme0n1p1", "/", "ext4", "cloudimg-rootfs", oldRootUUID, rootPartUUID, "259:1", 3000},
	{"/dev/nvme0n1p16", "/boot", "ext4", "BOOT", oldBootUUID, "5f1c0b2a-0000-4000-8000-000000000016", "259:16", 200},
	{"/dev/nvme0n1p15", "/boot/efi", "vfat", "UEFI", espVolumeID, "5f1c0b2a-0000-4000-8000-000000000015", "259:15", 100},
}

// sourceDevice is where the partition was on the source. The source had one
// more NVMe device than the target, so its root disk was nvme1n1 where the
// target's is nvme0n1 — the case that defeats matching by device name.
func sourceDevice(p partition) string {
	return strings.Replace(p.device, "nvme0n1", "nvme1n1", 1)
}

func fileName(p partition) string {
	return "web01-" + filepath.Base(p.device) + "-1a2b3c4d.aha"
}

// testManifest is the manifest the backup of testPartitions would write.
func testManifest(t *testing.T) backupmodel.Manifest {
	t.Helper()
	info := sysinfo.SystemInfo{
		Firmware:     sysinfo.FirmwareUEFI,
		NetplanFiles: map[string]string{"50-cloud-init.yaml": "match:\n  macaddress: " + strings.ToUpper(sourceMAC) + "\n"},
	}
	var children []map[string]string
	set := backupmodel.BackupSet{ID: testSetID, SystemID: testSystemID, Status: backupmodel.BackupCompleted}
	for _, p := range testPartitions {
		src := sourceDevice(p)
		info.Mounts = append(info.Mounts, sysinfo.Mount{MountPoint: p.mountPoint, Source: src, FSType: p.fsType, Root: "/", DevNo: p.devNo})
		children = append(children, map[string]string{"name": filepath.Base(src), "path": src, "maj:min": p.devNo,
			"uuid": p.uuid, "partuuid": p.partUUID, "label": p.label})
		set.Items = append(set.Items, backupmodel.BackupItem{
			Partition: filepath.Base(src), Device: src, MountPoint: p.mountPoint,
			FileName: fileName(p), Compression: backupmodel.CompressionZstd, SizeBytes: p.size,
			Status: backupmodel.BackupCompleted,
		})
	}
	blk, err := json.Marshal(map[string]any{"blockdevices": []any{map[string]any{"name": "nvme1n1", "maj:min": "259:0", "children": children}}})
	if err != nil {
		t.Fatal(err)
	}
	info.BlockDevices = blk
	sys, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	return backupmodel.Manifest{Version: backupmodel.ManifestVersion, Set: set, System: sys}
}

// fakeRunner plays the rescue environment's tools.
type fakeRunner struct {
	mu       sync.Mutex
	manifest []byte
	mntDir   string
	mounted  bool
	// mountedBefore lists devices mounted before the restore starts.
	mountedBefore map[string]string
	// blkidUUID overrides the UUID blkid reports for a device.
	blkidUUID map[string]string
	// hidePartUUID leaves a partition out of the PARTUUID listing, and
	// extraPartitions adds lines to it.
	hidePartUUID    string
	extraPartitions string
	calls           [][]string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	last := ""
	if len(args) > 0 {
		last = args[len(args)-1]
	}
	switch name {
	case "rclone":
		if strings.HasSuffix(args[1], backupmodel.ManifestName(testSetID)) {
			return f.manifest, nil
		}
		return nil, fmt.Errorf("no object %s", args[1])
	case "lsblk":
		if slices.Contains(args, "PKNAME") {
			return []byte("nvme0n1\n"), nil
		}
		if slices.Contains(args, "PATH,PARTUUID") {
			var b strings.Builder
			b.WriteString("/dev/nvme0n1\n")
			for _, p := range testPartitions {
				if f.hidePartUUID == p.partUUID {
					continue
				}
				fmt.Fprintf(&b, "%s %s\n", p.device, strings.ToUpper(p.partUUID))
			}
			return []byte(b.String() + f.extraPartitions), nil
		}
		if slices.Contains(args, "UUID,PARTUUID") {
			var b strings.Builder
			for _, p := range testPartitions {
				fmt.Fprintf(&b, "%s %s\n", strings.ToUpper(p.uuid), p.partUUID)
			}
			return []byte(b.String()), nil
		}
		if m, ok := f.mountedBefore[last]; ok {
			return []byte(m + "\n"), nil
		}
		if f.mounted {
			return []byte(filepath.Join(f.mntDir, filepath.Base(last)) + "\n"), nil
		}
		return []byte("\n"), nil
	case "blkid":
		if uuid, ok := f.blkidUUID[last]; ok {
			return []byte(uuid + "\n"), nil
		}
		for _, p := range testPartitions {
			if p.device == last {
				return []byte(p.uuid + "\n"), nil
			}
		}
		return nil, errors.New("no such device")
	case "mountall":
		f.mounted = true
	}
	return nil, nil
}

func (f *fakeRunner) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c[0])
	}
	return out
}

func (f *fakeRunner) find(name string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c[0] == name {
			out = append(out, c)
		}
	}
	return out
}

// fakeReporter records what the restore reports.
type fakeReporter struct {
	mu       sync.Mutex
	progress []model.RestoreProgressReq
	results  []model.RestoreResultReq
}

func (r *fakeReporter) Progress(_ context.Context, req model.RestoreProgressReq) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.progress = append(r.progress, req)
	return nil
}

func (r *fakeReporter) Result(_ context.Context, req model.RestoreResultReq) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, req)
	return nil
}

// restoredFiles is what extracting each backup file leaves behind: the
// configuration of an Ubuntu cloud image, naming its disks by the UUIDs the
// restore preserves. With stale set, every one of those names is wrong.
func restoredFiles(p partition, stale bool) map[string]string {
	bootUUID, partUUID := oldBootUUID, rootPartUUID
	if stale {
		bootUUID, partUUID = strangerUUID, strangerUUID
	}
	switch p.mountPoint {
	case "/":
		return map[string]string{
			"etc/fstab": "LABEL=cloudimg-rootfs / ext4 defaults 0 1\n" +
				"UUID=" + bootUUID + " /boot ext4 defaults 0 2\n" +
				"UUID=" + espVolumeID + " /boot/efi vfat umask=0077 0 1\n",
			"etc/netplan/50-cloud-init.yaml":              "match:\n  macaddress: " + sourceMAC + "\n",
			"etc/default/grub":                            "GRUB_CMDLINE_LINUX_DEFAULT=\"console=tty1 root=UUID=" + oldRootUUID + "\"\n",
			"etc/default/grub.d/40-force-partuuid.cfg":    "GRUB_FORCE_PARTUUID=" + partUUID + "\n",
			"etc/default/grub.d/50-cloudimg-settings.cfg": "GRUB_TIMEOUT=0\n",
		}
	case "/boot/efi":
		return map[string]string{
			"EFI/ubuntu/grub.cfg": "search.fs_uuid " + bootUUID + " root\nset prefix=($root)'/grub'\nconfigfile $prefix/grub.cfg\n",
		}
	}
	return map[string]string{"grub/grub.cfg": "menuentry\n"}
}

// fakeExtract writes restoredFiles into dest and counts the file's size as read.
func fakeExtract(fail string) extractFunc { return fakeExtractConfig(fail, false) }

func fakeExtractConfig(fail string, stale bool) extractFunc {
	return func(_ context.Context, src source, dest string, read *atomic.Int64) error {
		for _, p := range testPartitions {
			if !strings.HasSuffix(src.rcloneArgs[1], fileName(p)) {
				continue
			}
			if fail == p.mountPoint {
				read.Add(p.size / 2)
				return errors.New("unpack: exit status 2: tar: Unexpected EOF in archive")
			}
			for name, content := range restoredFiles(p, stale) {
				path := filepath.Join(dest, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					return err
				}
			}
			read.Add(p.size)
			return nil
		}
		return fmt.Errorf("unexpected source %v", src.rcloneArgs)
	}
}

// testEnv is a rescue environment with one network interface.
type testEnv struct {
	opts     Options
	runner   *fakeRunner
	reporter *fakeReporter
	mntDir   string
}

func newTestEnv(t *testing.T, uefi bool) *testEnv {
	t.Helper()
	sysRoot, mntDir := t.TempDir(), t.TempDir()
	for _, nic := range []struct{ name, mac string }{{"ens5", targetMAC}, {"lo", "00:00:00:00:00:00"}} {
		dir := filepath.Join(sysRoot, "sys", "class", "net", nic.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "address"), []byte(nic.mac+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if nic.name != "lo" {
			if err := os.Mkdir(filepath.Join(dir, "device"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	if uefi {
		if err := os.MkdirAll(filepath.Join(sysRoot, "sys", "firmware", "efi"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := json.Marshal(testManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{manifest: manifest, mntDir: mntDir}
	reporter := &fakeReporter{}
	return &testEnv{
		runner:   runner,
		reporter: reporter,
		mntDir:   mntDir,
		opts: Options{
			Staged: rescue.Staged{
				RestoreID: "9b1c2d3e", RecoveryID: "1a2b3c4d",
				Plan:         model.RestorePlan{SourceSystemID: testSystemID, SourceBackupSetID: testSetID},
				Repository:   rescue.Repository{Type: "s3", Provider: "Minio", Bucket: "aiha-backup", Prefix: "prod"},
				RcloneFile:   "/run/aiha/1a2b3c4d/rclone.conf",
				RcloneRemote: "aiha",
			},
			Reporter:       reporter,
			runner:         runner,
			extract:        fakeExtract(""),
			lookPath:       func(string) (string, error) { return "/usr/bin/x", nil },
			isBlockDevice:  func(string) error { return nil },
			sysRoot:        sysRoot,
			mntDir:         mntDir,
			reportInterval: time.Hour,
			now:            func() time.Time { return time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC) },
		},
	}
}

func (e *testEnv) read(t *testing.T, mountPoint, name string) string {
	t.Helper()
	for _, p := range testPartitions {
		if p.mountPoint == mountPoint {
			data, err := os.ReadFile(filepath.Join(e.mntDir, filepath.Base(p.device), name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			return string(data)
		}
	}
	t.Fatalf("no partition at %s", mountPoint)
	return ""
}

func TestRunRestoresSet(t *testing.T) {
	env := newTestEnv(t, true)
	res, err := Run(context.Background(), env.opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Partitions != 3 || res.Firmware != sysinfo.FirmwareUEFI || res.Root != filepath.Join(env.mntDir, "nvme0n1p1") {
		t.Fatalf("result = %+v", res)
	}

	// The manifest is read from the source system's directory.
	manifestCall := env.runner.find("rclone")[0]
	if want := "aiha:aiha-backup/prod/" + testSystemID + "/" + testSetID + "-manifest.json"; manifestCall[2] != want {
		t.Fatalf("manifest read from %s, want %s", manifestCall[2], want)
	}

	// Formatted shallowest first with the source's labels and UUIDs.
	mkfs := append(env.runner.find("mkfs.ext4"), env.runner.find("mkfs.vfat")...)
	wantMkfs := [][]string{
		{"mkfs.ext4", "-F", "-q", "-U", oldRootUUID, "-L", "cloudimg-rootfs", "/dev/nvme0n1p1"},
		{"mkfs.ext4", "-F", "-q", "-U", oldBootUUID, "-L", "BOOT", "/dev/nvme0n1p16"},
		{"mkfs.vfat", "-i", "AAAA0001", "-n", "UEFI", "/dev/nvme0n1p15"},
	}
	if fmt.Sprint(mkfs) != fmt.Sprint(wantMkfs) {
		t.Fatalf("mkfs calls = %v, want %v", mkfs, wantMkfs)
	}

	// Phases run in order: every format before mountall, every chroot step
	// after the files are synced and the tree is bound together.
	cmds := env.runner.commands()
	lastIndex := func(name string) int {
		i := -1
		for j, c := range cmds {
			if c == name {
				i = j
			}
		}
		return i
	}
	for _, order := range [][2]string{{"mkfs.vfat", "mountall"}, {"mountall", "sync"}, {"sync", "mount"}, {"mount", "arch-chroot"}, {"arch-chroot", "umount"}} {
		if lastIndex(order[0]) > slices.Index(cmds, order[1]) {
			t.Fatalf("%s ran after %s: %v", order[0], order[1], cmds)
		}
	}

	// The UUIDs were preserved, so the configuration naming them is left as
	// it was restored; only netplan is adapted to this machine.
	for _, f := range []struct{ mountPoint, name string }{
		{"/", "etc/fstab"},
		{"/", "etc/default/grub"},
		{"/", "etc/default/grub.d/40-force-partuuid.cfg"},
		{"/boot/efi", "EFI/ubuntu/grub.cfg"},
	} {
		var p partition
		for _, tp := range testPartitions {
			if tp.mountPoint == f.mountPoint {
				p = tp
			}
		}
		if got := env.read(t, f.mountPoint, f.name); got != restoredFiles(p, false)[f.name] {
			t.Fatalf("%s changed:\n%s", f.name, got)
		}
	}
	if got := env.read(t, "/", "etc/netplan/50-cloud-init.yaml"); !strings.Contains(got, targetMAC) || strings.Contains(got, sourceMAC) {
		t.Fatalf("netplan:\n%s", got)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %q", res.Warnings)
	}
	marker, err := state.ReadRecoveryPending(filepath.Join(env.mntDir, "nvme0n1p1", state.Dir))
	if err != nil || marker == nil || marker.RecoveryID != "1a2b3c4d" {
		t.Fatalf("recovery marker = %+v, %v", marker, err)
	}

	// /boot then /boot/efi are bound into the root before the chroot.
	root := filepath.Join(env.mntDir, "nvme0n1p1")
	binds := env.runner.find("mount")
	if len(binds) != 2 || binds[0][3] != filepath.Join(root, "boot") || binds[1][3] != filepath.Join(root, "boot/efi") {
		t.Fatalf("bind mounts = %v", binds)
	}
	chroot := env.runner.find("arch-chroot")
	want := [][]string{
		{"arch-chroot", root, "grub-install", "--target=x86_64-efi", "--efi-directory=/boot/efi"},
		{"arch-chroot", root, "update-grub"},
		{"arch-chroot", root, "update-initramfs", "-u", "-k", "all"},
	}
	if fmt.Sprint(chroot) != fmt.Sprint(want) {
		t.Fatalf("chroot calls = %v, want %v", chroot, want)
	}
	if umounts := env.runner.find("umount"); len(umounts) != 2 || umounts[0][1] != filepath.Join(root, "boot/efi") {
		t.Fatalf("unmounts = %v", umounts)
	}

	// Reports never go backwards, end on the last step with every byte of the
	// set read, and the result follows. A phase that passes quicker than a
	// report is sent may be skipped: only the latest position matters.
	var last model.RestoreProgressReq
	for i, p := range env.reporter.progress {
		if p.Seq <= last.Seq || p.Step < last.Step {
			t.Fatalf("report %d went backwards: %+v after %+v", i, p, last)
		}
		last = p
	}
	if last.ProcessedBytes != 3300 || last.TotalBytes != 3300 || last.Step != 6 || last.TotalSteps != 6 {
		t.Fatalf("final progress = %+v", last)
	}
	if len(env.reporter.results) != 1 || env.reporter.results[0].Outcome != "succeeded" {
		t.Fatalf("results = %+v", env.reporter.results)
	}
}

func TestRunInstallsBIOSBootloader(t *testing.T) {
	env := newTestEnv(t, false)
	if _, err := Run(context.Background(), env.opts); err != nil {
		t.Fatalf("run: %v", err)
	}
	install := env.runner.find("arch-chroot")[0]
	if !slices.Equal(install[2:], []string{"grub-install", "--target=i386-pc", "/dev/nvme0n1"}) {
		t.Fatalf("grub-install = %v", install)
	}
}

func TestRunStopsBeforeFormatting(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, e *testEnv)
		wantMsg string
	}{
		{"manifest of another set", func(t *testing.T, e *testEnv) {
			e.opts.Staged.Plan.SourceBackupSetID = "0000000000000000"
		}, "Manifest of backup set 0000000000000000 could not be read from the repository"},
		{"incomplete partition", func(t *testing.T, e *testEnv) {
			m := testManifest(t)
			m.Set.Items[1].Status = backupmodel.BackupFailed
			e.runner.manifest, _ = json.Marshal(m)
		}, "Backup set " + testSetID + " cannot be restored"},
		{"no UUID recorded", func(t *testing.T, e *testEnv) {
			m := testManifest(t)
			info, _ := decodeSystem(m.System)
			info.BlockDevices = json.RawMessage(`{"blockdevices":[]}`)
			m.System, _ = json.Marshal(info)
			e.runner.manifest, _ = json.Marshal(m)
		}, "Partition layout of backup set " + testSetID + " is not supported"},
		{"no root", func(t *testing.T, e *testEnv) {
			m := testManifest(t)
			m.Set.Items = m.Set.Items[1:]
			e.runner.manifest, _ = json.Marshal(m)
		}, "Partition layout of backup set " + testSetID + " is not supported"},
		{"missing tool", func(t *testing.T, e *testEnv) {
			e.opts.lookPath = func(name string) (string, error) {
				if name == "arch-chroot" {
					return "", errors.New("not found")
				}
				return "/usr/bin/" + name, nil
			}
		}, "Rescue environment lacks a required tool"},
		{"partition in use", func(t *testing.T, e *testEnv) {
			e.runner.mountedBefore = map[string]string{"/dev/nvme0n1p16": "/mnt/nvme0n1p16"}
		}, "Partition /dev/nvme0n1p16 is in use: unmount it before restoring"},
		{"no partition with the PARTUUID", func(t *testing.T, e *testEnv) {
			e.runner.hidePartUUID = testPartitions[1].partUUID
		}, "Partitions of backup set " + testSetID + " cannot be matched to this machine"},
		{"PARTUUID on two partitions", func(t *testing.T, e *testEnv) {
			e.runner.extraPartitions = "/dev/nvme2n1p1 " + rootPartUUID + "\n"
		}, "Partitions of backup set " + testSetID + " cannot be matched to this machine"},
		{"partition missing", func(t *testing.T, e *testEnv) {
			e.opts.isBlockDevice = func(d string) error {
				if d == "/dev/nvme0n1p15" {
					return os.ErrNotExist
				}
				return nil
			}
		}, "Partition /dev/nvme0n1p15 for /boot/efi not found on this machine"},
		{"two network interfaces", func(t *testing.T, e *testEnv) {
			dir := filepath.Join(e.opts.sysRoot, "sys", "class", "net", "ens6")
			_ = os.MkdirAll(filepath.Join(dir, "device"), 0o755)
			_ = os.WriteFile(filepath.Join(dir, "address"), []byte("02:00:00:00:00:01\n"), 0o644)
		}, "Network interface of the source cannot be matched to this machine"},
		{"UEFI without an EFI system partition", func(t *testing.T, e *testEnv) {
			m := testManifest(t)
			m.Set.Items = m.Set.Items[:2]
			e.runner.manifest, _ = json.Marshal(m)
		}, "This machine boots with UEFI but the backup set has no EFI system partition"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, true)
			tt.setup(t, env)
			if _, err := Run(context.Background(), env.opts); err == nil {
				t.Fatal("run succeeded")
			}
			for _, c := range env.runner.commands() {
				if strings.HasPrefix(c, "mkfs.") || c == "mountall" {
					t.Fatalf("%s ran although the restore stopped while preparing", c)
				}
			}
			results := env.reporter.results
			if len(results) != 1 || results[0].Outcome != "failed" || results[0].Phase != PhasePreparing || results[0].Message != tt.wantMsg {
				t.Fatalf("results = %+v, want failed in preparing with %q", results, tt.wantMsg)
			}
		})
	}
}

func TestRunReportsFailureWithoutToolOutput(t *testing.T) {
	env := newTestEnv(t, true)
	env.opts.extract = fakeExtract("/boot")
	_, err := Run(context.Background(), env.opts)
	if err == nil || !strings.Contains(err.Error(), "Unexpected EOF") {
		t.Fatalf("err = %v, want the tar failure", err)
	}
	results := env.reporter.results
	want := "Restoring web01-nvme0n1p16-1a2b3c4d.aha onto /dev/nvme0n1p16 failed"
	if len(results) != 1 || results[0].Outcome != "failed" || results[0].Phase != PhaseExtracting || results[0].Message != want {
		t.Fatalf("results = %+v", results)
	}
	if len(env.runner.find("arch-chroot")) != 0 {
		t.Fatal("the bootloader was installed after a failed extraction")
	}
}

// failingReporter refuses the result.
type failingReporter struct{ fakeReporter }

func (r *failingReporter) Result(context.Context, model.RestoreResultReq) error {
	return rescue.ErrReportRefused
}

func TestRunFailsWhenResultIsNotDelivered(t *testing.T) {
	env := newTestEnv(t, true)
	env.opts.Reporter = &failingReporter{}
	if _, err := Run(context.Background(), env.opts); !errors.Is(err, rescue.ErrReportRefused) {
		t.Fatalf("err = %v, want the undelivered result", err)
	}
}

func TestRunWarnsAboutStaleConfiguration(t *testing.T) {
	env := newTestEnv(t, true)
	env.opts.extract = fakeExtractConfig("", true)
	res, err := Run(context.Background(), env.opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Warnings) != 3 {
		t.Fatalf("warnings = %q, want fstab, grub defaults and the EFI stub", res.Warnings)
	}
	for _, want := range []string{"/etc/fstab", "GRUB_FORCE_PARTUUID", "EFI grub stub"} {
		if !strings.Contains(strings.Join(res.Warnings, "\n"), want) {
			t.Fatalf("no warning mentions %s: %q", want, res.Warnings)
		}
	}
	// Warned about, not changed, and the restore carried on to the end.
	root := testPartitions[0]
	if got := env.read(t, "/", "etc/fstab"); got != restoredFiles(root, true)["etc/fstab"] {
		t.Fatalf("fstab changed:\n%s", got)
	}
	if len(env.runner.find("arch-chroot")) != 3 {
		t.Fatal("the bootloader was not installed after warnings")
	}
	results := env.reporter.results
	if len(results) != 1 || results[0].Outcome != "succeeded" || !strings.Contains(results[0].Message, "3 warning(s)") {
		t.Fatalf("results = %+v", results)
	}
}

func TestRunFailsWhenUUIDIsNotPreserved(t *testing.T) {
	env := newTestEnv(t, true)
	env.runner.blkidUUID = map[string]string{"/dev/nvme0n1p16": strangerUUID}
	if _, err := Run(context.Background(), env.opts); err == nil {
		t.Fatal("run succeeded with a filesystem under another UUID")
	}
	if slices.Contains(env.runner.commands(), "mountall") {
		t.Fatal("the restore went on after a UUID was not preserved")
	}
	results := env.reporter.results
	if len(results) != 1 || results[0].Phase != PhaseFormatting || results[0].Message != "UUID of /dev/nvme0n1p16 could not be preserved" {
		t.Fatalf("results = %+v", results)
	}
}
