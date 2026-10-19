package inventory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeStore holds one system's inventory in memory.
type fakeStore struct {
	stored  map[string]Stored
	putErr  error
	getErr  error
	puts    int
	lastNow time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{stored: map[string]Stored{}, lastNow: time.Now().UTC()}
}

func (f *fakeStore) Put(_ context.Context, systemID, agentID string, inv Inventory) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.puts++
	f.stored[systemID] = Stored{
		SystemID:    systemID,
		AgentID:     agentID,
		CollectedAt: inv.CollectedAt,
		ReportedAt:  f.lastNow,
		Complete:    inv.Complete,
		Inventory:   inv,
		Now:         f.lastNow,
	}
	return nil
}

func (f *fakeStore) Get(_ context.Context, systemID string) (Stored, error) {
	if f.getErr != nil {
		return Stored{}, f.getErr
	}
	s, ok := f.stored[systemID]
	if !ok {
		return Stored{}, ErrNoInventory
	}
	s.Now = f.lastNow
	return s, nil
}

const testSystemID = "sys-0123456789abcdef"
const testAgentID = "agent-0123456789abcd"

func part(device, mountPoint, fsUUID string, candidate, selectable bool, reason string) Partition {
	return Partition{
		Device: device, MountPoint: mountPoint, FSType: "ext4", FSUUID: fsUUID,
		Candidate: candidate, Selectable: selectable, Reason: reason,
	}
}

// healthy is an Ubuntu 24.04 system with a root filesystem and an ESP, plus one
// partition the agent recorded as unusable.
func healthy() Inventory {
	return Inventory{
		CollectedAt: time.Now().UTC().Add(-time.Minute),
		Firmware:    "uefi",
		Complete:    true,
		Partitions: []Partition{
			part("/dev/nvme0n1p1", "/", "uuid-root", true, true, ""),
			part("/dev/nvme0n1p16", "/boot/efi", "uuid-esp", true, true, ""),
			part("/dev/loop1", "/snap/core22/1122", "", false, false,
				"squashfs is not a filesystem this agent captures"),
		},
	}
}

func serviceWith(t *testing.T, inv Inventory) (*Service, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	svc := NewService(store)
	if err := svc.Report(context.Background(), testSystemID, testAgentID, inv); err != nil {
		t.Fatalf("Report: %v", err)
	}
	return svc, store
}

func TestBackupDevicesChoosesTheCandidates(t *testing.T) {
	svc, _ := serviceWith(t, healthy())

	devices, expected, stored, err := svc.BackupDevices(context.Background(), testSystemID)
	if err != nil {
		t.Fatalf("BackupDevices: %v", err)
	}
	if len(devices) != 2 || devices[0] != "/dev/nvme0n1p1" || devices[1] != "/dev/nvme0n1p16" {
		t.Errorf("devices = %v, want the root filesystem and the ESP", devices)
	}
	if expected["/dev/nvme0n1p1"] != "uuid-root" {
		t.Errorf("expected UUIDs = %v, want the root filesystem's", expected)
	}
	if stored.AgentID != testAgentID {
		t.Errorf("AgentID = %q, want %q", stored.AgentID, testAgentID)
	}
}

// Choosing targets from an inventory the agent could not complete would
// produce a backup that succeeds and a restore that fails.
func TestBackupDevicesRefusesAnIncompleteInventory(t *testing.T) {
	inv := healthy()
	inv.Complete = false
	inv.Missing = []string{"/boot/efi"}
	inv.IncompleteReason = "fstab declares /boot/efi, which is not mounted"
	svc, _ := serviceWith(t, inv)

	_, _, _, err := svc.BackupDevices(context.Background(), testSystemID)
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v, want ErrIncomplete", err)
	}
	if !strings.Contains(err.Error(), "/boot/efi") {
		t.Errorf("err = %q, want it to carry the agent's reason", err)
	}
}

// Completeness requires a root filesystem, so the two should not disagree. If
// they ever do, refusing beats producing an empty backup.
func TestBackupDevicesRefusesACompleteInventoryWithNoCandidates(t *testing.T) {
	inv := healthy()
	for i := range inv.Partitions {
		inv.Partitions[i].Candidate = false
	}
	svc, _ := serviceWith(t, inv)

	if _, _, _, err := svc.BackupDevices(context.Background(), testSystemID); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v, want ErrIncomplete", err)
	}
}

func TestBackupDevicesWithoutAnInventory(t *testing.T) {
	svc := NewService(newFakeStore())

	if _, _, _, err := svc.BackupDevices(context.Background(), testSystemID); !errors.Is(err, ErrNoInventory) {
		t.Fatalf("err = %v, want ErrNoInventory", err)
	}
}

func TestValidateDevicesAcceptsWhatIsSelectable(t *testing.T) {
	svc, _ := serviceWith(t, healthy())

	expected, _, err := svc.ValidateDevices(context.Background(), testSystemID,
		[]string{"/dev/nvme0n1p1", "/dev/nvme0n1p16"})
	if err != nil {
		t.Fatalf("ValidateDevices: %v", err)
	}
	if expected["/dev/nvme0n1p16"] != "uuid-esp" {
		t.Errorf("expected UUIDs = %v, want the ESP's", expected)
	}
}

func TestValidateDevicesRefusesADeviceThatIsNotListed(t *testing.T) {
	svc, _ := serviceWith(t, healthy())

	_, _, err := svc.ValidateDevices(context.Background(), testSystemID, []string{"/dev/nonexistent"})
	if !errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("err = %v, want ErrUnknownDevice", err)
	}
	if !strings.Contains(err.Error(), "/dev/nonexistent") {
		t.Errorf("err = %q, want it to name the device", err)
	}
}

// The inventory records unusable partitions on purpose, so that an operator can
// see why they were left out. Being listed must not become permission.
func TestValidateDevicesRefusesAListedButUnusableDevice(t *testing.T) {
	svc, _ := serviceWith(t, healthy())

	_, _, err := svc.ValidateDevices(context.Background(), testSystemID, []string{"/dev/loop1"})
	if !errors.Is(err, ErrNotSelectable) {
		t.Fatalf("err = %v, want ErrNotSelectable", err)
	}
	if !strings.Contains(err.Error(), "squashfs") {
		t.Errorf("err = %q, want it to carry the agent's reason", err)
	}
}

// An incomplete inventory still says which devices exist, so naming them by
// hand stays possible: an operator may know something fstab does not.
func TestValidateDevicesWorksOnAnIncompleteInventory(t *testing.T) {
	inv := healthy()
	inv.Complete = false
	svc, _ := serviceWith(t, inv)

	if _, _, err := svc.ValidateDevices(context.Background(), testSystemID, []string{"/dev/nvme0n1p1"}); err != nil {
		t.Fatalf("ValidateDevices: %v", err)
	}
}

func TestReportRefusesWhatCannotBeStoredSafely(t *testing.T) {
	now := time.Now().UTC()
	twoDevices := func(device string) []Partition {
		return []Partition{
			part("/dev/nvme0n1p1", "/", "uuid-root", true, true, ""),
			part(device, "/boot/efi", "uuid-esp", true, true, ""),
		}
	}

	for _, c := range []struct {
		name string
		inv  Inventory
	}{
		{"no partitions", Inventory{CollectedAt: now}},
		{"the same device offered twice", Inventory{CollectedAt: now, Partitions: twoDevices("/dev/nvme0n1p1")}},
		{"no collection time", Inventory{Partitions: twoDevices("/dev/nvme0n1p16")}},
		{"a collection time in the future", Inventory{
			CollectedAt: now.Add(time.Hour), Partitions: twoDevices("/dev/nvme0n1p16")}},
		{"a collection time from last week", Inventory{
			CollectedAt: now.Add(-7 * 24 * time.Hour), Partitions: twoDevices("/dev/nvme0n1p16")}},
		{"a partition with no mount point", Inventory{
			CollectedAt: now, Partitions: []Partition{part("/dev/nvme0n1p1", "", "", true, true, "")}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := newFakeStore()
			err := NewService(store).Report(context.Background(), testSystemID, testAgentID, c.inv)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if store.puts != 0 {
				t.Error("a rejected report reached the store")
			}
		})
	}
}

func TestReportRefusesTooManyPartitions(t *testing.T) {
	inv := Inventory{CollectedAt: time.Now().UTC()}
	for i := range maxPartitions + 1 {
		inv.Partitions = append(inv.Partitions, Partition{MountPoint: "/mnt/" + string(rune('a'+i%26))})
	}

	store := newFakeStore()
	if err := NewService(store).Report(context.Background(), testSystemID, testAgentID, inv); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if store.puts != 0 {
		t.Error("a rejected report reached the store")
	}
}

// The system is the caller's, never the report's.
func TestReportNeedsAnIdentifiedCaller(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)

	for _, c := range []struct{ systemID, agentID string }{
		{"", testAgentID},
		{testSystemID, ""},
	} {
		if err := svc.Report(context.Background(), c.systemID, c.agentID, healthy()); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Report(%q, %q) err = %v, want ErrInvalidRequest", c.systemID, c.agentID, err)
		}
	}
	if store.puts != 0 {
		t.Error("a rejected report reached the store")
	}
}

func TestStoredAge(t *testing.T) {
	svc, store := serviceWith(t, healthy())
	store.lastNow = store.lastNow.Add(48 * time.Hour)

	stored, err := svc.Get(context.Background(), testSystemID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Age() < svc.StaleAfter() {
		t.Errorf("Age = %s, want it past the stale threshold of %s", stored.Age(), svc.StaleAfter())
	}
}

func TestStoredSelectable(t *testing.T) {
	svc, _ := serviceWith(t, healthy())

	stored, err := svc.Get(context.Background(), testSystemID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	selectable := stored.Selectable()
	if len(selectable) != 2 {
		t.Fatalf("Selectable = %d partitions, want 2", len(selectable))
	}
	for _, p := range selectable {
		if p.Device == "/dev/loop1" {
			t.Error("an unusable partition is offered as selectable")
		}
	}
}

// One filesystem can be mounted in several places. The agent reports the extra
// mounts with the reason they are not backed up, so that an operator can tell
// an exclusion from an absence — and a system with any bind mount of a real
// filesystem has to be able to report at all.
func TestReportAcceptsARepeatedDeviceThatIsNotOffered(t *testing.T) {
	inv := healthy()
	inv.Partitions = append(inv.Partitions, Partition{
		Device: "/dev/nvme0n1p1", MountPoint: "/var/snap/copy", FSType: "ext4",
		Candidate: false, Selectable: false,
		Reason: "a bind mount of a filesystem listed elsewhere",
	})

	store := newFakeStore()
	svc := NewService(store)
	if err := svc.Report(context.Background(), testSystemID, testAgentID, inv); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if store.puts != 1 {
		t.Fatalf("puts = %d, want 1", store.puts)
	}

	// The bind mount must not shadow the partition itself.
	expected, _, err := svc.ValidateDevices(context.Background(), testSystemID, []string{"/dev/nvme0n1p1"})
	if err != nil {
		t.Fatalf("ValidateDevices: %v", err)
	}
	if expected["/dev/nvme0n1p1"] != "uuid-root" {
		t.Errorf("expected UUIDs = %v, want the root filesystem's", expected)
	}

	// And automatic selection must still name it once.
	devices, _, _, err := svc.BackupDevices(context.Background(), testSystemID)
	if err != nil {
		t.Fatalf("BackupDevices: %v", err)
	}
	if len(devices) != 2 {
		t.Errorf("devices = %v, want the root filesystem and the ESP, each once", devices)
	}
}

// The order the rows arrive in must not decide the answer.
func TestValidateDevicesPrefersTheSelectableRowWhicheverComesFirst(t *testing.T) {
	bind := Partition{
		Device: "/dev/nvme0n1p1", MountPoint: "/var/snap/copy", FSType: "ext4",
		Reason: "a bind mount of a filesystem listed elsewhere",
	}

	for _, c := range []struct {
		name  string
		first bool
	}{{"the bind mount first", true}, {"the bind mount last", false}} {
		t.Run(c.name, func(t *testing.T) {
			inv := healthy()
			if c.first {
				inv.Partitions = append([]Partition{bind}, inv.Partitions...)
			} else {
				inv.Partitions = append(inv.Partitions, bind)
			}
			svc, _ := serviceWith(t, inv)

			if _, _, err := svc.ValidateDevices(context.Background(), testSystemID,
				[]string{"/dev/nvme0n1p1"}); err != nil {
				t.Fatalf("ValidateDevices: %v", err)
			}
		})
	}
}
