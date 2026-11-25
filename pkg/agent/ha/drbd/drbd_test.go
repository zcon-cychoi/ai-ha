package drbd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cloud-barista/ai-ha/pkg/backup/sysinfo"
)

// The fixtures are "drbdsetup status --json r0" exactly as captured on AWS EC2
// during the manual verification (plan/ha-h0.md, second run): DRBD 9.3.4,
// drbd-utils 9.36.0-rc.1, an 8 GiB volume replicated between two nodes.

type fakeRunner struct {
	out   []byte
	err   error
	calls [][]string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return f.out, f.err
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func kib(n int64) *int64 {
	b := n * 1024
	return &b
}

func TestStatusFromCapturedOutput(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		want    Status
	}{
		{"new-connecting.json", Status{
			Role: "Secondary", PeerRole: "Unknown", Connection: "Connecting", Replication: "Off",
			Disk: "Inconsistent", PeerDisk: "DUnknown", OutOfSyncBytes: kib(8388316)}},
		{"syncing-source.json", Status{
			Role: "Primary", PeerRole: "Secondary", Connection: "Connected", Replication: "SyncSource",
			Disk: "UpToDate", PeerDisk: "Inconsistent", OutOfSyncBytes: kib(7805660)}},
		{"syncing-target.json", Status{
			Role: "Secondary", PeerRole: "Primary", Connection: "Connected", Replication: "SyncTarget",
			Disk: "Inconsistent", PeerDisk: "UpToDate", OutOfSyncBytes: kib(7723740)}},
		{"established-primary.json", Status{
			Role: "Primary", PeerRole: "Secondary", Connection: "Connected", Replication: "Established",
			Disk: "UpToDate", PeerDisk: "UpToDate", OutOfSyncBytes: kib(0)}},
		{"established-secondary.json", Status{
			Role: "Secondary", PeerRole: "Primary", Connection: "Connected", Replication: "Established",
			Disk: "UpToDate", PeerDisk: "UpToDate", OutOfSyncBytes: kib(0)}},
		// A disconnect the primary asked for marks the secondary Outdated.
		{"graceful-disconnect-primary.json", Status{
			Role: "Primary", PeerRole: "Unknown", Connection: "StandAlone", Replication: "Off",
			Disk: "UpToDate", PeerDisk: "DUnknown", OutOfSyncBytes: kib(0)}},
		{"graceful-disconnect-secondary.json", Status{
			Role: "Secondary", PeerRole: "Unknown", Connection: "Connecting", Replication: "Off",
			Disk: "Outdated", PeerDisk: "DUnknown", OutOfSyncBytes: kib(0)}},
		// A lost link does not: the secondary keeps reporting UpToDate while
		// the primary writes on. Why a failover cannot prove the standby is
		// current (plan/ha-roadmap.md section 4, item 11).
		{"link-lost-primary.json", Status{
			Role: "Primary", PeerRole: "Unknown", Connection: "Connecting", Replication: "Off",
			Disk: "UpToDate", PeerDisk: "DUnknown", OutOfSyncBytes: kib(808)}},
		{"link-lost-secondary.json", Status{
			Role: "Secondary", PeerRole: "Unknown", Connection: "Connecting", Replication: "Off",
			Disk: "UpToDate", PeerDisk: "DUnknown", OutOfSyncBytes: kib(0)}},
		{"split-brain-primary.json", Status{
			Role: "Primary", PeerRole: "Unknown", Connection: "StandAlone", Replication: "Off",
			Disk: "UpToDate", PeerDisk: "DUnknown", OutOfSyncBytes: kib(11944)}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			runner := &fakeRunner{out: fixture(t, tc.fixture)}
			got, err := NewReader(runner).Status(context.Background(), "r0")
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if !sameStatus(got, tc.want) {
				t.Errorf("Status = %s, want %s", show(got), show(tc.want))
			}
			wantCall := []string{"drbdsetup", "status", "--json", "r0"}
			if len(runner.calls) != 1 || !slices.Equal(runner.calls[0], wantCall) {
				t.Errorf("calls = %v, want %v", runner.calls, wantCall)
			}
		})
	}
}

func TestStatusRefusesWhatItCannotRead(t *testing.T) {
	// edit returns the established primary's output changed by f.
	edit := func(t *testing.T, f func(res map[string]any)) []byte {
		t.Helper()
		var doc []map[string]any
		if err := json.Unmarshal(fixture(t, "established-primary.json"), &doc); err != nil {
			t.Fatalf("decode fixture: %v", err)
		}
		f(doc[0])
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return b
	}
	firstConn := func(res map[string]any) map[string]any {
		return res["connections"].([]any)[0].(map[string]any)
	}

	for _, tc := range []struct {
		name string
		out  func(t *testing.T) []byte
		want string
	}{
		{"not JSON", func(*testing.T) []byte { return []byte("r0 role:Primary") }, "invalid character"},
		{"no resource", func(*testing.T) []byte { return []byte("[]") }, "did not report resource r0"},
		{"another resource", func(t *testing.T) []byte {
			return edit(t, func(res map[string]any) { res["name"] = "r1" })
		}, "did not report resource r0"},
		{"two volumes", func(t *testing.T) []byte {
			return edit(t, func(res map[string]any) {
				devs := res["devices"].([]any)
				res["devices"] = append(devs, devs[0])
			})
		}, "2 volumes"},
		{"no peer", func(t *testing.T) []byte {
			return edit(t, func(res map[string]any) { res["connections"] = []any{} })
		}, "0 peers"},
		{"no peer device for the volume", func(t *testing.T) []byte {
			return edit(t, func(res map[string]any) { firstConn(res)["peer_devices"] = []any{} })
		}, "incomplete status"},
		{"a renamed field", func(t *testing.T) []byte {
			return edit(t, func(res map[string]any) {
				conn := firstConn(res)
				conn["connection"] = conn["connection-state"]
				delete(conn, "connection-state")
			})
		}, "incomplete status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewReader(&fakeRunner{out: tc.out(t)}).Status(context.Background(), "r0")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestStatusWithoutOutOfSync(t *testing.T) {
	var doc []map[string]any
	if err := json.Unmarshal(fixture(t, "established-primary.json"), &doc); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	pd := doc[0]["connections"].([]any)[0].(map[string]any)["peer_devices"].([]any)[0].(map[string]any)
	delete(pd, "out-of-sync")
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	got, err := NewReader(&fakeRunner{out: out}).Status(context.Background(), "r0")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	// Unknown is not zero: a switch has to see zero reported.
	if got.OutOfSyncBytes != nil {
		t.Errorf("OutOfSyncBytes = %d, want nil", *got.OutOfSyncBytes)
	}
}

func TestStatusChecksTheNameBeforeRunning(t *testing.T) {
	for _, name := range []string{"", "-r0", "--json", "r0 r1", "../r0", strings.Repeat("r", 64)} {
		runner := &fakeRunner{}
		if _, err := NewReader(runner).Status(context.Background(), name); err == nil {
			t.Errorf("Status(%q) succeeded", name)
		}
		if len(runner.calls) != 0 {
			t.Errorf("Status(%q) ran %v", name, runner.calls)
		}
	}
}

func TestStatusKeepsTheCommandError(t *testing.T) {
	missing := fmt.Errorf("drbdsetup: %w", sysinfo.ErrCommandNotFound)
	_, err := NewReader(&fakeRunner{err: missing}).Status(context.Background(), "r0")
	if !errors.Is(err, sysinfo.ErrCommandNotFound) {
		t.Errorf("err = %v, want it to wrap ErrCommandNotFound", err)
	}
}

func sameStatus(a, b Status) bool {
	if (a.OutOfSyncBytes == nil) != (b.OutOfSyncBytes == nil) {
		return false
	}
	if a.OutOfSyncBytes != nil && *a.OutOfSyncBytes != *b.OutOfSyncBytes {
		return false
	}
	a.OutOfSyncBytes, b.OutOfSyncBytes = nil, nil
	return a == b
}

func show(s Status) string {
	oos := "nil"
	if s.OutOfSyncBytes != nil {
		oos = fmt.Sprint(*s.OutOfSyncBytes)
	}
	return fmt.Sprintf("{%s/%s %s/%s %s/%s oos=%s}", s.Role, s.PeerRole, s.Connection, s.Replication, s.Disk, s.PeerDisk, oos)
}
