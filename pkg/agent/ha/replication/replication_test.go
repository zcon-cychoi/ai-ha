package replication

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
)

const (
	testSecret   = "0123456789abcdef0123456789abcdef0123456789abcdef"
	testResource = "aiha-6a7b8c9d"
	testDisk     = "/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol0aaa"
	peerDisk     = "/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol0bbb"
	testService  = "ai-service.service"
	mountUnit    = "data.mount"
	// The per-resource units as systemd names them: the hyphen of the
	// resource name is escaped.
	testTarget      = `drbd@aiha\x2d6a7b8c9d.target`
	testDRBDService = `drbd@aiha\x2d6a7b8c9d.service`
	diskSize        = int64(8 << 30)
)

// exitErr is a command that exited with a status, as *exec.ExitError is.
type exitErr int

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

// unitProps is a unit as systemctl show reports it.
type unitProps struct{ load, active, file, triggeredBy string }

// node simulates the commands setup and teardown run. It answers from its
// state and changes it the way the real command would, so that a second run
// over the same node sees what the first one left.
type node struct {
	t    *testing.T
	root string

	diskType     string
	diskChildren []string // names of devices on the disk, e.g. "nvme1n1p1"
	diskMounts   []string
	diskSig      string // what blkid finds on the disk
	volumeSig    string // what blkid finds on the replicated device

	units map[string]*unitProps

	up         bool
	role       string
	disk       string
	peerRole   string
	peerDisk   string
	connection string
	// connectAfter is how many status reads a link takes to come up.
	connectAfter int

	calls []string
	// fail makes a command fail, matched by prefix.
	fail map[string]error
}

func newNode(t *testing.T) *node {
	t.Helper()
	root := t.TempDir()
	byID := filepath.Join(root, strings.TrimPrefix(testDisk, "/"))
	if err := os.MkdirAll(filepath.Dir(byID), 0o755); err != nil {
		t.Fatal(err)
	}
	// A dangling link is enough: lsblk, which is simulated, does the rest.
	if err := os.Symlink("../../nvme1n1", byID); err != nil {
		t.Fatal(err)
	}
	return &node{
		t:        t,
		root:     root,
		diskType: "disk",
		units: map[string]*unitProps{
			testService:     {load: "loaded", active: "inactive", file: "enabled"},
			mountUnit:       {load: "not-found", active: "inactive"},
			"drbd.service":  {load: "loaded", active: "inactive", file: "disabled"},
			testTarget:      {load: "loaded", active: "inactive", file: "disabled"},
			testDRBDService: {load: "loaded", active: "inactive", file: "static"},
		},
		fail: map[string]error{},
	}
}

func (n *node) configurer(host string) *Configurer {
	c := New(n)
	c.root = n.root
	c.hostname = func() (string, error) { return host, nil }
	c.sleep = func(context.Context, time.Duration) error { return nil }
	c.connectTimeout = time.Hour
	return c
}

func (n *node) loadModule() {
	if err := os.MkdirAll(filepath.Join(n.root, "proc"), 0o755); err != nil {
		n.t.Fatal(err)
	}
	p := filepath.Join(n.root, "proc", "drbd")
	if _, err := os.Stat(p); err == nil {
		return
	}
	if err := os.WriteFile(p, []byte("version: 9.3.4\n"), 0o444); err != nil {
		n.t.Fatal(err)
	}
}

func (n *node) bringUp() {
	n.loadModule()
	if !n.up {
		n.up, n.role, n.disk = true, "Secondary", "Inconsistent"
		if n.connection == "" {
			n.connection = "Connecting"
		}
	}
}

func (n *node) statusJSON() string {
	repl := "Off"
	if n.connection == "Connected" {
		repl = "Established"
	}
	peerRole, peerDisk := n.peerRole, n.peerDisk
	if n.connection != "Connected" {
		peerRole, peerDisk = "Unknown", "DUnknown"
	}
	return fmt.Sprintf(`[{"name":%q,"role":%q,"devices":[{"volume":0,"disk-state":%q}],`+
		`"connections":[{"peer-role":%q,"connection-state":%q,"peer_devices":[{"volume":0,`+
		`"replication-state":%q,"peer-disk-state":%q,"out-of-sync":0}]}]}]`,
		testResource, n.role, n.disk, peerRole, n.connection, repl, peerDisk)
}

// Run implements sysinfo.Runner.
func (n *node) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	n.calls = append(n.calls, cmd)
	for prefix, err := range n.fail {
		if strings.HasPrefix(cmd, prefix) {
			return nil, err
		}
	}

	switch {
	case name == "lsblk":
		var children []string
		for _, c := range n.diskChildren {
			children = append(children, fmt.Sprintf(`{"name":%q,"type":"part","size":1024,"mountpoints":[null]}`, c))
		}
		if n.up {
			children = append(children, `{"name":"drbd0","type":"disk","size":1024,"mountpoints":[null]}`)
		}
		mounts := "null"
		if len(n.diskMounts) > 0 {
			mounts = `"` + strings.Join(n.diskMounts, `","`) + `"`
		}
		return fmt.Appendf(nil, `{"blockdevices":[{"name":"nvme1n1","type":%q,"size":%d,"mountpoints":[%s],"children":[%s]}]}`,
			n.diskType, diskSize, mounts, strings.Join(children, ",")), nil

	case name == "blkid":
		sig := n.diskSig
		if args[len(args)-1] == "/dev/drbd0" {
			sig = n.volumeSig
		}
		if sig == "" {
			return nil, fmt.Errorf("run blkid: %w", exitErr(2))
		}
		return []byte(sig + "\n"), nil

	case name == "systemd-escape":
		if args[len(args)-1] != "/data" {
			n.t.Fatalf("systemd-escape for %s", args[len(args)-1])
		}
		return []byte(mountUnit + "\n"), nil

	case cmd == "systemctl daemon-reload":
		return nil, nil

	case name == "systemctl" && args[0] == "show":
		u := n.units[args[len(args)-1]]
		if u == nil {
			u = &unitProps{load: "not-found", active: "inactive"}
		}
		return fmt.Appendf(nil, "LoadState=%s\nActiveState=%s\nUnitFileState=%s\nTriggeredBy=%s\n",
			u.load, u.active, u.file, u.triggeredBy), nil

	case name == "systemctl" && (args[0] == "start" || args[0] == "stop"):
		for _, unit := range args[1:] {
			if unit == "--" {
				continue
			}
			u := n.units[unit]
			if u == nil {
				n.t.Fatalf("systemctl %s of an unknown unit %s", args[0], unit)
			}
			if args[0] == "start" {
				u.active = "active"
				if unit == testTarget {
					n.bringUp()
				}
			} else {
				u.active = "inactive"
				if unit == testDRBDService {
					n.up = false
				}
			}
		}
		return nil, nil

	case name == "systemctl" && (args[0] == "enable" || args[0] == "disable"):
		state := "enabled"
		if args[0] == "disable" {
			state = "disabled"
		}
		for _, unit := range args[1:] {
			if u := n.units[unit]; u != nil {
				u.file = state
			}
		}
		return nil, nil

	case cmd == "drbdadm dump "+testResource:
		return []byte("resource ... shared-secret " + testSecret), nil
	case cmd == "drbdadm create-md "+testResource:
		n.diskSig = "drbd"
		return nil, nil
	case cmd == "drbdadm adjust "+testResource:
		n.bringUp()
		return nil, nil
	case cmd == "drbdadm primary --force "+testResource:
		n.role, n.disk = "Primary", "UpToDate"
		return nil, nil
	case cmd == "drbdadm primary "+testResource:
		n.role = "Primary"
		return nil, nil
	case cmd == "drbdsetup secondary "+testResource:
		n.role = "Secondary"
		return nil, nil
	case cmd == "drbdsetup down "+testResource:
		n.up = false
		return nil, nil

	case cmd == "drbdsetup status --json":
		if !n.up {
			return []byte("[]"), nil
		}
		return fmt.Appendf(nil, `[{"name":%q}]`, testResource), nil
	case cmd == "drbdsetup status --json "+testResource:
		if !n.up {
			return nil, errors.New("run drbdsetup: exit status 10: " + testResource + ": No such resource")
		}
		if n.connection != "Connected" && n.connectAfter > 0 {
			n.connectAfter--
			if n.connectAfter == 0 {
				n.connection = "Connected"
			}
		}
		return []byte(n.statusJSON()), nil

	case name == "mkfs.ext4":
		n.volumeSig = "ext4"
		return nil, nil
	}
	n.t.Fatalf("unexpected command: %s", cmd)
	return nil, nil
}

// ran reports whether a command starting with prefix was run.
func (n *node) ran(prefix string) bool {
	return slices.ContainsFunc(n.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func standbySpec() model.ReplicationSetupSpec {
	return model.ReplicationSetupSpec{
		FlowID: "4d5e6f7a8b9c0d1e", Step: 1, Resource: testResource, Device: "/dev/drbd0",
		MountPoint: "/data", Port: 7789, Protocol: "C", ServiceUnit: testService,
		Self: model.ReplicationNode{Hostname: "ip-10-0-2-10", Address: "10.0.2.10", NodeID: 1, Disk: testDisk},
		Peer: model.ReplicationNode{Hostname: "ip-10-0-1-10", Address: "10.0.1.10", NodeID: 0, Disk: peerDisk},
	}
}

func primarySpec() model.ReplicationSetupSpec {
	s := standbySpec()
	s.Step = 2
	s.Self, s.Peer = s.Peer, s.Self
	s.Self.Disk, s.Peer.Disk = testDisk, peerDisk
	s.InitialPrimary = true
	s.PeerDiskBytes = diskSize
	return s
}

func stepDone(t *testing.T, res Result, name string) bool {
	t.Helper()
	for _, s := range res.Steps {
		if s.Name == name {
			return s.Done
		}
	}
	t.Fatalf("step %s not reported: %+v", name, res.Steps)
	return false
}

func TestStandbySetupOnABlankDisk(t *testing.T) {
	n := newNode(t)
	res, err := n.configurer("ip-10-0-2-10").Setup(context.Background(), standbySpec(), testSecret)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// The resource file holds the secret, so its mode matters as much as its
	// content. Both nodes write the same file: node 0 comes first.
	p := filepath.Join(n.root, "etc/drbd.d", testResource+".res")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("resource file mode = %o, want 600", info.Mode().Perm())
	}
	text, _ := os.ReadFile(p)
	for _, want := range []string{
		"options { auto-promote no; }", "protocol C;", `shared-secret "` + testSecret + `";`,
		"on ip-10-0-1-10 {\n    node-id   0;", "on ip-10-0-2-10 {\n    node-id   1;",
		"disk      " + peerDisk + ";", "disk      " + testDisk + ";", "address   10.0.2.10:7789;",
	} {
		if !strings.Contains(string(text), want) {
			t.Errorf("resource file lacks %q:\n%s", want, text)
		}
	}
	if strings.Index(string(text), "ip-10-0-1-10") > strings.Index(string(text), "ip-10-0-2-10") {
		t.Errorf("nodes not in node ID order:\n%s", text)
	}
	mount, _ := os.ReadFile(filepath.Join(n.root, "etc/systemd/system", mountUnit))
	if !strings.Contains(string(mount), "What=/dev/drbd0\nWhere=/data\n") || strings.Contains(string(mount), "[Install]") {
		t.Errorf("mount unit:\n%s", mount)
	}
	dropIn, _ := os.ReadFile(filepath.Join(n.root, "etc/systemd/system", testService+".d", dropInName))
	if !strings.Contains(string(dropIn), "RequiresMountsFor=/data") {
		t.Errorf("drop-in:\n%s", dropIn)
	}

	for _, s := range []string{StepResourceFile, StepMountUnit, StepServiceDropIn, StepDisableService, StepCreateMetadata, StepBringUp} {
		if !stepDone(t, res, s) {
			t.Errorf("%s not done on a blank node", s)
		}
	}
	if n.units[testService].file != "disabled" {
		t.Error("service left enabled at boot")
	}
	if n.units[testTarget].file != "enabled" {
		t.Error("resource not brought up at boot")
	}
	// The standby only brings its side up.
	if n.ran("drbdadm primary") || n.ran("mkfs") || n.units[testService].active != "inactive" {
		t.Errorf("standby went further than bringing its side up: %v", n.calls)
	}
	if res.Role != "Secondary" || res.DiskBytes != diskSize {
		t.Errorf("result = %+v", res)
	}
	// Nothing about the job's outcome may carry the secret.
	if strings.Contains(fmt.Sprintf("%+v", res), testSecret) {
		t.Error("the result carries the secret")
	}
}

// Asking again changes nothing on a node already set up: no new metadata, no
// unit reload, no files rewritten.
func TestRepeatedStandbySetupRedoesNothing(t *testing.T) {
	n := newNode(t)
	c := n.configurer("ip-10-0-2-10")
	if _, err := c.Setup(context.Background(), standbySpec(), testSecret); err != nil {
		t.Fatal(err)
	}
	n.calls = nil

	res, err := c.Setup(context.Background(), standbySpec(), testSecret)
	if err != nil {
		t.Fatalf("second Setup: %v", err)
	}
	for _, s := range []string{StepResourceFile, StepMountUnit, StepServiceDropIn, StepDisableService, StepCreateMetadata, StepBringUp} {
		if stepDone(t, res, s) {
			t.Errorf("%s redone", s)
		}
	}
	for _, c := range []string{"drbdadm create-md", "systemctl daemon-reload", "systemctl disable"} {
		if n.ran(c) {
			t.Errorf("ran %q again", c)
		}
	}
}

func TestInitialPrimarySetup(t *testing.T) {
	n := newNode(t)
	n.peerRole, n.peerDisk = "Secondary", "Inconsistent"
	// The peer, set up just before, takes a few looks to connect.
	n.connectAfter = 3
	res, err := n.configurer("ip-10-0-1-10").Setup(context.Background(), primarySpec(), testSecret)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// The order matters: promote, then format, then mount, then the service.
	order := []string{"drbdadm primary --force", "mkfs.ext4 -q -E nodiscard /dev/drbd0",
		"systemctl start -- " + mountUnit, "systemctl start -- " + testService}
	last := -1
	for _, want := range order {
		i := slices.IndexFunc(n.calls, func(c string) bool { return strings.HasPrefix(c, want) })
		if i < 0 || i < last {
			t.Fatalf("%q out of order in %v", want, n.calls)
		}
		last = i
	}
	for _, s := range []string{StepFirstPromotion, StepFormat, StepMount, StepStartService} {
		if !stepDone(t, res, s) {
			t.Errorf("%s not done", s)
		}
	}
	if res.Role != "Primary" {
		t.Errorf("role = %s", res.Role)
	}
}

// A primary set up again after its setup completed, or failed past the
// format, neither promotes with force nor formats again.
func TestRepeatedPrimarySetupDoesNotFormatAgain(t *testing.T) {
	n := newNode(t)
	n.peerRole, n.peerDisk, n.connection = "Secondary", "Inconsistent", "Connected"
	c := n.configurer("ip-10-0-1-10")
	if _, err := c.Setup(context.Background(), primarySpec(), testSecret); err != nil {
		t.Fatal(err)
	}
	n.calls = nil

	res, err := c.Setup(context.Background(), primarySpec(), testSecret)
	if err != nil {
		t.Fatalf("second Setup: %v", err)
	}
	for _, cmd := range []string{"drbdadm primary", "mkfs", "drbdadm create-md", "systemctl start --"} {
		if n.ran(cmd) {
			t.Errorf("ran %q again: %v", cmd, n.calls)
		}
	}
	if stepDone(t, res, StepFormat) || stepDone(t, res, StepPromote) {
		t.Errorf("steps = %+v", res.Steps)
	}
}

// The case in the roadmap: the format succeeded and the mount failed. Asked
// again, setup mounts without formatting.
func TestPrimarySetupAfterAFailedMountMountsWithoutFormatting(t *testing.T) {
	n := newNode(t)
	n.peerRole, n.peerDisk, n.connection = "Secondary", "Inconsistent", "Connected"
	c := n.configurer("ip-10-0-1-10")
	n.fail["systemctl start -- "+mountUnit] = errors.New("run systemctl: exit status 1")
	if _, err := c.Setup(context.Background(), primarySpec(), testSecret); err == nil {
		t.Fatal("Setup succeeded with the mount failing")
	}
	if n.volumeSig != "ext4" {
		t.Fatal("the format did not happen before the mount")
	}
	delete(n.fail, "systemctl start -- "+mountUnit)
	n.calls = nil

	res, err := c.Setup(context.Background(), primarySpec(), testSecret)
	if err != nil {
		t.Fatalf("second Setup: %v", err)
	}
	if n.ran("mkfs") || n.ran("drbdadm primary --force") {
		t.Errorf("formatted or force-promoted again: %v", n.calls)
	}
	if !stepDone(t, res, StepMount) || !stepDone(t, res, StepStartService) {
		t.Errorf("steps = %+v", res.Steps)
	}
}

// A rebooted primary comes back Secondary with its data. Setup asked again
// promotes it the ordinary way.
func TestPrimarySetupPromotesAnUpToDateDiskWithoutForce(t *testing.T) {
	n := newNode(t)
	n.diskSig, n.volumeSig = "drbd", "ext4"
	n.bringUp()
	n.disk, n.peerRole, n.peerDisk, n.connection = "UpToDate", "Secondary", "UpToDate", "Connected"

	res, err := n.configurer("ip-10-0-1-10").Setup(context.Background(), primarySpec(), testSecret)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if n.ran("drbdadm primary --force") || !n.ran("drbdadm primary "+testResource) || n.ran("mkfs") {
		t.Errorf("calls = %v", n.calls)
	}
	if !stepDone(t, res, StepPromote) {
		t.Errorf("steps = %+v", res.Steps)
	}
}

// Refusals leave the node as it was: no file written, nothing run past the
// look.
func TestSetupRefusals(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		spec    func() model.ReplicationSetupSpec
		prepare func(*node)
		want    string
	}{
		{name: "disk holds a filesystem", prepare: func(n *node) { n.diskSig = "ext4" }, want: "holds ext4"},
		{name: "disk is partitioned", prepare: func(n *node) { n.diskChildren = []string{"nvme1n1p1"} }, want: "holds nvme1n1p1"},
		{name: "disk is mounted", prepare: func(n *node) { n.diskMounts = []string{"/mnt"} }, want: "mounted at /mnt"},
		{name: "a partition, not a disk", prepare: func(n *node) { n.diskType = "part" }, want: "not a whole disk"},
		{name: "disk missing", spec: func() model.ReplicationSetupSpec {
			s := standbySpec()
			s.Self.Disk = "/dev/disk/by-id/nvme-gone"
			return s
		}, want: "does not exist"},
		{name: "kernel name", spec: func() model.ReplicationSetupSpec {
			s := standbySpec()
			s.Self.Disk = "/dev/nvme1n1"
			return s
		}, want: "not a /dev/disk/by-id path"},
		{name: "another host", host: "ip-10-0-9-9", want: "the job is for"},
		{name: "service running on a standby", prepare: func(n *node) { n.units[testService].active = "active" }, want: "stop it"},
		{name: "service triggered by a socket", prepare: func(n *node) { n.units[testService].triggeredBy = "ai-service.socket" }, want: "started by ai-service.socket"},
		{name: "service missing", prepare: func(n *node) { n.units[testService].load = "not-found" }, want: "is not-found"},
		{name: "drbd.service enabled", prepare: func(n *node) { n.units["drbd.service"].file = "enabled" }, want: "drbd.service has been enabled"},
		{name: "no per-resource unit", prepare: func(n *node) { n.units[testTarget].load = "not-found" }, want: "install drbd-utils 9"},
		{name: "disks of different sizes", spec: func() model.ReplicationSetupSpec {
			s := standbySpec()
			s.PeerDiskBytes = diskSize * 2
			return s
		}, want: "same size"},
		{name: "secret unusable", spec: func() model.ReplicationSetupSpec { return standbySpec() }, want: "shared secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newNode(t)
			if tc.prepare != nil {
				tc.prepare(n)
			}
			spec := standbySpec()
			if tc.spec != nil {
				spec = tc.spec()
			}
			host := tc.host
			if host == "" {
				host = "ip-10-0-2-10"
			}
			secret := testSecret
			if tc.name == "secret unusable" {
				secret = `x" ; }`
			}
			_, err := n.configurer(host).Setup(context.Background(), spec, secret)
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a refusal mentioning %q", err, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(n.root, "etc")); statErr == nil {
				t.Error("files were written before the refusal")
			}
			for _, c := range []string{"drbdadm", "systemctl disable", "systemctl enable", "systemctl start", "mkfs"} {
				if n.ran(c) {
					t.Errorf("ran %q before refusing", c)
				}
			}
		})
	}
}

// The first promotion declares this node's data the source. It happens only
// when neither side has data yet.
func TestPrimaryRefusesToPromoteOverNewerData(t *testing.T) {
	n := newNode(t)
	n.diskSig = "drbd"
	n.bringUp()
	n.disk, n.peerRole, n.peerDisk, n.connection = "Inconsistent", "Secondary", "UpToDate", "Connected"

	_, err := n.configurer("ip-10-0-1-10").Setup(context.Background(), primarySpec(), testSecret)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "cannot be promoted") {
		t.Fatalf("err = %v", err)
	}
	if n.ran("drbdadm primary") || n.ran("mkfs") {
		t.Errorf("calls = %v", n.calls)
	}
}

func TestPrimaryGivesUpOnAPeerThatNeverConnects(t *testing.T) {
	n := newNode(t)
	c := n.configurer("ip-10-0-1-10")
	c.connectTimeout = 0
	_, err := c.Setup(context.Background(), primarySpec(), testSecret)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "did not connect") {
		t.Fatalf("err = %v", err)
	}
	if n.ran("drbdadm primary") {
		t.Error("promoted without a peer")
	}
}

func TestTeardownOfAPrimary(t *testing.T) {
	n := newNode(t)
	n.peerRole, n.peerDisk, n.connection = "Secondary", "Inconsistent", "Connected"
	c := n.configurer("ip-10-0-1-10")
	if _, err := c.Setup(context.Background(), primarySpec(), testSecret); err != nil {
		t.Fatal(err)
	}
	n.calls = nil

	spec := model.ReplicationTeardownSpec{FlowID: "f", Step: 1, Resource: testResource, MountPoint: "/data", ServiceUnit: testService}
	res, err := c.Teardown(context.Background(), spec)
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	order := []string{"systemctl stop -- " + testService, "systemctl stop -- " + mountUnit,
		"drbdsetup secondary", "systemctl disable drbd@", "systemctl stop drbd@"}
	last := -1
	for _, want := range order {
		i := slices.IndexFunc(n.calls, func(c string) bool { return strings.HasPrefix(c, want) })
		if i < 0 || i < last {
			t.Fatalf("%q out of order in %v", want, n.calls)
		}
		last = i
	}
	if n.up {
		t.Error("resource still up")
	}
	for _, rel := range []string{"etc/drbd.d/" + testResource + ".res", "etc/systemd/system/" + mountUnit,
		"etc/systemd/system/" + testService + ".d"} {
		if _, err := os.Stat(filepath.Join(n.root, rel)); err == nil {
			t.Errorf("%s left behind", rel)
		}
	}
	// The data stays: no wipe of any kind.
	if n.diskSig != "drbd" || n.volumeSig != "ext4" || n.ran("wipefs") || n.ran("drbdadm wipe-md") {
		t.Error("teardown touched the data")
	}
	for _, s := range []string{StepStopService, StepUnmount, StepDemote, StepTakeDown, StepRemoveFiles} {
		if !stepDone(t, res, s) {
			t.Errorf("%s not done", s)
		}
	}

	// And again: nothing is left to do.
	n.calls = nil
	res, err = c.Teardown(context.Background(), spec)
	if err != nil {
		t.Fatalf("second Teardown: %v", err)
	}
	for _, s := range res.Steps {
		if s.Done {
			t.Errorf("%s redone", s.Name)
		}
	}
	if n.ran("drbdsetup down") || n.ran("systemctl daemon-reload") {
		t.Errorf("calls = %v", n.calls)
	}
}

// A resource whose module is not loaded is not up, and drbdsetup is not
// asked: it would try to load the module, which the sandbox refuses.
func TestNothingIsUpWithoutTheModule(t *testing.T) {
	n := newNode(t)
	up, _, err := n.configurer("x").resourceState(context.Background(), testResource)
	if err != nil || up {
		t.Fatalf("up = %v, err = %v", up, err)
	}
	if n.ran("drbdsetup") {
		t.Error("asked drbdsetup with no module loaded")
	}
}
