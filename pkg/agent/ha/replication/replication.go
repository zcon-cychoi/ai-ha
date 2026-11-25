// Package replication configures and removes this node's side of an HA
// group's replicated volume (TM-AI-HA-02 4.4, plan/ha-roadmap.md H3b).
//
// Every step looks at the node first and does only what is not already so,
// because a request can come again: an operator asks for setup once more after
// fixing what failed, or a result was lost after the work was done. Two steps
// cannot be undone — writing replication metadata and making a filesystem —
// and each happens only on a disk that holds nothing: a blank disk gets
// metadata, a blank volume gets a filesystem, and a disk holding anything else
// is refused rather than overwritten.
//
// The agent runs in a sandbox (deploy/aiha-agent.service). It cannot load
// kernel modules, and a mount it made would stay inside its own mount
// namespace where the service could not see it. So the resource is brought up
// by its systemd unit, drbd@<resource>.target, whose drbdsetup loads the
// module, and the volume is mounted by a mount unit: both run under PID 1. The
// agent itself runs only systemctl and the DRBD tools.
//
// drbd.service is not used. It waits for every resource's peer to connect,
// without a limit unless the resource file sets one, so on the standby, which
// is set up before its peer, it would never finish starting; at boot it would
// hold up the system the same way whenever the peer is down. LINBIT's own
// unit description says to use drbd@.target instead.
//
// The resource file holds the peers' shared secret. It is written 0600, never
// logged, and excluded from backups (pkg/backup/source).
package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/agent/ha/drbd"
	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/sysinfo"
)

// Paths, relative to the root the Configurer writes under.
const (
	resourceDir = "etc/drbd.d"
	unitDir     = "etc/systemd/system"
	// dropInName is the drop-in that makes the service require the volume.
	dropInName = "aiha-replication.conf"
)

// Step names reported in a job's result.
const (
	StepResourceFile   = "write-resource-file"
	StepMountUnit      = "write-mount-unit"
	StepServiceDropIn  = "require-mount-for-service"
	StepDisableService = "disable-service-at-boot"
	StepCreateMetadata = "create-metadata"
	// StepOverwriteData is reported, done, when metadata was created over a
	// disk that held data, as the job asked.
	StepOverwriteData  = "overwrite-disk-data"
	StepBringUp        = "bring-up-resource"
	StepFirstPromotion = "first-promotion"
	StepPromote        = "promote"
	StepFormat         = "format-volume"
	StepMount          = "mount-volume"
	StepStartService   = "start-service"
	StepStopService    = "stop-service"
	StepUnmount        = "unmount-volume"
	StepDemote         = "demote"
	StepTakeDown       = "take-down-resource"
	StepRemoveFiles    = "remove-files"
)

// connectTimeout is how long the initial primary waits for its peer, which
// was set up just before it, to connect. A first promotion needs to see the
// peer's disk, so it cannot go ahead without it.
const connectTimeout = 2 * time.Minute

// pollInterval is how often the link is looked at while waiting for it.
const pollInterval = 2 * time.Second

// blankEdge is how much of each end of a disk without a signature must read
// as zeros for the disk to count as empty. drbdmeta asks before creating
// metadata over a disk whose first 68 KiB are not all zeros, and a job has
// no one to answer; the internal metadata goes at the end.
const blankEdge = 1 << 20

var (
	resourcePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	devicePattern   = regexp.MustCompile(`^/dev/drbd[0-9]{1,5}$`)
	// unitPattern matches ha's: a service unit name that cannot start with a
	// hyphen, where systemctl would read an option.
	unitPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.\\@-]{0,246}\.service$`)
	hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$`)
	// secretPattern keeps the secret to what can sit between quotes in the
	// resource file. The framework makes it from hex digits.
	secretPattern = regexp.MustCompile(`^[A-Za-z0-9]{16,64}$`)
	// byIDName is a name under /dev/disk/by-id. udev builds them from
	// letters, digits and a few separators; quotes and spaces would break the
	// resource file.
	byIDName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:+@-]{0,254}$`)
)

// ErrRefused reports a request this node will not carry out as it stands.
// Nothing was changed by the step that refused; earlier steps' effects stay.
var ErrRefused = errors.New("refused")

// Result is what a job reports.
type Result = model.ReplicationJobResult

// Configurer carries out setup and teardown on this node.
type Configurer struct {
	runner   sysinfo.Runner
	status   *drbd.Reader
	root     string
	hostname func() (string, error)
	sleep    func(context.Context, time.Duration) error
	// connectTimeout is connectTimeout, shortened by tests.
	connectTimeout time.Duration
}

// New returns a Configurer that runs commands through runner and writes its
// files under the real root.
func New(runner sysinfo.Runner) *Configurer {
	return &Configurer{
		runner:         runner,
		status:         drbd.NewReader(runner),
		root:           "/",
		hostname:       os.Hostname,
		sleep:          sleepCtx,
		connectTimeout: connectTimeout,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// refuse builds an ErrRefused error.
func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// record keeps the steps of one job.
type record struct{ steps []model.ReplicationStep }

func (r *record) add(name string, done bool) {
	r.steps = append(r.steps, model.ReplicationStep{Name: name, Done: done})
}

// Setup configures this node's side of the resource spec describes. secret is
// the peers' shared secret, from the job's claim.
func (c *Configurer) Setup(ctx context.Context, spec model.ReplicationSetupSpec, secret string) (Result, error) {
	var rec record
	res, err := c.setup(ctx, spec, secret, &rec)
	res.Steps = rec.steps
	return res, err
}

func (c *Configurer) setup(ctx context.Context, spec model.ReplicationSetupSpec, secret string, rec *record) (Result, error) {
	if err := validateSetup(spec, secret); err != nil {
		return Result{}, err
	}
	host, err := c.hostname()
	if err != nil {
		return Result{}, fmt.Errorf("read this node's host name: %w", err)
	}
	if host != spec.Self.Hostname {
		return Result{}, refuse("this node is %q but the job is for %q, so the resource file would not name it", host, spec.Self.Hostname)
	}

	// Preflight: look before touching anything.
	disk, err := c.inspectDisk(ctx, spec.Self.Disk)
	if err != nil {
		return Result{}, err
	}
	res := Result{DiskBytes: disk.size}
	if disk.holdsData && !spec.OverwriteDisk {
		return res, refuse("%s has no filesystem or other signature, but its first or last MiB is not zeros: "+
			"setup only uses an empty disk. If nothing on it is needed, clear it (blkdiscard -z) "+
			"or ask for setup with --overwrite-disk", spec.Self.Disk)
	}
	if spec.PeerDiskBytes > 0 && disk.size != spec.PeerDiskBytes {
		return res, refuse("this node's data disk is %d bytes and the peer's %d: they must be the same size",
			disk.size, spec.PeerDiskBytes)
	}
	if err := c.checkUnitsAvailable(ctx, spec.Resource); err != nil {
		return res, err
	}
	svc, err := c.unit(ctx, spec.ServiceUnit)
	if err != nil {
		return res, err
	}
	if svc.LoadState != "loaded" {
		return res, refuse("service %s is %s on this node", spec.ServiceUnit, orUnknown(svc.LoadState))
	}
	if svc.TriggeredBy != "" {
		return res, refuse("service %s is started by %s, which would start it on a node that is not primary: "+
			"remove that trigger first", spec.ServiceUnit, svc.TriggeredBy)
	}
	mountUnit, err := c.mountUnitName(ctx, spec.MountPoint)
	if err != nil {
		return res, err
	}
	up, st, err := c.resourceState(ctx, spec.Resource)
	if err != nil {
		return res, err
	}
	if svc.running() {
		// A service already running is the end state of an earlier setup of
		// the primary, when the volume is up and mounted here. Anywhere else
		// it is writing to the root filesystem, and what it writes during
		// setup would not be replicated.
		mnt, err := c.unit(ctx, mountUnit)
		if err != nil {
			return res, err
		}
		setUpHere := spec.InitialPrimary && up && st.Role == "Primary" && mnt.running()
		if !setUpHere {
			return res, refuse("service %s is running, and the volume is not mounted here: "+
				"stop it before setting up replication", spec.ServiceUnit)
		}
	}

	// Configuration files. They are rewritten only when they differ, so a
	// repeated setup changes nothing on a node already set up.
	text, err := resourceFile(spec, secret)
	if err != nil {
		return res, err
	}
	changed := false
	wrote, err := c.writeFile(filepath.Join(resourceDir, spec.Resource+".res"), text, 0o600)
	if err != nil {
		return res, err
	}
	rec.add(StepResourceFile, wrote)
	changed = changed || wrote
	// dump parses the configuration as the tool will. Its output holds the
	// secret, so it is discarded unread.
	if _, err := c.runner.Run(ctx, "drbdadm", "dump", spec.Resource); err != nil {
		return res, fmt.Errorf("check the resource file: %w", err)
	}
	if wrote, err = c.writeFile(filepath.Join(unitDir, mountUnit), mountUnitFile(spec), 0o644); err != nil {
		return res, err
	}
	rec.add(StepMountUnit, wrote)
	changed = changed || wrote
	if wrote, err = c.writeFile(filepath.Join(unitDir, spec.ServiceUnit+".d", dropInName), dropInFile(spec), 0o644); err != nil {
		return res, err
	}
	rec.add(StepServiceDropIn, wrote)
	changed = changed || wrote
	if changed {
		if _, err := c.runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return res, fmt.Errorf("reload systemd units: %w", err)
		}
	}
	// A service enabled at boot would start before anyone decided which node
	// is primary. On a secondary its mount fails and so does it, but the
	// intent is that it is started by the agent only.
	disabled := false
	if strings.HasPrefix(svc.UnitFileState, "enabled") {
		if _, err := c.runner.Run(ctx, "systemctl", "disable", "--", spec.ServiceUnit); err != nil {
			return res, fmt.Errorf("disable %s at boot: %w", spec.ServiceUnit, err)
		}
		disabled = true
	}
	rec.add(StepDisableService, disabled)

	// Metadata, only on a blank disk.
	switch disk.signature {
	case "":
		args := []string{"create-md", spec.Resource}
		if disk.holdsData {
			// Asked for by the job (checked above): drbdmeta would
			// otherwise ask whether to go on.
			args = []string{"create-md", "--force", spec.Resource}
		}
		if _, err := c.runner.Run(ctx, "drbdadm", args...); err != nil {
			return res, fmt.Errorf("create the replication metadata: %w", err)
		}
		rec.add(StepCreateMetadata, true)
		if disk.holdsData {
			rec.add(StepOverwriteData, true)
		}
	case "drbd":
		rec.add(StepCreateMetadata, false)
	}

	// Bring the resource up, and at boot. drbd@.target is started by PID 1,
	// which can load the module; adjust then applies a resource file that
	// changed on a resource already up, and does nothing otherwise.
	if _, err := c.runner.Run(ctx, "systemctl", "enable", "drbd-configured.target", targetUnit(spec.Resource)); err != nil {
		return res, fmt.Errorf("enable the resource at boot: %w", err)
	}
	if _, err := c.runner.Run(ctx, "systemctl", "start", targetUnit(spec.Resource)); err != nil {
		return res, fmt.Errorf("bring the resource up: %w", err)
	}
	if _, err := c.runner.Run(ctx, "drbdadm", "adjust", spec.Resource); err != nil {
		return res, fmt.Errorf("apply the resource file: %w", err)
	}
	rec.add(StepBringUp, !up)
	if st, err = c.status.Status(ctx, spec.Resource); err != nil {
		return res, err
	}
	res.Role, res.Disk = st.Role, st.Disk

	if !spec.InitialPrimary {
		if st.Role != "Secondary" {
			return res, refuse("the resource is %s here, and the standby must be Secondary", st.Role)
		}
		return res, nil
	}
	return c.becomePrimary(ctx, spec, mountUnit, st, &res, rec)
}

// becomePrimary promotes the initial primary, formats a blank volume, mounts
// it and starts the service.
func (c *Configurer) becomePrimary(ctx context.Context, spec model.ReplicationSetupSpec, mountUnit string, st drbd.Status, res *Result, rec *record) (Result, error) {
	if st.Role != "Primary" {
		var err error
		if st, err = c.waitConnected(ctx, spec.Resource); err != nil {
			return *res, err
		}
		switch {
		case st.Disk == "Inconsistent" && st.PeerDisk == "Inconsistent":
			// Neither side holds data yet, so one has to be declared the
			// source. This is the only time --force is used.
			if _, err := c.runner.Run(ctx, "drbdadm", "primary", "--force", spec.Resource); err != nil {
				return *res, fmt.Errorf("promote for the initial synchronization: %w", err)
			}
			rec.add(StepFirstPromotion, true)
		case st.Disk == "UpToDate":
			if _, err := c.runner.Run(ctx, "drbdadm", "primary", spec.Resource); err != nil {
				return *res, fmt.Errorf("promote: %w", err)
			}
			rec.add(StepPromote, true)
		default:
			return *res, refuse("this node's disk is %s and the peer's %s, so it cannot be promoted without "+
				"overwriting data that may be newer", st.Disk, orUnknown(st.PeerDisk))
		}
	} else {
		rec.add(StepPromote, false)
	}

	fsType, err := c.signature(ctx, spec.Device)
	if err != nil {
		return *res, err
	}
	switch fsType {
	case "":
		// No discard: on a volume being synchronized it would walk the whole
		// device, and a new disk has nothing to discard.
		if _, err := c.runner.Run(ctx, "mkfs.ext4", "-q", "-E", "nodiscard", spec.Device); err != nil {
			return *res, fmt.Errorf("format the replicated volume: %w", err)
		}
		rec.add(StepFormat, true)
	case "ext4":
		rec.add(StepFormat, false)
	default:
		return *res, refuse("the replicated volume holds %s, not ext4: it is not formatted over", fsType)
	}

	mnt, err := c.unit(ctx, mountUnit)
	if err != nil {
		return *res, err
	}
	if !mnt.running() {
		if _, err := c.runner.Run(ctx, "systemctl", "start", "--", mountUnit); err != nil {
			return *res, fmt.Errorf("mount the replicated volume: %w", err)
		}
	}
	rec.add(StepMount, !mnt.running())

	svc, err := c.unit(ctx, spec.ServiceUnit)
	if err != nil {
		return *res, err
	}
	if !svc.running() {
		if _, err := c.runner.Run(ctx, "systemctl", "start", "--", spec.ServiceUnit); err != nil {
			return *res, fmt.Errorf("start %s: %w", spec.ServiceUnit, err)
		}
	}
	rec.add(StepStartService, !svc.running())
	if after, err := c.unit(ctx, spec.ServiceUnit); err != nil {
		return *res, err
	} else if after.ActiveState != "active" {
		return *res, fmt.Errorf("%s is %s after starting it", spec.ServiceUnit, orUnknown(after.ActiveState))
	}

	if st, err = c.status.Status(ctx, spec.Resource); err != nil {
		return *res, err
	}
	res.Role, res.Disk = st.Role, st.Disk
	return *res, nil
}

// waitConnected waits for the peer to connect, which the initial primary
// needs to see the peer's disk.
func (c *Configurer) waitConnected(ctx context.Context, resource string) (drbd.Status, error) {
	deadline := time.Now().Add(c.connectTimeout)
	for {
		st, err := c.status.Status(ctx, resource)
		if err != nil {
			return drbd.Status{}, err
		}
		if st.Connection == "Connected" && st.PeerDisk != "" && st.PeerDisk != "DUnknown" {
			return st, nil
		}
		if time.Now().After(deadline) {
			return st, refuse("the peer did not connect within %s (link %s): check that its setup completed "+
				"and that the replication port is open between the nodes", c.connectTimeout, st.Connection)
		}
		if err := c.sleep(ctx, pollInterval); err != nil {
			return st, err
		}
	}
}

// Teardown removes this node's side of the resource: the service stops, the
// volume is unmounted, the resource steps down and goes down, and the files
// setup wrote are removed. The disk's data and metadata are left as they are.
func (c *Configurer) Teardown(ctx context.Context, spec model.ReplicationTeardownSpec) (Result, error) {
	var rec record
	res, err := c.teardown(ctx, spec, &rec)
	res.Steps = rec.steps
	return res, err
}

func (c *Configurer) teardown(ctx context.Context, spec model.ReplicationTeardownSpec, rec *record) (Result, error) {
	if err := validateTeardown(spec); err != nil {
		return Result{}, err
	}
	mountUnit, err := c.mountUnitName(ctx, spec.MountPoint)
	if err != nil {
		return Result{}, err
	}

	for _, u := range []struct{ unit, step string }{
		{spec.ServiceUnit, StepStopService}, {mountUnit, StepUnmount},
	} {
		st, err := c.unit(ctx, u.unit)
		if err != nil {
			return Result{}, err
		}
		if st.running() {
			if _, err := c.runner.Run(ctx, "systemctl", "stop", "--", u.unit); err != nil {
				return Result{}, fmt.Errorf("stop %s: %w", u.unit, err)
			}
		}
		rec.add(u.step, st.running())
	}

	wasUp, st, err := c.resourceState(ctx, spec.Resource)
	if err != nil {
		return Result{}, err
	}
	demoted := false
	if wasUp && st.Role == "Primary" {
		// drbdsetup rather than drbdadm: it needs no resource file, so a
		// resource configured by hand whose file is gone still comes down.
		if _, err := c.runner.Run(ctx, "drbdsetup", "secondary", spec.Resource); err != nil {
			return Result{}, fmt.Errorf("step down: %w", err)
		}
		demoted = true
	}
	rec.add(StepDemote, demoted)

	target := targetUnit(spec.Resource)
	if t, err := c.unit(ctx, target); err != nil {
		return Result{}, err
	} else if t.LoadState == "loaded" {
		if _, err := c.runner.Run(ctx, "systemctl", "disable", target); err != nil {
			return Result{}, fmt.Errorf("stop bringing the resource up at boot: %w", err)
		}
		// Stopping the target stops its service, whose stop takes the
		// resource down.
		if _, err := c.runner.Run(ctx, "systemctl", "stop", target, serviceUnit(spec.Resource)); err != nil {
			return Result{}, fmt.Errorf("stop the resource's units: %w", err)
		}
	}
	stillUp, _, err := c.resourceState(ctx, spec.Resource)
	if err != nil {
		return Result{}, err
	}
	if stillUp {
		if _, err := c.runner.Run(ctx, "drbdsetup", "down", spec.Resource); err != nil {
			return Result{}, fmt.Errorf("take the resource down: %w", err)
		}
	}
	rec.add(StepTakeDown, wasUp)

	removed := false
	for _, p := range []string{
		filepath.Join(resourceDir, spec.Resource+".res"),
		filepath.Join(unitDir, mountUnit),
		filepath.Join(unitDir, spec.ServiceUnit+".d", dropInName),
	} {
		gone, err := c.removeFile(p)
		if err != nil {
			return Result{}, err
		}
		removed = removed || gone
	}
	// The drop-in directory goes too if setup's file was all it held. It
	// fails, as it should, when the directory holds the operator's own
	// drop-ins or is already gone; neither is this job's concern.
	_ = os.Remove(filepath.Join(c.root, unitDir, spec.ServiceUnit+".d"))
	if removed {
		if _, err := c.runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return Result{}, fmt.Errorf("reload systemd units: %w", err)
		}
	}
	rec.add(StepRemoveFiles, removed)
	return Result{}, nil
}

// diskFacts is what the preflight learns about the data disk.
type diskFacts struct {
	size      int64
	signature string
	// holdsData is set for a disk without a signature whose start or end
	// is not all zeros.
	holdsData bool
}

// inspectDisk checks that byID is a whole disk, unmounted and partitionless,
// holding either nothing or replication metadata.
func (c *Configurer) inspectDisk(ctx context.Context, byID string) (diskFacts, error) {
	// The link is checked here for a plain message; lsblk below checks that
	// what it names is a block device.
	if _, err := os.Lstat(filepath.Join(c.root, strings.TrimPrefix(byID, "/"))); errors.Is(err, fs.ErrNotExist) {
		return diskFacts{}, refuse("%s does not exist on this node: check the disk against its inventory", byID)
	} else if err != nil {
		return diskFacts{}, fmt.Errorf("look up %s: %w", byID, err)
	}

	out, err := c.runner.Run(ctx, "lsblk", "--json", "--bytes", "--output", "NAME,TYPE,SIZE,MOUNTPOINTS", byID)
	if err != nil {
		return diskFacts{}, fmt.Errorf("read %s: %w", byID, err)
	}
	dev, err := parseLsblk(out)
	if err != nil {
		return diskFacts{}, fmt.Errorf("read %s: %w", byID, err)
	}
	if dev.Type != "disk" {
		return diskFacts{}, refuse("%s is a %s, not a whole disk", byID, dev.Type)
	}
	if mp := dev.mountpoints(); len(mp) > 0 {
		return diskFacts{}, refuse("%s is mounted at %s", byID, strings.Join(mp, ", "))
	}
	for _, child := range dev.Children {
		// The replicated device sits on the disk once the resource is up;
		// anything else on it is somebody's data.
		if !strings.HasPrefix(child.Name, "drbd") {
			return diskFacts{}, refuse("%s holds %s (%s): use a disk with nothing on it", byID, child.Name, child.Type)
		}
	}
	if dev.Size == nil || *dev.Size <= 0 {
		return diskFacts{}, fmt.Errorf("lsblk did not report the size of %s", byID)
	}

	sig, err := c.signature(ctx, byID)
	if err != nil {
		return diskFacts{}, err
	}
	if sig != "" && sig != "drbd" {
		return diskFacts{}, refuse("%s holds %s: setup only uses a new, empty disk, and does not write over one", byID, sig)
	}
	facts := diskFacts{size: *dev.Size, signature: sig}
	if sig == "" {
		// blkid knows filesystems and partition tables, not data written
		// without one.
		if facts.holdsData, err = c.holdsData(ctx, byID, facts.size); err != nil {
			return diskFacts{}, err
		}
	}
	return facts, nil
}

// holdsData reports whether the first or last blankEdge bytes of dev, size
// bytes long, are not all zeros.
func (c *Configurer) holdsData(ctx context.Context, dev string, size int64) (bool, error) {
	n := min(int64(blankEdge), size)
	for _, skip := range []int64{0, size - n} {
		_, err := c.runner.Run(ctx, "cmp", "--silent", "--bytes="+strconv.FormatInt(n, 10),
			"--ignore-initial="+strconv.FormatInt(skip, 10)+":0", "--", dev, "/dev/zero")
		if code, ok := exitCode(err); ok && code == 1 {
			// cmp's "differ".
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("compare %s with zeros: %w", dev, err)
		}
	}
	return false, nil
}

// signature returns what blkid finds on dev, empty for nothing. It probes the
// device itself rather than the cache, which may predate the last write.
func (c *Configurer) signature(ctx context.Context, dev string) (string, error) {
	out, err := c.runner.Run(ctx, "blkid", "-p", "-o", "value", "-s", "TYPE", dev)
	if code, ok := exitCode(err); ok && code == 2 {
		// blkid's "nothing found".
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("probe %s for signatures: %w", dev, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// checkUnitsAvailable checks that the resource can be brought up the way
// setup does it, and that nothing else will bring it up differently.
func (c *Configurer) checkUnitsAvailable(ctx context.Context, resource string) error {
	t, err := c.unit(ctx, targetUnit(resource))
	if err != nil {
		return err
	}
	if t.LoadState != "loaded" {
		return refuse("%s is %s: install drbd-utils 9 from LINBIT, which provides it", targetUnit(resource), orUnknown(t.LoadState))
	}
	legacy, err := c.unit(ctx, "drbd.service")
	if err != nil {
		return err
	}
	// drbd-utils installs it disabled; this catches a node where it was
	// turned on by hand, as the manual verification once did.
	if strings.HasPrefix(legacy.UnitFileState, "enabled") {
		return refuse("drbd.service has been enabled: at boot it waits for every resource's peer without a limit. " +
			"Disable it again (systemctl disable drbd.service), as drbd-utils installs it; " +
			"each resource is brought up by its own drbd@<resource>.target")
	}
	return nil
}

// resourceState reports whether resource is up on this node, and its state
// when it is. With the module not loaded nothing is up; asking drbdsetup then
// would have it try to load the module, which the sandbox refuses.
func (c *Configurer) resourceState(ctx context.Context, resource string) (bool, drbd.Status, error) {
	if _, err := os.Stat(filepath.Join(c.root, "proc", "drbd")); errors.Is(err, fs.ErrNotExist) {
		return false, drbd.Status{}, nil
	} else if err != nil {
		return false, drbd.Status{}, fmt.Errorf("check for the replication module: %w", err)
	}
	out, err := c.runner.Run(ctx, "drbdsetup", "status", "--json")
	if err != nil {
		return false, drbd.Status{}, fmt.Errorf("list the resources up on this node: %w", err)
	}
	var all []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &all); err != nil {
		return false, drbd.Status{}, fmt.Errorf("list the resources up on this node: %w", err)
	}
	for _, r := range all {
		if r.Name == resource {
			st, err := c.status.Status(ctx, resource)
			return true, st, err
		}
	}
	return false, drbd.Status{}, nil
}

// unitState is the part of a unit's state setup looks at.
type unitState struct {
	LoadState     string
	ActiveState   string
	UnitFileState string
	TriggeredBy   string
}

// running reports a unit that is up or on its way up.
func (u unitState) running() bool {
	switch u.ActiveState {
	case "active", "activating", "reloading", "refreshing":
		return true
	}
	return false
}

// unit reads a unit's state. "systemctl show" exits zero whatever the state,
// so the state is read from its output rather than from an exit status.
func (c *Configurer) unit(ctx context.Context, unit string) (unitState, error) {
	out, err := c.runner.Run(ctx, "systemctl", "show",
		"--property=LoadState,ActiveState,UnitFileState,TriggeredBy", "--", unit)
	if err != nil {
		return unitState{}, fmt.Errorf("read the state of %s: %w", unit, err)
	}
	props := map[string]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	return unitState{
		LoadState:     props["LoadState"],
		ActiveState:   props["ActiveState"],
		UnitFileState: props["UnitFileState"],
		TriggeredBy:   props["TriggeredBy"],
	}, nil
}

// mountUnitName is the name systemd gives the mount unit of mountPoint.
func (c *Configurer) mountUnitName(ctx context.Context, mountPoint string) (string, error) {
	out, err := c.runner.Run(ctx, "systemd-escape", "--path", "--suffix=mount", "--", mountPoint)
	if err != nil {
		return "", fmt.Errorf("name the mount unit of %s: %w", mountPoint, err)
	}
	name := strings.TrimSpace(string(out))
	if !strings.HasSuffix(name, ".mount") || strings.ContainsAny(name, "/ \t\n") || strings.HasPrefix(name, "-") {
		return "", fmt.Errorf("systemd-escape named the mount unit of %s %q", mountPoint, name)
	}
	return name, nil
}

// writeFile writes content to rel under the root unless it is there already,
// reporting whether it wrote. The file is replaced whole, through a temporary
// file in the same directory, so a reader never sees half of it.
func (c *Configurer) writeFile(rel string, content []byte, perm os.FileMode) (bool, error) {
	p := filepath.Join(c.root, rel)
	if existing, err := os.ReadFile(p); err == nil && bytes.Equal(existing, content) {
		if err := os.Chmod(p, perm); err != nil {
			return false, fmt.Errorf("set the mode of %s: %w", "/"+rel, err)
		}
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", "/"+filepath.Dir(rel), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*")
	if err != nil {
		return false, fmt.Errorf("write %s: %w", "/"+rel, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	// The mode is set before the content goes in: the resource file holds
	// the secret and must never be readable by others, even briefly.
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("write %s: %w", "/"+rel, err)
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("write %s: %w", "/"+rel, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("write %s: %w", "/"+rel, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("write %s: %w", "/"+rel, err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return false, fmt.Errorf("write %s: %w", "/"+rel, err)
	}
	return true, nil
}

// removeFile removes rel under the root, reporting whether it was there.
func (c *Configurer) removeFile(rel string) (bool, error) {
	err := os.Remove(filepath.Join(c.root, rel))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("remove %s: %w", "/"+rel, err)
	}
	return true, nil
}

// targetUnit and serviceUnit are the per-resource units drbd-utils provides.
// Their instance is the systemd-escaped resource name: drbd@.service passes
// the unescaped instance (%I) to drbdadm, so an unescaped hyphen would read
// as "/" and name a volume of another resource. resourcePattern leaves the
// hyphen as the only character that needs escaping.
func targetUnit(resource string) string  { return "drbd@" + unitInstance(resource) + ".target" }
func serviceUnit(resource string) string { return "drbd@" + unitInstance(resource) + ".service" }

func unitInstance(resource string) string { return strings.ReplaceAll(resource, "-", `\x2d`) }

// resourceFile is the resource's configuration, in DRBD 9 syntax. The two
// nodes are written in node ID order, so both write the same file.
func resourceFile(spec model.ReplicationSetupSpec, secret string) ([]byte, error) {
	nodes := []model.ReplicationNode{spec.Self, spec.Peer}
	if nodes[0].NodeID > nodes[1].NodeID {
		nodes[0], nodes[1] = nodes[1], nodes[0]
	}
	var b strings.Builder
	b.WriteString("# Written by aiha-agent for an AI-HA group. The framework rewrites it on\n")
	b.WriteString("# every setup; edits do not last. It holds the peers' shared secret.\n")
	fmt.Fprintf(&b, "resource %s {\n", spec.Resource)
	// Without this a mount, or opening the device for writing, would promote
	// the node: a rebooted node must come back Secondary.
	b.WriteString("  options { auto-promote no; }\n")
	b.WriteString("  net {\n")
	fmt.Fprintf(&b, "    protocol %s;\n", spec.Protocol)
	b.WriteString("    cram-hmac-alg sha256;\n")
	fmt.Fprintf(&b, "    shared-secret \"%s\";\n", secret)
	b.WriteString("  }\n")
	for _, n := range nodes {
		addr, err := netip.ParseAddr(n.Address)
		if err != nil {
			return nil, fmt.Errorf("address of %s: %w", n.Hostname, err)
		}
		endpoint := addr.String() + ":" + strconv.Itoa(spec.Port)
		if addr.Is6() {
			endpoint = "ipv6 [" + addr.String() + "]:" + strconv.Itoa(spec.Port)
		}
		fmt.Fprintf(&b, "  on %s {\n", n.Hostname)
		fmt.Fprintf(&b, "    node-id   %d;\n", n.NodeID)
		fmt.Fprintf(&b, "    device    %s;\n", spec.Device)
		fmt.Fprintf(&b, "    disk      %s;\n", n.Disk)
		fmt.Fprintf(&b, "    address   %s;\n", endpoint)
		b.WriteString("    meta-disk internal;\n")
		b.WriteString("  }\n")
	}
	b.WriteString("}\n")
	return []byte(b.String()), nil
}

// mountUnitFile mounts the replicated volume. It has no [Install] section:
// it is started by the agent on the primary, never at boot.
func mountUnitFile(spec model.ReplicationSetupSpec) []byte {
	return fmt.Appendf(nil, `# Written by aiha-agent for an AI-HA group. Started on the primary only,
# by the agent; it is never started at boot.
[Unit]
Description=AI-HA replicated volume %s

[Mount]
What=%s
Where=%s
Type=ext4
`, spec.Resource, spec.Device, spec.MountPoint)
}

// dropInFile makes the service require the replicated mount. On a node that
// is not primary the mount fails, and so does starting the service, which
// would otherwise write to the root filesystem's mount point.
func dropInFile(spec model.ReplicationSetupSpec) []byte {
	return fmt.Appendf(nil, `# Written by aiha-agent for an AI-HA group: %s runs only where the
# replicated volume is mounted.
[Unit]
RequiresMountsFor=%s
`, spec.ServiceUnit, spec.MountPoint)
}

// validateSetup checks a setup job before anything is looked at. What is
// checked is what goes into a file or onto a command line.
func validateSetup(spec model.ReplicationSetupSpec, secret string) error {
	if err := validateCommon(spec.Resource, spec.MountPoint, spec.ServiceUnit); err != nil {
		return err
	}
	switch {
	case !devicePattern.MatchString(spec.Device):
		return refuse("the replicated device %q is not a /dev/drbdN path", spec.Device)
	case spec.Port < 1 || spec.Port > 65535:
		return refuse("the replication port %d is out of range", spec.Port)
	case spec.Protocol != "C":
		return refuse("replication protocol %q is not supported; only C is", spec.Protocol)
	case !secretPattern.MatchString(secret):
		return refuse("the job came without a usable shared secret")
	case spec.Self.NodeID == spec.Peer.NodeID:
		return refuse("both nodes have node ID %d", spec.Self.NodeID)
	case spec.PeerDiskBytes < 0:
		return refuse("the peer's disk size cannot be negative")
	}
	for _, n := range []model.ReplicationNode{spec.Self, spec.Peer} {
		name, ok := strings.CutPrefix(n.Disk, "/dev/disk/by-id/")
		switch {
		case !hostnamePattern.MatchString(n.Hostname):
			return refuse("host name %q cannot go in a resource file", n.Hostname)
		case n.NodeID < 0 || n.NodeID > 31:
			return refuse("node ID %d is out of range", n.NodeID)
		case !ok || !byIDName.MatchString(name):
			return refuse("data disk %q is not a /dev/disk/by-id path: a kernel name can name another disk after a reboot", n.Disk)
		}
		if _, err := netip.ParseAddr(n.Address); err != nil {
			return refuse("address %q of %s is not an IP address", n.Address, n.Hostname)
		}
	}
	if spec.Self.Hostname == spec.Peer.Hostname {
		return refuse("both nodes are named %q", spec.Self.Hostname)
	}
	return nil
}

func validateTeardown(spec model.ReplicationTeardownSpec) error {
	return validateCommon(spec.Resource, spec.MountPoint, spec.ServiceUnit)
}

func validateCommon(resource, mountPoint, unit string) error {
	switch {
	case !resourcePattern.MatchString(resource):
		return refuse("%q is not a replication resource name", resource)
	case !strings.HasPrefix(mountPoint, "/") || path.Clean(mountPoint) != mountPoint || mountPoint == "/" ||
		strings.ContainsAny(mountPoint, " \t\n\"'\\%"):
		return refuse("mount point %q must be a clean absolute path other than /", mountPoint)
	case !unitPattern.MatchString(unit):
		return refuse("%q is not a service unit name", unit)
	}
	return nil
}

// lsblkDevice is the part of lsblk's JSON the preflight reads.
type lsblkDevice struct {
	Name        string        `json:"name"`
	Type        string        `json:"type"`
	Size        *int64        `json:"size"`
	Mountpoints []*string     `json:"mountpoints"`
	Children    []lsblkDevice `json:"children"`
}

func (d lsblkDevice) mountpoints() []string {
	var out []string
	for _, m := range d.Mountpoints {
		if m != nil && *m != "" {
			out = append(out, *m)
		}
	}
	return out
}

func parseLsblk(out []byte) (lsblkDevice, error) {
	var doc struct {
		BlockDevices []lsblkDevice `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return lsblkDevice{}, err
	}
	if len(doc.BlockDevices) != 1 {
		return lsblkDevice{}, fmt.Errorf("lsblk reported %d devices for one disk", len(doc.BlockDevices))
	}
	return doc.BlockDevices[0], nil
}

// exitCode returns the exit status a command error carries, if any.
func exitCode(err error) (int, bool) {
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		return ec.ExitCode(), true
	}
	return 0, false
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
