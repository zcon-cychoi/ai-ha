// Package restore carries out a restore in the rescue environment: it reads
// the claimed backup set's manifest from the repository, recreates the
// filesystems, unpacks the backup files into them, adjusts the restored
// system's configuration to this machine and reinstalls its bootloader.
//
// The target is assumed to be built from the same image as the source, so its
// partitions are the source's: the partition table is left as it is and each
// partition is found by the PARTUUID the source recorded — device names follow
// detection order and differ between instances. Filesystems
// are recreated with the source's labels and UUIDs, so the restored
// configuration names them correctly as it is; the UUIDs it refers to are
// checked, and a mismatch is reported as a warning.
//
// Nothing is changed on disk until every check that can be made beforehand has
// passed. From the first format on, a failure leaves the disk unbootable; the
// restore stops there and reports where.
package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/agent/rescue"
	"github.com/cloud-barista/ai-ha/pkg/agent/state"
	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	backupmodel "github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/sysinfo"
	"github.com/rs/zerolog/log"
)

const (
	// commandTimeout bounds one command. mkfs on a large partition and
	// update-initramfs for several kernels take minutes; an hour means the
	// command is wedged.
	commandTimeout = time.Hour
	// resultTimeout bounds delivering the result once the restore has ended.
	resultTimeout = 10 * time.Minute
	// espMountPoint is where the EFI system partition is mounted.
	espMountPoint = "/boot/efi"
)

// Options configures Run.
type Options struct {
	// Staged is the claim the rescue agent staged: the plan, the repository
	// and the report token.
	Staged rescue.Staged
	// Reporter delivers progress and the result to the framework.
	Reporter Reporter

	// Test seams; zero values select the production behavior.
	runner         sysinfo.Runner
	extract        extractFunc
	lookPath       func(string) (string, error)
	isBlockDevice  func(string) error
	sysRoot        string
	mntDir         string
	reportInterval time.Duration
	now            func() time.Time
}

// Result summarizes a finished restore.
type Result struct {
	// Partitions is how many partitions were restored.
	Partitions int
	// Firmware is how the target boots, which decided the bootloader install.
	Firmware sysinfo.Firmware
	// Root is where the restored root filesystem is mounted.
	Root    string
	Elapsed time.Duration
	// Warnings are the checks of the restored configuration that failed. The
	// restore carries on past them; each may keep the system from booting.
	Warnings []string
}

// stepError is a failure with a message fit for the framework: it names what
// failed without the tool output underneath, which stays in the local log.
type stepError struct {
	msg string
	err error
}

func (e *stepError) Error() string { return e.msg + ": " + e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

func fail(err error, format string, args ...any) error {
	return &stepError{msg: fmt.Sprintf(format, args...), err: err}
}

// Run restores the staged backup set onto this machine and reports progress
// and the result to the framework. It does not reboot.
func Run(ctx context.Context, opts Options) (Result, error) {
	opts = withDefaults(opts)
	start := opts.now()
	t := newTracker()

	reportCtx, stopReports := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { reportLoop(reportCtx, opts.Reporter, t, opts.reportInterval) })

	res, err := run(ctx, opts, t)
	stopReports()
	wg.Wait()
	res.Elapsed = opts.now().Sub(start)

	// Reported even when ctx has ended: a restore cut short by its timeout is
	// exactly what the framework needs to hear about.
	resultCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resultTimeout)
	defer cancel()
	result := model.RestoreResultReq{Outcome: "succeeded", Phase: t.currentPhase()}
	if err != nil {
		result.Outcome = "failed"
		result.Message = publicMessage(err, t.currentPhase())
		log.Error().Err(err).Str("phase", result.Phase).Msg("Restore failed")
	} else {
		result.Message = fmt.Sprintf("Restored %d partition(s) (%.1fs)", res.Partitions, res.Elapsed.Seconds())
		if len(res.Warnings) > 0 {
			result.Message += fmt.Sprintf("; %d warning(s): %s", len(res.Warnings), strings.Join(res.Warnings, "; "))
		}
		log.Info().Int("partitions", res.Partitions).Int("warnings", len(res.Warnings)).
			Dur("elapsed", res.Elapsed).Msg("Restore finished")
	}
	if pErr := opts.Reporter.Progress(resultCtx, t.snapshot(t.nextSeq())); pErr != nil {
		log.Warn().Err(pErr).Msg("Could not send the final progress report")
	}
	if rErr := opts.Reporter.Result(resultCtx, result); rErr != nil {
		log.Error().Err(rErr).Str("outcome", result.Outcome).Msg("Could not report the restore result to the framework")
		if err == nil {
			return res, fmt.Errorf("restore finished, but its result could not be reported: %w", rErr)
		}
	}
	return res, err
}

// publicMessage is what the framework is told about a failure.
func publicMessage(err error, phase string) string {
	var se *stepError
	if errors.As(err, &se) {
		return se.msg
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Sprintf("Restore stopped during %s: time limit reached or cancelled", phase)
	}
	return fmt.Sprintf("Restore failed during %s", phase)
}

func withDefaults(o Options) Options {
	if o.runner == nil {
		o.runner = sysinfo.ExecRunner{Timeout: commandTimeout}
	}
	if o.extract == nil {
		o.extract = extract
	}
	if o.lookPath == nil {
		o.lookPath = exec.LookPath
	}
	if o.isBlockDevice == nil {
		o.isBlockDevice = isBlockDevice
	}
	if o.sysRoot == "" {
		o.sysRoot = "/"
	}
	if o.mntDir == "" {
		o.mntDir = "/mnt"
	}
	if o.reportInterval <= 0 {
		o.reportInterval = defaultReportInterval
	}
	if o.now == nil {
		o.now = time.Now
	}
	return o
}

// run carries out the phases in order.
func run(ctx context.Context, o Options, t *tracker) (Result, error) {
	t.enter(PhasePreparing)
	p, err := prepare(ctx, o)
	if err != nil {
		return Result{}, err
	}
	t.setTotal(p.totalBytes)
	res := Result{Partitions: len(p.targets), Firmware: p.firmware, Root: p.root().mountDir}

	t.enter(PhaseFormatting)
	if err := format(ctx, o, p.targets); err != nil {
		return res, err
	}
	t.enter(PhaseMounting)
	if err := mount(ctx, o, p.targets); err != nil {
		return res, err
	}
	t.enter(PhaseExtracting)
	if err := unpack(ctx, o, p, t); err != nil {
		return res, err
	}
	t.enter(PhaseReconfiguring)
	warnings, err := reconfigure(ctx, o, p)
	res.Warnings = warnings
	if err != nil {
		return res, err
	}
	t.enter(PhaseBootloader)
	if err := installBootloader(ctx, o, p); err != nil {
		return res, err
	}
	return res, nil
}

// prepared is everything checked and worked out before the first change.
type prepared struct {
	staged     rescue.Staged
	targets    []*target
	firmware   sysinfo.Firmware
	totalBytes int64
	// oldMAC and newMAC rewrite the restored netplan; both empty when the
	// source's netplan matches no interface by MAC.
	oldMAC, newMAC string
}

func (p *prepared) root() *target { return p.byMount("/") }

func (p *prepared) byMount(mountPoint string) *target {
	for _, t := range p.targets {
		if t.item.MountPoint == mountPoint {
			return t
		}
	}
	return nil
}

// prepare reads the manifest and checks that this machine can take the set.
// It changes nothing, so a failure here leaves the disks as they were.
func prepare(ctx context.Context, o Options) (*prepared, error) {
	s := o.Staged
	manifestName := backupmodel.ManifestName(s.Plan.SourceBackupSetID)
	raw, err := o.runner.Run(ctx, "rclone", rcloneArgs(s, "cat", manifestName)...)
	if err != nil {
		return nil, fail(err, "Manifest of backup set %s could not be read from the repository", s.Plan.SourceBackupSetID)
	}
	var m backupmodel.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fail(err, "Manifest of backup set %s is malformed", s.Plan.SourceBackupSetID)
	}
	if err := checkManifest(m, s.Plan); err != nil {
		return nil, fail(err, "Backup set %s cannot be restored", s.Plan.SourceBackupSetID)
	}
	info, err := decodeSystem(m.System)
	if err != nil {
		return nil, fail(err, "Manifest of backup set %s is malformed", s.Plan.SourceBackupSetID)
	}
	targets, err := planTargets(m.Set, info, o.mntDir)
	if err != nil {
		return nil, fail(err, "Partition layout of backup set %s is not supported", s.Plan.SourceBackupSetID)
	}
	p := &prepared{staged: s, targets: targets, firmware: firmwareOf(o.sysRoot)}
	log.Info().Str("setId", m.Set.ID).Str("systemId", m.Set.SystemID).Int("partitions", len(targets)).
		Str("sourceFirmware", string(info.Firmware)).Str("targetFirmware", string(p.firmware)).
		Msg("Backup set manifest read")

	if p.firmware == sysinfo.FirmwareUEFI && p.byMount(espMountPoint) == nil {
		return nil, fail(errors.New("no "+espMountPoint+" partition in the set"),
			"This machine boots with UEFI but the backup set has no EFI system partition")
	}

	if err := p.matchNIC(info, o.sysRoot); err != nil {
		return nil, fail(err, "Network interface of the source cannot be matched to this machine")
	}
	if err := checkTools(o, targets); err != nil {
		return nil, fail(err, "Rescue environment lacks a required tool")
	}
	listing, err := o.runner.Run(ctx, "lsblk", "-r", "-n", "-o", "PATH,PARTUUID")
	if err != nil {
		return nil, fail(err, "Partitions of this machine could not be listed")
	}
	if err := resolveDevices(targets, string(listing), o.mntDir); err != nil {
		return nil, fail(err, "Partitions of backup set %s cannot be matched to this machine", s.Plan.SourceBackupSetID)
	}
	for _, tg := range targets {
		log.Info().Str("mountPoint", tg.item.MountPoint).Str("sourceDevice", tg.item.Device).
			Str("device", tg.device).Str("partUuid", tg.partUUID).Msg("Matched partition by PARTUUID")
		if err := o.isBlockDevice(tg.device); err != nil {
			return nil, fail(err, "Partition %s for %s not found on this machine", tg.device, tg.item.MountPoint)
		}
		mounts, err := mountPoints(ctx, o.runner, tg.device)
		if err != nil {
			return nil, fail(err, "Mount state of %s could not be read", tg.device)
		}
		if len(mounts) > 0 {
			return nil, fail(fmt.Errorf("mounted at %s", strings.Join(mounts, ", ")),
				"Partition %s is in use: unmount it before restoring", tg.device)
		}
		p.totalBytes += tg.item.SizeBytes
	}
	return p, nil
}

// matchNIC pairs the MAC address the source's netplan matches on with this
// machine's network interface. Only the one-to-one case can be decided
// without guessing; anything else is refused before the disks are touched.
func (p *prepared) matchNIC(info *sysinfo.SystemInfo, sysRoot string) error {
	macs := netplanMACs(info.NetplanFiles)
	if len(macs) == 0 {
		return nil
	}
	nics, err := physicalNICs(sysRoot)
	if err != nil {
		return err
	}
	if len(macs) != 1 || len(nics) != 1 {
		return fmt.Errorf("the source's netplan matches %d MAC address(es) and this machine has %d network interface(s); only one of each is supported", len(macs), len(nics))
	}
	p.oldMAC, p.newMAC = macs[0], nics[0]
	return nil
}

// physicalNICs returns the MAC addresses of this machine's network
// interfaces backed by a device, leaving out loopback and virtual ones.
func physicalNICs(sysRoot string) ([]string, error) {
	dir := filepath.Join(sysRoot, "sys", "class", "net")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list network interfaces: %w", err)
	}
	var macs []string
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "device")); err != nil {
			continue
		}
		addr, err := os.ReadFile(filepath.Join(dir, e.Name(), "address"))
		if err != nil {
			return nil, fmt.Errorf("read the address of %s: %w", e.Name(), err)
		}
		macs = append(macs, strings.ToLower(strings.TrimSpace(string(addr))))
	}
	return macs, nil
}

// firmwareOf reports how this machine booted.
func firmwareOf(sysRoot string) sysinfo.Firmware {
	if _, err := os.Stat(filepath.Join(sysRoot, "sys", "firmware", "efi")); err == nil {
		return sysinfo.FirmwareUEFI
	}
	return sysinfo.FirmwareBIOS
}

// checkTools confirms every command the restore runs outside the restored
// system is installed, so that a missing one is found before formatting.
func checkTools(o Options, targets []*target) error {
	tools := []string{"rclone", "tar", "lsblk", "blkid", "mountall", "mount", "umount", "sync", "arch-chroot"}
	for _, tg := range targets {
		tools = append(tools, "mkfs."+tg.fsType)
		if d := decompressors[tg.item.Compression]; d != nil {
			tools = append(tools, d[0])
		}
	}
	var missing []string
	seen := map[string]bool{}
	for _, tool := range tools {
		if seen[tool] {
			continue
		}
		seen[tool] = true
		if _, err := o.lookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("not installed: %s", strings.Join(missing, ", "))
	}
	return nil
}

// isBlockDevice reports an error unless path is a block device.
func isBlockDevice(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return fmt.Errorf("%s is not a block device", path)
	}
	return nil
}

// mountPoints returns where device is mounted, if anywhere.
func mountPoints(ctx context.Context, r sysinfo.Runner, device string) ([]string, error) {
	out, err := r.Run(ctx, "lsblk", "-n", "-o", "MOUNTPOINTS", device)
	if err != nil {
		return nil, err
	}
	var mounts []string
	for line := range strings.Lines(string(out)) {
		if m := strings.TrimSpace(line); m != "" {
			mounts = append(mounts, m)
		}
	}
	return mounts, nil
}

// rcloneArgs builds an rclone command reading name from the source system's
// directory in the repository.
func rcloneArgs(s rescue.Staged, command, name string) []string {
	remote := s.RcloneRemote + ":" + path.Join(s.Repository.Bucket, s.Repository.Prefix, s.Plan.SourceSystemID, name)
	return []string{
		command, remote,
		"--config", s.RcloneFile,
		// Bounded so that a stalled connection fails instead of hanging the
		// restore; low-level retries ride out a brief network drop.
		"--contimeout", "30s", "--timeout", "5m", "--low-level-retries", "10",
	}
}

// format recreates each filesystem with the source's label and UUID, and
// confirms the UUID took. Everything after relies on it: fstab, grub and the
// initramfs of the restored system name the filesystems by these UUIDs.
func format(ctx context.Context, o Options, targets []*target) error {
	for _, tg := range targets {
		args := mkfsArgs(tg.fsType, tg.label, tg.uuid, tg.device)
		log.Info().Str("device", tg.device).Str("fsType", tg.fsType).Str("label", tg.label).
			Str("uuid", tg.uuid).Str("mountPoint", tg.item.MountPoint).Msg("Formatting partition")
		if _, err := o.runner.Run(ctx, "mkfs."+tg.fsType, args...); err != nil {
			return fail(err, "Formatting %s as %s failed", tg.device, tg.fsType)
		}
		out, err := o.runner.Run(ctx, "blkid", "-s", "UUID", "-o", "value", tg.device)
		if err != nil {
			return fail(err, "UUID of the new filesystem on %s could not be read", tg.device)
		}
		if got := strings.TrimSpace(string(out)); !strings.EqualFold(got, tg.uuid) {
			return fail(fmt.Errorf("blkid reports %q, want %q", got, tg.uuid),
				"UUID of %s could not be preserved", tg.device)
		}
	}
	return nil
}

// mkfsArgs are the arguments to mkfs.<fsType>. The label and UUID are the
// source's: fstab and grub name a filesystem by one or the other, as Ubuntu
// cloud images do with both.
func mkfsArgs(fsType, label, uuid, device string) []string {
	var args []string
	switch fsType {
	case "ext4":
		args = []string{"-F", "-q", "-U", uuid}
		if label != "" {
			args = append(args, "-L", label)
		}
	case "xfs":
		args = []string{"-f", "-q", "-m", "uuid=" + uuid}
		if label != "" {
			args = append(args, "-L", label)
		}
	case "vfat":
		// A vfat volume ID is written "ABCD-1234" and given as "ABCD1234".
		args = []string{"-i", strings.ReplaceAll(uuid, "-", "")}
		if label != "" {
			args = append(args, "-n", label)
		}
	}
	return append(args, device)
}

// mount mounts the new filesystems with SystemRescue's mountall, which puts
// each at /mnt/<device name>, and confirms every target is where expected.
func mount(ctx context.Context, o Options, targets []*target) error {
	if _, err := o.runner.Run(ctx, "mountall", "--no-bind"); err != nil {
		return fail(err, "Mounting the new filesystems failed")
	}
	for _, tg := range targets {
		mounts, err := mountPoints(ctx, o.runner, tg.device)
		if err != nil {
			return fail(err, "Mount state of %s could not be read", tg.device)
		}
		found := false
		for _, m := range mounts {
			if m == tg.mountDir {
				found = true
			}
		}
		if !found {
			return fail(fmt.Errorf("mounted at %v", mounts), "%s is not mounted at %s", tg.device, tg.mountDir)
		}
	}
	return nil
}

// unpack streams every backup file into its filesystem.
func unpack(ctx context.Context, o Options, p *prepared, t *tracker) error {
	for _, tg := range p.targets {
		t.setItem(tg.item.FileName)
		started := time.Now()
		log.Info().Str("file", tg.item.FileName).Str("device", tg.device).Str("dest", tg.mountDir).
			Int64("sizeBytes", tg.item.SizeBytes).Msg("Restoring backup file")
		src := source{
			rcloneArgs: rcloneArgs(p.staged, "cat", tg.item.FileName),
			decompress: decompressors[tg.item.Compression],
		}
		if err := o.extract(ctx, src, tg.mountDir, &t.read); err != nil {
			return fail(err, "Restoring %s onto %s failed", tg.item.FileName, tg.device)
		}
		log.Info().Str("file", tg.item.FileName).Dur("elapsed", time.Since(started)).Msg("Backup file restored")
	}
	if _, err := o.runner.Run(ctx, "sync"); err != nil {
		return fail(err, "Flushing the restored files to disk failed")
	}
	return nil
}

// reconfigure adapts the restored system to this machine and checks what it
// does not change: its network interface is pointed at this machine's MAC
// address, and the marker that keeps the restored agent from acting as the
// source system until its boot is confirmed is written. The filesystem UUIDs
// it names are checked against this machine; a mismatch is returned as a
// warning, not an error, and the file is left as it is.
func reconfigure(ctx context.Context, o Options, p *prepared) ([]string, error) {
	root := p.root().mountDir
	var warnings []string
	warn := func(msg string) {
		log.Warn().Msg(msg)
		warnings = append(warnings, msg)
	}

	uuids := make(map[string]string, len(p.targets))
	for _, tg := range p.targets {
		uuids[tg.item.MountPoint] = tg.uuid
	}
	fstab, err := os.ReadFile(filepath.Join(root, "etc", "fstab"))
	if err != nil {
		return warnings, fail(err, "Reading /etc/fstab of the restored system failed")
	}
	// A rewrite that would change an entry means the entry is wrong.
	if _, wrong := rewriteFstab(string(fstab), uuids); wrong {
		warn("/etc/fstab names a restored mount point by a UUID other than its filesystem's")
	}

	if p.oldMAC != "" {
		files, err := globAll(filepath.Join(root, "etc", "netplan"), "*.yaml", "*.yml")
		if err != nil {
			return warnings, fail(err, "Listing the restored netplan configuration failed")
		}
		for _, f := range files {
			changed, err := rewriteFile(f, func(c string) (string, bool) { return rewriteNetplan(c, p.oldMAC, p.newMAC) })
			if err != nil {
				return warnings, fail(err, "Updating the restored netplan configuration failed")
			}
			if changed {
				log.Info().Str("file", filepath.Base(f)).Msg("Pointed netplan at this machine's MAC address")
			}
		}
	}

	known, err := diskIdentifiers(ctx, o.runner)
	if err != nil {
		return warnings, fail(err, "Listing this machine's disk identifiers failed")
	}
	grubFiles, err := globAll(filepath.Join(root, "etc", "default", "grub.d"), "*.cfg")
	if err != nil {
		return warnings, fail(err, "Listing the restored grub defaults failed")
	}
	if _, err := os.Stat(filepath.Join(root, "etc", "default", "grub")); err == nil {
		grubFiles = append([]string{filepath.Join(root, "etc", "default", "grub")}, grubFiles...)
	}
	for _, f := range grubFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			return warnings, fail(err, "Reading the restored grub defaults failed")
		}
		for _, id := range grubPinnedIDs(string(data)) {
			if !known[strings.ToLower(id.value)] {
				warn(fmt.Sprintf("%s: %s names a disk identifier not on this machine", strings.TrimPrefix(f, root), id.where))
			}
		}
	}

	if esp := p.byMount(espMountPoint); esp != nil {
		boot := p.byMount("/boot")
		if boot == nil {
			boot = p.root()
		}
		stubs, err := filepath.Glob(filepath.Join(esp.mountDir, "EFI", "*", "grub.cfg"))
		if err != nil {
			return warnings, fail(err, "Listing grub configuration on the EFI system partition failed")
		}
		for _, f := range stubs {
			data, err := os.ReadFile(f)
			if err != nil {
				return warnings, fail(err, "Reading the grub stub on the EFI system partition failed")
			}
			if _, wrong := rewriteEFIStub(string(data), boot.uuid); wrong {
				warn(fmt.Sprintf("EFI grub stub %s searches for a partition other than %s", strings.TrimPrefix(f, esp.mountDir), boot.item.MountPoint))
			}
		}
	}

	marker := state.RecoveryPending{RecoveryID: p.staged.RecoveryID, RestoredAt: o.now().UTC()}
	if err := state.WriteRecoveryPending(filepath.Join(root, state.Dir), marker); err != nil {
		return warnings, fail(err, "Writing the recovery marker onto the restored system failed")
	}
	return warnings, nil
}

// diskIdentifiers returns every filesystem UUID and partition UUID on this
// machine, lowercased.
func diskIdentifiers(ctx context.Context, r sysinfo.Runner) (map[string]bool, error) {
	out, err := r.Run(ctx, "lsblk", "-n", "-r", "-o", "UUID,PARTUUID")
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for id := range strings.FieldsSeq(string(out)) {
		known[strings.ToLower(id)] = true
	}
	return known, nil
}

// globAll returns the files in dir matching any of patterns. A missing dir has
// none.
func globAll(dir string, patterns ...string) ([]string, error) {
	var out []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			return nil, err
		}
		out = append(out, matches...)
	}
	return out, nil
}

// installBootloader assembles the restored tree under its root, then reinstalls
// grub and rebuilds the initramfs from inside it with arch-chroot.
func installBootloader(ctx context.Context, o Options, p *prepared) (err error) {
	root := p.root().mountDir

	// mountall mounted every filesystem side by side; the chroot needs them
	// nested as the restored system mounts them.
	var bound []string
	defer func() {
		for i := len(bound) - 1; i >= 0; i-- {
			if _, uErr := o.runner.Run(context.WithoutCancel(ctx), "umount", bound[i]); uErr != nil {
				log.Warn().Err(uErr).Str("path", bound[i]).Msg("Could not unmount a bind mount")
				if err == nil {
					err = fail(uErr, "Unmounting %s after the bootloader install failed", bound[i])
				}
			}
		}
	}()
	for _, tg := range p.targets {
		if tg.item.MountPoint == "/" {
			continue
		}
		dst := filepath.Join(root, tg.item.MountPoint)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fail(err, "Preparing %s in the restored system failed", tg.item.MountPoint)
		}
		if _, err := o.runner.Run(ctx, "mount", "--bind", tg.mountDir, dst); err != nil {
			return fail(err, "Mounting %s into the restored system failed", tg.item.MountPoint)
		}
		bound = append(bound, dst)
	}

	install := []string{root, "grub-install"}
	if p.firmware == sysinfo.FirmwareUEFI {
		install = append(install, "--target=x86_64-efi", "--efi-directory="+espMountPoint)
	} else {
		out, err := o.runner.Run(ctx, "lsblk", "-n", "-d", "-o", "PKNAME", p.root().device)
		disk := strings.TrimSpace(string(out))
		if err != nil || disk == "" {
			if err == nil {
				err = errors.New("lsblk reported no parent disk")
			}
			return fail(err, "Disk holding %s could not be determined", p.root().device)
		}
		install = append(install, "--target=i386-pc", "/dev/"+disk)
	}

	steps := []struct {
		args []string
		what string
	}{
		{install, "Installing grub"},
		{[]string{root, "update-grub"}, "Updating the grub configuration"},
		{[]string{root, "update-initramfs", "-u", "-k", "all"}, "Rebuilding the initramfs"},
	}
	for _, s := range steps {
		log.Info().Strs("command", s.args[1:]).Msg(s.what)
		if _, err := o.runner.Run(ctx, "arch-chroot", s.args...); err != nil {
			return fail(err, "%s in the restored system failed", s.what)
		}
	}
	return nil
}
