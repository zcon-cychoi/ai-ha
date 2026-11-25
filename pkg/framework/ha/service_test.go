package ha

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/agent"
)

// memStore is a Store in memory. The transactional properties are tested
// against PostgreSQL; this one serves the rules the Service applies.
type memStore struct {
	groups    map[string]*GroupStatus
	failEval  map[string]bool
	evaluated []string
}

func newMemStore() *memStore {
	return &memStore{groups: map[string]*GroupStatus{}, failEval: map[string]bool{}}
}

func (m *memStore) Create(_ context.Context, g NewGroup) (GroupStatus, error) {
	for _, existing := range m.groups {
		if existing.Name == g.Spec.Name {
			return GroupStatus{}, ErrNameTaken
		}
	}
	gs := &GroupStatus{
		Group: Group{
			GroupID: g.GroupID, Name: g.Spec.Name, State: StateRegistered, Verdict: VerdictUnknown,
			Resource: g.Spec.Resource, Service: g.Spec.Service, ServiceAddress: g.Spec.ServiceAddress,
			EventSeq: 1, CreatedBy: g.CreatedBy,
		},
		Primary: Node{SystemID: g.Spec.Primary.SystemID, Role: RolePrimary, Address: g.Spec.Primary.Address,
			Disk: g.Spec.Primary.Disk, NodeID: 0},
		Standby: Node{SystemID: g.Spec.Standby.SystemID, Role: RoleStandby, Address: g.Spec.Standby.Address,
			Disk: g.Spec.Standby.Disk, NodeID: 1},
		Now: time.Now().UTC(),
	}
	m.groups[g.GroupID] = gs
	return *gs, nil
}

func (m *memStore) List(context.Context) ([]GroupStatus, error) {
	var out []GroupStatus
	for _, g := range m.groups {
		out = append(out, *g)
	}
	return out, nil
}

func (m *memStore) Get(_ context.Context, idOrName string) (GroupStatus, error) {
	for _, g := range m.groups {
		if g.GroupID == idOrName || g.Name == idOrName {
			return *g, nil
		}
	}
	return GroupStatus{}, ErrGroupNotFound
}

func (m *memStore) GroupIDs(context.Context) ([]string, error) {
	var ids []string
	for id := range m.groups {
		ids = append(ids, id)
	}
	return ids, nil
}

func (m *memStore) Events(context.Context, string, int64, int) ([]Event, error) { return nil, nil }

func (m *memStore) Report(_ context.Context, systemID, agentID string, r model.AgentHealthReq, decide Decide) (GroupStatus, bool, error) {
	for _, g := range m.groups {
		var node *Node
		switch systemID {
		case g.Primary.SystemID:
			node = &g.Primary
		case g.Standby.SystemID:
			node = &g.Standby
		default:
			continue
		}
		if r.GroupID != g.GroupID || r.Role != string(node.Role) {
			return *g, false, nil
		}
		now := time.Now().UTC()
		node.Report, node.ReceivedAt, node.AgentID = &r, &now, agentID
		g.Now = now
		m.apply(g, decide(*g))
		return *g, true, nil
	}
	return GroupStatus{}, false, ErrNotAssigned
}

func (m *memStore) Evaluate(_ context.Context, groupID string, decide Decide) (GroupStatus, error) {
	m.evaluated = append(m.evaluated, groupID)
	if m.failEval[groupID] {
		return GroupStatus{}, errors.New("injected failure")
	}
	g, ok := m.groups[groupID]
	if !ok {
		return GroupStatus{}, ErrGroupNotFound
	}
	m.apply(g, decide(*g))
	return *g, nil
}

func (m *memStore) apply(g *GroupStatus, ch Change) {
	g.State, g.Verdict, g.Reasons = ch.State, ch.Verdict, ch.Reasons
	g.LastSyncConfirmedAt, g.LinkDownSince, g.PrimaryFailingSince =
		ch.LastSyncConfirmedAt, ch.LinkDownSince, ch.PrimaryFailingSince
	g.EventSeq += int64(len(ch.Events))
}

// systemSet answers whether a system is enrolled.
type systemSet map[string]bool

func (s systemSet) GetSystem(_ context.Context, id string) (agent.System, error) {
	if !s[id] {
		return agent.System{}, agent.ErrSystemNotFound
	}
	return agent.System{SystemID: id}, nil
}

var testTimings = Timings{ReportInterval: 30 * time.Second, StaleAfter: 90 * time.Second}

func newTestService(t *testing.T) (*Service, *memStore) {
	t.Helper()
	store := newMemStore()
	n := 0
	newID := func() string {
		n++
		return fmt.Sprintf("%016x", 0xabc000+n)
	}
	return NewService(store, systemSet{"sys-a": true, "sys-b": true, "sys-c": true}, testTimings, newID), store
}

func validSpec() GroupSpec {
	return GroupSpec{
		Name:     "inference-ha",
		Primary:  NodeSpec{SystemID: "sys-a", Address: "10.0.1.10"},
		Standby:  NodeSpec{SystemID: "sys-b", Address: "10.0.2.10"},
		Resource: ResourceSpec{Name: "r0", Device: "/dev/drbd0", MountPoint: "/data", Port: 7789},
		Service:  ServiceSpec{Unit: "ai.service", Port: 8000, HealthPath: "/healthz", ExpectedGPUs: 1, MaxHealthLatencyMs: 1000},
	}
}

func TestCreateValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*GroupSpec)
	}{
		{"empty name", func(s *GroupSpec) { s.Name = "" }},
		{"uppercase name", func(s *GroupSpec) { s.Name = "Inference" }},
		{"name like an id", func(s *GroupSpec) { s.Name = "0123456789abcdef" }},
		{"same system twice", func(s *GroupSpec) { s.Standby.SystemID = "sys-a" }},
		{"missing standby", func(s *GroupSpec) { s.Standby.SystemID = "" }},
		{"host name for an address", func(s *GroupSpec) { s.Primary.Address = "ip-10-0-1-10" }},
		{"same address twice", func(s *GroupSpec) { s.Standby.Address = "10.0.1.10" }},
		{"loopback address", func(s *GroupSpec) { s.Standby.Address = "127.0.0.1" }},
		{"bad resource name", func(s *GroupSpec) { s.Resource.Name = "r 0" }},
		{"device outside /dev", func(s *GroupSpec) { s.Resource.Device = "/tmp/drbd0" }},
		{"mount point is root", func(s *GroupSpec) { s.Resource.MountPoint = "/" }},
		{"relative mount point", func(s *GroupSpec) { s.Resource.MountPoint = "data" }},
		{"replication port zero", func(s *GroupSpec) { s.Resource.Port = 0 }},
		{"unit without suffix", func(s *GroupSpec) { s.Service.Unit = "ai" }},
		{"unit with a path", func(s *GroupSpec) { s.Service.Unit = "../ai.service" }},
		{"unit like an option", func(s *GroupSpec) { s.Service.Unit = "-ai.service" }},
		{"service port shared with replication", func(s *GroupSpec) { s.Service.Port = 7789 }},
		{"relative health path", func(s *GroupSpec) { s.Service.HealthPath = "healthz" }},
		{"too many GPUs", func(s *GroupSpec) { s.Service.ExpectedGPUs = 65 }},
		{"latency limit at the timeout", func(s *GroupSpec) {
			s.Service.MaxHealthLatencyMs = int(model.HAHealthAPITimeout / time.Millisecond)
		}},
		{"latency limit with nothing to measure", func(s *GroupSpec) { s.Service.HealthPath = "" }},
		// A kernel name can name another disk after a reboot.
		{"kernel name for a disk", func(s *GroupSpec) {
			s.Resource.Name = ""
			s.Primary.Disk, s.Standby.Disk = "/dev/nvme1n1", "/dev/disk/by-id/nvme-vol0bbb"
		}},
		{"by-id path with a subdirectory", func(s *GroupSpec) {
			s.Resource.Name = ""
			s.Primary.Disk, s.Standby.Disk = "/dev/disk/by-id/a/../../sda", "/dev/disk/by-id/nvme-vol0bbb"
		}},
		{"one disk only", func(s *GroupSpec) { s.Resource.Name = ""; s.Primary.Disk = "/dev/disk/by-id/nvme-vol0aaa" }},
		// A named resource is adopted as it is; disks are for one set up.
		{"disks for an adopted resource", func(s *GroupSpec) {
			s.Primary.Disk, s.Standby.Disk = "/dev/disk/by-id/nvme-vol0aaa", "/dev/disk/by-id/nvme-vol0bbb"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newTestService(t)
			spec := validSpec()
			tc.mutate(&spec)
			if _, err := svc.Create(context.Background(), spec, "test"); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestCreate(t *testing.T) {
	ctx := context.Background()

	t.Run("data disks are kept per node", func(t *testing.T) {
		svc, _ := newTestService(t)
		spec := validSpec()
		spec.Resource.Name = ""
		spec.Primary.Disk, spec.Standby.Disk = " /dev/disk/by-id/nvme-vol0aaa", "/dev/disk/by-id/nvme-vol0bbb "
		gs, err := svc.Create(ctx, spec, "test")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if gs.Primary.Disk != "/dev/disk/by-id/nvme-vol0aaa" || gs.Standby.Disk != "/dev/disk/by-id/nvme-vol0bbb" {
			t.Errorf("disks = %q, %q", gs.Primary.Disk, gs.Standby.Disk)
		}
	})

	t.Run("an unknown system is refused", func(t *testing.T) {
		svc, _ := newTestService(t)
		spec := validSpec()
		spec.Standby.SystemID = "sys-typo"
		if _, err := svc.Create(ctx, spec, "test"); !errors.Is(err, ErrUnknownSystem) {
			t.Fatalf("err = %v, want ErrUnknownSystem", err)
		}
	})

	t.Run("a named resource is adopted", func(t *testing.T) {
		svc, _ := newTestService(t)
		gs, err := svc.Create(ctx, validSpec(), "test")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if gs.Resource.Name != "r0" || gs.State != StateRegistered {
			t.Errorf("group = %+v", gs.Group)
		}
	})

	t.Run("an unnamed resource is issued a name", func(t *testing.T) {
		svc, _ := newTestService(t)
		spec := validSpec()
		spec.Resource.Name = ""
		gs, err := svc.Create(ctx, spec, "test")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if !strings.HasPrefix(gs.Resource.Name, "aiha-") || !resourcePattern.MatchString(gs.Resource.Name) {
			t.Errorf("issued resource name = %q", gs.Resource.Name)
		}
	})
}

func TestReport(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	gs, err := svc.Create(ctx, validSpec(), "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Now().UTC()

	t.Run("a report needs a collection time", func(t *testing.T) {
		if _, err := svc.Report(ctx, "sys-a", "agent-a", model.AgentHealthReq{}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("err = %v, want ErrInvalidRequest", err)
		}
	})

	t.Run("half an assignment is refused", func(t *testing.T) {
		req := model.AgentHealthReq{CollectedAt: now, GroupID: gs.GroupID}
		if _, err := svc.Report(ctx, "sys-a", "agent-a", req); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("err = %v, want ErrInvalidRequest", err)
		}
	})

	t.Run("a system in no group is told so", func(t *testing.T) {
		resp, err := svc.Report(ctx, "sys-c", "agent-c", model.AgentHealthReq{CollectedAt: now})
		if err != nil {
			t.Fatalf("report: %v", err)
		}
		if resp.Assigned || resp.Assignment != nil || resp.ReportIntervalSeconds != 30 {
			t.Errorf("resp = %+v", resp)
		}
	})

	t.Run("the primary is told what to check, with no probe", func(t *testing.T) {
		resp, err := svc.Report(ctx, "sys-a", "agent-a", model.AgentHealthReq{CollectedAt: now})
		if err != nil {
			t.Fatalf("report: %v", err)
		}
		a := resp.Assignment
		if !resp.Assigned || a == nil || a.Role != "primary" || a.GroupID != gs.GroupID || a.Probe != nil {
			t.Fatalf("resp = %+v / %+v", resp, a)
		}
		if a.Service.Unit != "ai.service" || a.Resource.Name != "r0" || a.Resource.MountPoint != "/data" {
			t.Errorf("assignment = %+v", a)
		}
	})

	t.Run("the standby is told to probe the primary", func(t *testing.T) {
		resp, err := svc.Report(ctx, "sys-b", "agent-b", model.AgentHealthReq{CollectedAt: now})
		if err != nil {
			t.Fatalf("report: %v", err)
		}
		a := resp.Assignment
		if a == nil || a.Role != "standby" || a.Probe == nil {
			t.Fatalf("assignment = %+v", a)
		}
		if a.Probe.Address != "10.0.1.10" || a.Probe.Port != 8000 || a.Probe.HealthPath != "/healthz" {
			t.Errorf("probe = %+v", a.Probe)
		}
	})

	t.Run("a report under the assignment is judged", func(t *testing.T) {
		req := *healthyPrimary()
		req.CollectedAt, req.GroupID = now, gs.GroupID
		if _, err := svc.Report(ctx, "sys-a", "agent-a", req); err != nil {
			t.Fatalf("report: %v", err)
		}
		got, err := svc.Get(ctx, gs.Name)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		// The standby has not reported under its role, so the pair is not ok.
		if got.Verdict != VerdictAttention || got.Primary.Report == nil {
			t.Errorf("after report = %s %v", got.Verdict, got.Reasons)
		}
	})
}

func TestEvaluateAllContinuesPastAFailure(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestService(t)
	first, err := svc.Create(ctx, validSpec(), "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	other := validSpec()
	other.Name, other.Primary.SystemID, other.Standby.SystemID = "second", "sys-c", "sys-b"
	if _, err := svc.Create(ctx, other, "test"); err != nil {
		t.Fatalf("create second: %v", err)
	}
	store.failEval[first.GroupID] = true

	n, err := svc.EvaluateAll(ctx)
	if err == nil || n != 1 || len(store.evaluated) != 2 {
		t.Fatalf("evaluated %d (tried %v), err %v", n, store.evaluated, err)
	}
}

func TestEventsLimits(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	if _, err := svc.Create(ctx, validSpec(), "test"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := svc.Events(ctx, "inference-ha", -1, 10); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("negative after: err = %v", err)
	}
	if _, _, err := svc.Events(ctx, "inference-ha", 0, maxEventLimit+1); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("limit above max: err = %v", err)
	}
	if _, _, err := svc.Events(ctx, "nope", 0, 0); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("unknown group: err = %v", err)
	}
}
