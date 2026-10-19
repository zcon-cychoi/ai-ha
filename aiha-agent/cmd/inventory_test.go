//go:build !rescue

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/sysinfo"
)

// fakeReporter records what reached the framework and can be made to fail a
// fixed number of times first.
type fakeReporter struct {
	mu        sync.Mutex
	reports   []model.AgentInventoryReq
	failFirst int
	err       error
}

func newFakeReporter(failFirst int) *fakeReporter {
	return &fakeReporter{failFirst: failFirst, err: errors.New("framework unavailable")}
}

func (f *fakeReporter) Inventory(_ context.Context, _ string, req model.AgentInventoryReq) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFirst > 0 {
		f.failFirst--
		return f.err
	}
	f.reports = append(f.reports, req)
	return nil
}

func (f *fakeReporter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reports)
}

func (f *fakeReporter) last() model.AgentInventoryReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reports[len(f.reports)-1]
}

// testResolver answers device-number lookups from a table, so that these tests
// do not depend on the block devices of the machine they run on.
type testResolver map[string]string

func (r testResolver) Resolve(devNo string) (string, error) {
	if path, ok := r[devNo]; ok {
		return path, nil
	}
	return "", errors.New("no such device")
}

func staticReader(req model.AgentInventoryReq) systemReader {
	return func(context.Context) (model.AgentInventoryReq, error) { return req, nil }
}

func failingReader(err error) systemReader {
	return func(context.Context) (model.AgentInventoryReq, error) {
		return model.AgentInventoryReq{}, err
	}
}

func sampleReport() model.AgentInventoryReq {
	return model.AgentInventoryReq{
		CollectedAt: time.Now().UTC(),
		Complete:    true,
		Partitions: []model.InventoryPartition{
			{Device: "/dev/nvme0n1p1", MountPoint: "/", FSType: "ext4", Candidate: true, Selectable: true},
		},
	}
}

// waitFor polls until cond holds or the deadline passes, so that the tests do
// not depend on a fixed sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestReportInventoryReportsOnStartup(t *testing.T) {
	client := newFakeReporter(0)

	go reportInventory(t.Context(), client, staticReader(sampleReport()), "token", time.Hour)

	waitFor(t, "the first report", func() bool { return client.count() > 0 })
	if got := client.last(); len(got.Partitions) != 1 || got.Partitions[0].Device != "/dev/nvme0n1p1" {
		t.Errorf("reported %+v", got)
	}
}

// A new agent whose first report fails must not wait out the whole interval:
// until it reports, a backup of its system cannot be targeted automatically.
func TestReportInventoryRetriesAFailedReportBeforeTheNextTick(t *testing.T) {
	client := newFakeReporter(1)

	// An interval far longer than the test: only the retry path can produce a
	// report here.
	go reportInventory(t.Context(), client, staticReader(sampleReport()), "token", time.Hour)

	waitFor(t, "a report after a failure", func() bool { return client.count() > 0 })
}

// A reading that failed must not be reported. An inventory built from nothing
// would replace a good one and look just as freshly confirmed.
func TestReportInventorySendsNothingWhenTheSystemCannotBeRead(t *testing.T) {
	client := newFakeReporter(0)

	go reportInventory(t.Context(), client, failingReader(sysinfo.ErrNoMountTable), "token", time.Hour)

	time.Sleep(50 * time.Millisecond)
	if n := client.count(); n != 0 {
		t.Errorf("reports = %d, want none: an unreadable system was reported anyway", n)
	}
}

func TestReportInventoryStopsWithItsContext(t *testing.T) {
	client := newFakeReporter(0)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		reportInventory(ctx, client, staticReader(sampleReport()), "token", time.Hour)
	}()

	waitFor(t, "the first report", func() bool { return client.count() > 0 })
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the reporting loop outlived its context")
	}
}

// The loop must also come back from a cancellation that lands while it is
// backing off from a failure.
func TestReportInventoryStopsWhileRetrying(t *testing.T) {
	client := newFakeReporter(1000)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		reportInventory(ctx, client, staticReader(sampleReport()), "token", time.Hour)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the reporting loop did not stop while retrying")
	}
}

// What is sent must carry no file contents. The agent's full system facts
// include /etc/netplan, which may hold a wireless passphrase.
func TestInventoryOfCarriesNoFileContents(t *testing.T) {
	const secret = "super-secret-passphrase"
	info := &sysinfo.SystemInfo{
		CollectedAt: time.Now().UTC(),
		Firmware:    sysinfo.FirmwareUEFI,
		Kernel:      sysinfo.Kernel{Release: "6.8.0-45-generic", Machine: "x86_64"},
		OSRelease:   sysinfo.OSRelease{ID: "ubuntu", VersionID: "24.04", Raw: "ID=ubuntu\n"},
		NetplanFiles: map[string]string{
			"50-cloud-init.yaml": "network:\n  wifis:\n    wlan0:\n      password: " + secret + "\n",
		},
		Fstab: sysinfo.Fstab{Raw: "LABEL=cloudimg-rootfs / ext4 defaults 0 1\n"},
		Mounts: []sysinfo.Mount{
			{DevNo: "259:1", Root: "/", MountPoint: "/", Source: "/dev/root", FSType: "ext4"},
		},
		PartitionTables: map[string]string{"/dev/nvme0n1": "label: gpt\n"},
	}

	req, err := inventoryOf(info, testResolver{"259:1": "/dev/nvme0n1p1"})
	if err != nil {
		t.Fatalf("inventoryOf: %v", err)
	}

	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("encode the report: %v", err)
	}
	for _, leak := range []string{secret, "wifis", "label: gpt", "LABEL=cloudimg-rootfs"} {
		if strings.Contains(string(encoded), leak) {
			t.Errorf("the report carries %q:\n%s", leak, encoded)
		}
	}

	if req.OSID != "ubuntu" || req.Architecture != "x86_64" || req.Firmware != "uefi" {
		t.Errorf("the platform summary is wrong: %+v", req)
	}
	if len(req.Partitions) != 1 {
		t.Fatalf("partitions = %d, want 1", len(req.Partitions))
	}
	// The mount table called it /dev/root, which cannot be handed back as a
	// device to back up. What is reported has to be the real path.
	if device := req.Partitions[0].Device; device != "/dev/nvme0n1p1" {
		t.Errorf("Device = %q, want /dev/nvme0n1p1", device)
	}
}

// A system the agent could not fully account for is still reported, marked so.
func TestInventoryOfMarksAnIncompleteSystem(t *testing.T) {
	info := &sysinfo.SystemInfo{
		CollectedAt: time.Now().UTC(),
		Mounts: []sysinfo.Mount{
			{DevNo: "259:1", Root: "/", MountPoint: "/", Source: "/dev/nvme0n1p1", FSType: "ext4"},
		},
		Fstab: sysinfo.Fstab{Entries: []sysinfo.FstabEntry{
			{MountPoint: "/", FSType: "ext4", Options: "defaults"},
			{MountPoint: "/boot/efi", FSType: "vfat", Options: "umask=0077"},
		}},
	}

	req, err := inventoryOf(info, testResolver{"259:1": "/dev/nvme0n1p1"})
	if err != nil {
		t.Fatalf("inventoryOf: %v", err)
	}
	if req.Complete {
		t.Error("a system missing its ESP was reported as complete")
	}
	if len(req.Missing) != 1 || req.Missing[0] != "/boot/efi" {
		t.Errorf("Missing = %v, want /boot/efi", req.Missing)
	}
	if req.IncompleteReason == "" {
		t.Error("no reason was given for the system being incomplete")
	}
}

func TestInventoryOfWithoutAMountTable(t *testing.T) {
	_, err := inventoryOf(&sysinfo.SystemInfo{CollectedAt: time.Now().UTC()}, testResolver{})
	if !errors.Is(err, sysinfo.ErrNoMountTable) {
		t.Fatalf("err = %v, want ErrNoMountTable", err)
	}
}
