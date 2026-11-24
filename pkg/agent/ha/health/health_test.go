package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/sysinfo"
)

// fakeRunner answers commands from a table and records what it was asked.
type fakeRunner struct {
	mu      sync.Mutex
	outputs map[string]string
	errs    map[string]error
	calls   [][]string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	if err := f.errs[name]; err != nil {
		return nil, err
	}
	return []byte(f.outputs[name]), nil
}

// secondaryInSync is a standby's "drbdsetup status --json", cut down to the
// fields read. pkg/agent/ha/drbd tests the reading against captured output.
const secondaryInSync = `[{"name":"r0","role":"Secondary",
  "devices":[{"volume":0,"disk-state":"UpToDate"}],
  "connections":[{"peer-role":"Primary","connection-state":"Connected",
    "peer_devices":[{"volume":0,"replication-state":"Established","peer-disk-state":"UpToDate","out-of-sync":0}]}]}]`

func healthyRunner() *fakeRunner {
	return &fakeRunner{outputs: map[string]string{
		"systemctl":  "LoadState=loaded\nActiveState=active\nSubState=running\n",
		"nvidia-smi": "0\n1\n",
		"drbdsetup":  secondaryInSync,
	}}
}

// serve starts a local health API answering status at /healthz and returns
// its port.
func serve(t *testing.T, status int) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(status)
		case "/moved":
			http.Redirect(w, r, "/healthz", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %s: %v", srv.URL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %s: %v", srv.URL, err)
	}
	return port
}

// closedPort returns a local port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func assignment(port int, role string) *model.HAAssignment {
	a := &model.HAAssignment{
		GroupID: "6a7b8c9d0e1f2a3b", Role: role,
		Service:  model.HAServiceSpec{Unit: "ai.service", Port: port, HealthPath: "/healthz", ExpectedGPUs: 2},
		Resource: model.HAAssignedResource{Name: "r0", Device: "/dev/drbd0", MountPoint: "/data"},
	}
	if role == "standby" {
		a.Probe = &model.HAProbeTarget{Address: "127.0.0.1", Port: port, HealthPath: "/healthz"}
	}
	return a
}

func TestMeasureWithoutAnAssignment(t *testing.T) {
	runner := healthyRunner()
	req := NewCollector(runner).Measure(context.Background(), nil)

	if req.CollectedAt.IsZero() || req.GroupID != "" || req.Role != "" {
		t.Errorf("req = %+v", req)
	}
	if req.Service != nil || req.Port != nil || req.GPU != nil || len(runner.calls) != 0 {
		t.Errorf("measured without an assignment: %+v, calls %v", req, runner.calls)
	}
}

func TestMeasureHealthyStandby(t *testing.T) {
	port := serve(t, http.StatusOK)
	runner := healthyRunner()
	req := NewCollector(runner).Measure(context.Background(), assignment(port, "standby"))

	if req.GroupID != "6a7b8c9d0e1f2a3b" || req.Role != "standby" {
		t.Errorf("assignment echo = %q/%q", req.GroupID, req.Role)
	}
	if req.Service == nil || req.Service.ActiveState != "active" || req.Service.SubState != "running" || req.Service.Error != "" {
		t.Errorf("service = %+v", req.Service)
	}
	if req.Port == nil || !req.Port.Open {
		t.Errorf("port = %+v", req.Port)
	}
	if req.HealthAPI == nil || req.HealthAPI.StatusCode != http.StatusOK || req.HealthAPI.Error != "" {
		t.Errorf("health = %+v", req.HealthAPI)
	}
	if req.PeerPort == nil || !req.PeerPort.Open || req.PeerHealthAPI == nil || req.PeerHealthAPI.StatusCode != http.StatusOK {
		t.Errorf("probe = %+v / %+v", req.PeerPort, req.PeerHealthAPI)
	}
	if req.GPU == nil || req.GPU.Count != 2 {
		t.Errorf("gpu = %+v", req.GPU)
	}
	rep := req.Replication
	if rep == nil || rep.Error != "" || rep.Role != "Secondary" || rep.PeerRole != "Primary" ||
		rep.Connection != "Connected" || rep.Sync != "Established" ||
		rep.Disk != "UpToDate" || rep.PeerDisk != "UpToDate" {
		t.Errorf("replication = %+v", rep)
	} else if rep.OutOfSyncBytes == nil || *rep.OutOfSyncBytes != 0 {
		t.Errorf("out of sync = %v, want a reported 0", rep.OutOfSyncBytes)
	}

	for _, want := range [][]string{
		{"systemctl", "show", "--property=LoadState,ActiveState,SubState", "--", "ai.service"},
		{"drbdsetup", "status", "--json", "r0"},
	} {
		if !slices.ContainsFunc(runner.calls, func(c []string) bool { return slices.Equal(c, want) }) {
			t.Errorf("calls = %v, want %v", runner.calls, want)
		}
	}
}

func TestMeasureSkipsWhatIsNotAsked(t *testing.T) {
	port := serve(t, http.StatusOK)
	a := assignment(port, "primary")
	a.Service.HealthPath, a.Service.ExpectedGPUs = "", 0
	runner := healthyRunner()

	req := NewCollector(runner).Measure(context.Background(), a)
	if req.HealthAPI != nil || req.GPU != nil || req.PeerPort != nil || req.PeerHealthAPI != nil {
		t.Errorf("req = %+v", req)
	}
	if slices.ContainsFunc(runner.calls, func(c []string) bool { return c[0] == "nvidia-smi" }) {
		t.Errorf("nvidia-smi ran with no GPUs expected")
	}
}

func TestMeasureFailures(t *testing.T) {
	t.Run("a closed port", func(t *testing.T) {
		req := NewCollector(healthyRunner()).Measure(context.Background(), assignment(closedPort(t), "primary"))
		if req.Port == nil || req.Port.Open || req.Port.Error == "" {
			t.Errorf("port = %+v", req.Port)
		}
		if req.HealthAPI == nil || req.HealthAPI.StatusCode != 0 || req.HealthAPI.Error == "" {
			t.Errorf("health = %+v", req.HealthAPI)
		}
	})

	t.Run("an unhealthy answer is reported, not judged", func(t *testing.T) {
		req := NewCollector(healthyRunner()).Measure(context.Background(), assignment(serve(t, 503), "primary"))
		if req.HealthAPI == nil || req.HealthAPI.StatusCode != 503 || req.HealthAPI.Error != "" {
			t.Errorf("health = %+v", req.HealthAPI)
		}
	})

	t.Run("a redirect is not followed", func(t *testing.T) {
		a := assignment(serve(t, http.StatusOK), "primary")
		a.Service.HealthPath = "/moved"
		req := NewCollector(healthyRunner()).Measure(context.Background(), a)
		if req.HealthAPI == nil || req.HealthAPI.StatusCode != http.StatusFound {
			t.Errorf("health = %+v", req.HealthAPI)
		}
	})

	t.Run("missing tools are named", func(t *testing.T) {
		runner := &fakeRunner{errs: map[string]error{
			"systemctl":  fmt.Errorf("systemctl: %w", sysinfo.ErrCommandNotFound),
			"nvidia-smi": fmt.Errorf("nvidia-smi: %w", sysinfo.ErrCommandNotFound),
			"drbdsetup":  fmt.Errorf("drbdsetup: %w", sysinfo.ErrCommandNotFound),
		}}
		req := NewCollector(runner).Measure(context.Background(), assignment(closedPort(t), "primary"))
		if req.Service == nil || req.Service.Error != "systemctl is not installed" {
			t.Errorf("service = %+v", req.Service)
		}
		if req.GPU == nil || req.GPU.Error != "nvidia-smi is not installed" {
			t.Errorf("gpu = %+v", req.GPU)
		}
		if req.Replication == nil || req.Replication.Error != "drbdsetup is not installed" {
			t.Errorf("replication = %+v", req.Replication)
		}
	})

	t.Run("replication that cannot be read carries only the reason", func(t *testing.T) {
		runner := healthyRunner()
		runner.outputs["drbdsetup"] = "r0 role:Secondary"
		req := NewCollector(runner).Measure(context.Background(), assignment(closedPort(t), "primary"))
		rep := req.Replication
		if rep == nil || rep.Error == "" || rep.Role != "" || rep.Connection != "" || rep.OutOfSyncBytes != nil {
			t.Errorf("replication = %+v", rep)
		}
	})

	t.Run("a failing tool's error is clipped to what the framework accepts", func(t *testing.T) {
		runner := healthyRunner()
		runner.errs = map[string]error{"nvidia-smi": errors.New(strings.Repeat("x", 5000))}
		req := NewCollector(runner).Measure(context.Background(), assignment(closedPort(t), "primary"))
		if req.GPU == nil || len(req.GPU.Error) != maxErrorLen {
			t.Errorf("gpu error length = %d", len(req.GPU.Error))
		}
	})

	t.Run("a unit with no state", func(t *testing.T) {
		runner := &fakeRunner{outputs: map[string]string{"systemctl": "LoadState=not-found\n"}}
		req := NewCollector(runner).Measure(context.Background(), assignment(closedPort(t), "primary"))
		if req.Service == nil || req.Service.LoadState != "not-found" || req.Service.Error == "" {
			t.Errorf("service = %+v", req.Service)
		}
	})
}

func TestClip(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly", 7, "exactly"},
		{"abcdef", 3, "abc"},
		{"가나다", 4, "가"}, // each is 3 bytes: the second cannot be split
		{"가나다", 6, "가나"},
	} {
		got := clip(tc.in, tc.n)
		if got != tc.want || !utf8.ValidString(got) {
			t.Errorf("clip(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
