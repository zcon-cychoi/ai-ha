package ha

import (
	"slices"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
)

// These tests are TM-AI-HA-02 tables 5-1 and 5-3, and the state rules of
// plan/ha-roadmap.md section 6, written as cases. Each starts from a pair of
// nodes that is healthy in every respect and breaks one thing.

const testStale = 90 * time.Second

var testNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func healthyReplication(role, peerRole string) *model.HAReplicationStatus {
	return &model.HAReplicationStatus{
		Role: role, PeerRole: peerRole, Connection: "Connected",
		Disk: "UpToDate", PeerDisk: "UpToDate", OutOfSyncBytes: new(int64(0)),
	}
}

func healthyPrimary() *model.AgentHealthReq {
	return &model.AgentHealthReq{
		CollectedAt: testNow.Add(-5 * time.Second), GroupID: "g", Role: "primary",
		Service:     &model.HAUnitCheck{LoadState: "loaded", ActiveState: "active", SubState: "running"},
		Port:        &model.HAPortCheck{Address: "127.0.0.1:8000", Open: true},
		HealthAPI:   &model.HAHTTPCheck{URL: "http://127.0.0.1:8000/healthz", StatusCode: 200, LatencyMs: 20},
		GPU:         &model.HAGPUCheck{Count: 1},
		Replication: healthyReplication("Primary", "Secondary"),
	}
}

func healthyStandby() *model.AgentHealthReq {
	return &model.AgentHealthReq{
		CollectedAt: testNow.Add(-5 * time.Second), GroupID: "g", Role: "standby",
		Service:       &model.HAUnitCheck{LoadState: "loaded", ActiveState: "inactive", SubState: "dead"},
		Port:          &model.HAPortCheck{Address: "127.0.0.1:8000"},
		PeerPort:      &model.HAPortCheck{Address: "10.0.1.10:8000", Open: true},
		PeerHealthAPI: &model.HAHTTPCheck{URL: "http://10.0.1.10:8000/healthz", StatusCode: 200, LatencyMs: 25},
		GPU:           &model.HAGPUCheck{Count: 1},
		Replication:   healthyReplication("Secondary", "Primary"),
	}
}

func healthyGroup(state State) GroupStatus {
	received := testNow.Add(-10 * time.Second)
	return GroupStatus{
		Group: Group{
			GroupID: "g", Name: "inference-ha", State: state, Verdict: VerdictOK,
			Resource: ResourceSpec{Name: "r0", Device: "/dev/drbd0", MountPoint: "/data", Port: 7789},
			Service: ServiceSpec{
				Unit: "ai.service", Port: 8000, HealthPath: "/healthz", ExpectedGPUs: 1, MaxHealthLatencyMs: 1000,
			},
		},
		Primary: Node{SystemID: "sys-a", Role: RolePrimary, Address: "10.0.1.10", Report: healthyPrimary(), ReceivedAt: &received},
		Standby: Node{SystemID: "sys-b", Role: RoleStandby, Address: "10.0.2.10", Report: healthyStandby(), ReceivedAt: &received},
		Now:     testNow,
	}
}

func codes(rs []Reason) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, string(r.Node)+"/"+r.Code)
	}
	slices.Sort(out)
	return out
}

func TestAssessVerdicts(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*GroupStatus)
		verdict Verdict
		codes   []string
	}{
		{name: "healthy pair", mutate: func(*GroupStatus) {}, verdict: VerdictOK},

		// Errors (table 5-3).
		{name: "primary silent", verdict: VerdictError, codes: []string{"primary/primary_report_lost"},
			mutate: func(g *GroupStatus) { g.Primary.ReceivedAt = new(testNow.Add(-2 * time.Minute)) }},
		{name: "primary never reported", verdict: VerdictError, codes: []string{"primary/primary_report_lost"},
			mutate: func(g *GroupStatus) { g.Primary.Report, g.Primary.ReceivedAt = nil, nil }},
		{name: "primary service stopped", verdict: VerdictError, codes: []string{"primary/primary_service_down"},
			mutate: func(g *GroupStatus) { g.Primary.Report.Service.ActiveState = "failed" }},
		{name: "primary service unreadable", verdict: VerdictError, codes: []string{"primary/primary_service_down"},
			mutate: func(g *GroupStatus) { g.Primary.Report.Service = &model.HAUnitCheck{Error: "systemctl failed"} }},
		{name: "primary port closed", verdict: VerdictError, codes: []string{"primary/primary_port_closed"},
			mutate: func(g *GroupStatus) { g.Primary.Report.Port.Open = false }},
		{name: "primary health API 503", verdict: VerdictError, codes: []string{"primary/primary_health_failed"},
			mutate: func(g *GroupStatus) { g.Primary.Report.HealthAPI.StatusCode = 503 }},
		{name: "primary health API no answer", verdict: VerdictError, codes: []string{"primary/primary_health_failed"},
			mutate: func(g *GroupStatus) {
				g.Primary.Report.HealthAPI = &model.HAHTTPCheck{URL: "x", Error: "timeout", LatencyMs: 5000}
			}},
		{name: "link lasting down", verdict: VerdictError, codes: []string{"/link_down_persistent"},
			mutate: func(g *GroupStatus) {
				g.Primary.Report.Replication.Connection = "StandAlone"
				g.Standby.Report.Replication.Connection = "StandAlone"
				g.LinkDownSince = new(testNow.Add(-2 * time.Minute))
			}},
		{name: "standby disk outdated", verdict: VerdictError,
			codes:  []string{"standby/disk_bad"},
			mutate: func(g *GroupStatus) { g.Standby.Report.Replication.Disk = "Outdated" }},
		{name: "both primary", verdict: VerdictError, codes: []string{"/both_primary"},
			mutate: func(g *GroupStatus) {
				g.Standby.Report.Replication.Role = "Primary"
				g.Primary.Report.Replication.PeerRole = "Primary"
			}},
		{name: "standby sees both primary while primary is silent", verdict: VerdictError,
			codes: []string{"/both_primary", "primary/primary_report_lost"},
			mutate: func(g *GroupStatus) {
				g.Primary.ReceivedAt = new(testNow.Add(-5 * time.Minute))
				g.Standby.Report.Replication.Role = "Primary"
				g.Standby.Report.Replication.PeerRole = "Primary"
			}},
		{name: "primary replica is secondary", verdict: VerdictError, codes: []string{"primary/role_mismatch"},
			mutate: func(g *GroupStatus) { g.Primary.Report.Replication.Role = "Secondary" }},
		{name: "replication not reported", verdict: VerdictError, codes: []string{"standby/replication_unreported"},
			mutate: func(g *GroupStatus) { g.Standby.Report.Replication = nil }},
		{name: "replication unreadable", verdict: VerdictError, codes: []string{"primary/replication_unreadable"},
			mutate: func(g *GroupStatus) {
				g.Primary.Report.Replication = &model.HAReplicationStatus{Error: "resource not configured"}
			}},

		// Attention (table 5-3).
		{name: "resync in progress", verdict: VerdictAttention, codes: []string{"/resyncing"},
			mutate: func(g *GroupStatus) {
				g.Primary.Report.Replication.Connection = "SyncSource"
				g.Primary.Report.Replication.PeerDisk = "Inconsistent"
				g.Primary.Report.Replication.OutOfSyncBytes = new(int64(1 << 30))
				g.Standby.Report.Replication.Connection = "SyncTarget"
				g.Standby.Report.Replication.Disk = "Inconsistent"
				g.Standby.Report.Replication.OutOfSyncBytes = new(int64(1 << 30))
			}},
		{name: "resync in progress, DRBD 9 words", verdict: VerdictAttention, codes: []string{"/resyncing"},
			mutate: func(g *GroupStatus) {
				g.Primary.Report.Replication.Sync = "SyncSource"
				g.Standby.Report.Replication.Sync = "SyncTarget"
				g.Standby.Report.Replication.Disk = "Inconsistent"
			}},
		{name: "standby report late", verdict: VerdictAttention, codes: []string{"standby/standby_report_late"},
			mutate: func(g *GroupStatus) { g.Standby.ReceivedAt = new(testNow.Add(-3 * time.Minute)) }},
		{name: "health API slow", verdict: VerdictAttention, codes: []string{"primary/primary_health_slow"},
			mutate: func(g *GroupStatus) { g.Primary.Report.HealthAPI.LatencyMs = 1500 }},
		{name: "standby GPU missing", verdict: VerdictAttention, codes: []string{"standby/gpu_missing"},
			mutate: func(g *GroupStatus) { g.Standby.Report.GPU.Count = 0 }},
		{name: "standby GPU unreadable", verdict: VerdictAttention, codes: []string{"standby/gpu_missing"},
			mutate: func(g *GroupStatus) { g.Standby.Report.GPU = &model.HAGPUCheck{Error: "nvidia-smi not found"} }},
		{name: "standby service running", verdict: VerdictAttention, codes: []string{"standby/standby_service_running"},
			mutate: func(g *GroupStatus) { g.Standby.Report.Service.ActiveState = "active" }},
		{name: "link briefly down", verdict: VerdictAttention, codes: []string{"/link_down"},
			mutate: func(g *GroupStatus) { g.Standby.Report.Replication.Connection = "Connecting" }},
		{name: "standby cannot reach primary", verdict: VerdictAttention, codes: []string{"standby/probe_port_failed"},
			mutate: func(g *GroupStatus) { g.Standby.Report.PeerPort.Open = false }},
		{name: "out of sync while connected", verdict: VerdictAttention, codes: []string{"/out_of_sync"},
			mutate: func(g *GroupStatus) { g.Primary.Report.Replication.OutOfSyncBytes = new(int64(4096)) }},
		{name: "out of sync not measured", verdict: VerdictAttention, codes: []string{"/sync_unconfirmed"},
			mutate: func(g *GroupStatus) { g.Standby.Report.Replication.OutOfSyncBytes = nil }},

		// What the service definition switches off.
		{name: "no GPUs expected", verdict: VerdictOK,
			mutate: func(g *GroupStatus) {
				g.Service.ExpectedGPUs = 0
				g.Primary.Report.GPU, g.Standby.Report.GPU = nil, nil
			}},
		{name: "no health path", verdict: VerdictOK,
			mutate: func(g *GroupStatus) {
				g.Service.HealthPath, g.Service.MaxHealthLatencyMs = "", 0
				g.Primary.Report.HealthAPI, g.Standby.Report.PeerHealthAPI = nil, nil
			}},
		{name: "a stopped primary is not repeated as an unreachable one", verdict: VerdictError,
			codes: []string{"primary/primary_health_failed", "primary/primary_port_closed", "primary/primary_service_down"},
			mutate: func(g *GroupStatus) {
				g.Primary.Report.Service.ActiveState = "inactive"
				g.Primary.Report.Port.Open = false
				g.Primary.Report.HealthAPI = &model.HAHTTPCheck{Error: "refused"}
				g.Standby.Report.PeerPort.Open = false
				g.Standby.Report.PeerHealthAPI = &model.HAHTTPCheck{Error: "refused"}
			}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := healthyGroup(StateProtected)
			tc.mutate(&g)
			ch := Assess(g, testStale)
			if ch.Verdict != tc.verdict {
				t.Errorf("verdict = %s, want %s (reasons %v)", ch.Verdict, tc.verdict, ch.Reasons)
			}
			want := slices.Clone(tc.codes)
			slices.Sort(want)
			if got := codes(ch.Reasons); !slices.Equal(got, want) {
				t.Errorf("reasons = %v, want %v", got, want)
			}
		})
	}
}

func TestAssessStates(t *testing.T) {
	failingSince := testNow.Add(-2 * time.Minute)
	cases := []struct {
		name   string
		from   State
		mutate func(*GroupStatus)
		want   State
	}{
		{name: "syncing finishes", from: StateSyncing, mutate: func(*GroupStatus) {}, want: StateProtected},
		{name: "syncing waits for the resync", from: StateSyncing, want: StateSyncing,
			mutate: func(g *GroupStatus) {
				g.Primary.Report.Replication.Connection = "SyncSource"
				g.Standby.Report.Replication.Connection = "SyncTarget"
				g.Standby.Report.Replication.Disk = "Inconsistent"
			}},
		{name: "syncing does not fail over", from: StateSyncing, want: StateSyncing,
			mutate: func(g *GroupStatus) { g.Primary.ReceivedAt = new(testNow.Add(-10 * time.Minute)) }},
		{name: "protected stays protected", from: StateProtected, mutate: func(*GroupStatus) {}, want: StateProtected},
		{name: "a slow primary is not degraded protection", from: StateProtected, want: StateProtected,
			mutate: func(g *GroupStatus) { g.Primary.Report.HealthAPI.LatencyMs = 4000 }},
		{name: "protected degrades when the link drops", from: StateProtected, want: StateDegraded,
			mutate: func(g *GroupStatus) { g.Standby.Report.Replication.Connection = "Connecting" }},
		{name: "protected degrades when the standby goes quiet", from: StateProtected, want: StateDegraded,
			mutate: func(g *GroupStatus) { g.Standby.ReceivedAt = new(testNow.Add(-3 * time.Minute)) }},
		{name: "a fenced node keeps the group degraded", from: StateDegraded, want: StateDegraded,
			mutate: func(g *GroupStatus) { g.Standby.Fenced = true }},
		{name: "degraded recovers", from: StateDegraded, mutate: func(*GroupStatus) {}, want: StateProtected},
		{name: "a fresh primary failure waits", from: StateProtected, want: StateProtected,
			mutate: func(g *GroupStatus) { g.Primary.Report.Service.ActiveState = "failed" }},
		{name: "a lasting primary failure asks for a failover", from: StateProtected, want: StateFailoverPending,
			mutate: func(g *GroupStatus) {
				g.Primary.Report.Service.ActiveState = "failed"
				g.PrimaryFailingSince = &failingSince
			}},
		{name: "a silent primary asks for a failover", from: StateDegraded, want: StateFailoverPending,
			mutate: func(g *GroupStatus) { g.Primary.ReceivedAt = new(testNow.Add(-2 * time.Minute)) }},
		{name: "failover pending holds while the primary fails", from: StateFailoverPending, want: StateFailoverPending,
			mutate: func(g *GroupStatus) { g.Primary.ReceivedAt = new(testNow.Add(-1 * time.Hour)) }},
		{name: "a recovered primary withdraws the failover", from: StateFailoverPending, mutate: func(*GroupStatus) {},
			want: StateProtected},
		{name: "a recovered primary with a broken link is degraded", from: StateFailoverPending, want: StateDegraded,
			mutate: func(g *GroupStatus) { g.Standby.Report.Replication.Connection = "Connecting" }},
		{name: "both primary is an error", from: StateProtected, want: StateError,
			mutate: func(g *GroupStatus) { g.Standby.Report.Replication.Role = "Primary" }},
		{name: "registered is not the evaluator's", from: StateRegistered, want: StateRegistered,
			mutate: func(g *GroupStatus) { g.Standby.Report.Replication.Role = "Primary" }},
		{name: "error is not the evaluator's", from: StateError, mutate: func(*GroupStatus) {}, want: StateError},
		{name: "switching is not the evaluator's", from: StateSwitching, want: StateSwitching,
			mutate: func(g *GroupStatus) { g.Primary.ReceivedAt = nil }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := healthyGroup(tc.from)
			tc.mutate(&g)
			ch := Assess(g, testStale)
			if ch.State != tc.want {
				t.Errorf("state = %s, want %s (reasons %v)", ch.State, tc.want, ch.Reasons)
			}
			moved := slices.ContainsFunc(ch.Events, func(e NewEvent) bool { return e.Kind == EventStateChanged })
			if moved != (tc.want != tc.from) {
				t.Errorf("state_changed event = %v, want %v", moved, tc.want != tc.from)
			}
		})
	}
}

func TestAssessTimers(t *testing.T) {
	t.Run("a link that drops is timed from now", func(t *testing.T) {
		g := healthyGroup(StateProtected)
		g.Standby.Report.Replication.Connection = "Connecting"
		ch := Assess(g, testStale)
		if ch.LinkDownSince == nil || !ch.LinkDownSince.Equal(testNow) {
			t.Fatalf("link down since = %v, want %v", ch.LinkDownSince, testNow)
		}
	})
	t.Run("a link that stays down keeps its start", func(t *testing.T) {
		g := healthyGroup(StateProtected)
		since := testNow.Add(-30 * time.Second)
		g.LinkDownSince = &since
		g.Standby.Report.Replication.Connection = "Connecting"
		if ch := Assess(g, testStale); ch.LinkDownSince == nil || !ch.LinkDownSince.Equal(since) {
			t.Fatalf("link down since = %v, want %v", ch.LinkDownSince, since)
		}
	})
	t.Run("a link seen up is cleared", func(t *testing.T) {
		g := healthyGroup(StateProtected)
		since := testNow.Add(-30 * time.Second)
		g.LinkDownSince = &since
		if ch := Assess(g, testStale); ch.LinkDownSince != nil {
			t.Fatalf("link down since = %v, want cleared", ch.LinkDownSince)
		}
	})
	t.Run("a link nobody reported on is left as it was", func(t *testing.T) {
		g := healthyGroup(StateProtected)
		since := testNow.Add(-30 * time.Second)
		g.LinkDownSince = &since
		g.Primary.ReceivedAt = new(testNow.Add(-time.Hour))
		g.Standby.ReceivedAt = new(testNow.Add(-time.Hour))
		if ch := Assess(g, testStale); ch.LinkDownSince == nil || !ch.LinkDownSince.Equal(since) {
			t.Fatalf("link down since = %v, want %v", ch.LinkDownSince, since)
		}
	})
	t.Run("a silent primary is failing since its last report", func(t *testing.T) {
		g := healthyGroup(StateProtected)
		last := testNow.Add(-2 * time.Minute)
		g.Primary.ReceivedAt = &last
		if ch := Assess(g, testStale); ch.PrimaryFailingSince == nil || !ch.PrimaryFailingSince.Equal(last) {
			t.Fatalf("primary failing since = %v, want %v", ch.PrimaryFailingSince, last)
		}
	})
	t.Run("sync is confirmed only when it is", func(t *testing.T) {
		g := healthyGroup(StateProtected)
		if ch := Assess(g, testStale); ch.LastSyncConfirmedAt == nil || !ch.LastSyncConfirmedAt.Equal(testNow) {
			t.Fatalf("last sync confirmed = %v, want %v", ch.LastSyncConfirmedAt, testNow)
		}
		before := testNow.Add(-time.Hour)
		g.LastSyncConfirmedAt = &before
		g.Standby.Report.Replication.Connection = "StandAlone"
		if ch := Assess(g, testStale); ch.LastSyncConfirmedAt == nil || !ch.LastSyncConfirmedAt.Equal(before) {
			t.Fatalf("last sync confirmed = %v, want kept at %v", ch.LastSyncConfirmedAt, before)
		}
	})
}

func TestAssessEvents(t *testing.T) {
	t.Run("nothing changed, nothing recorded", func(t *testing.T) {
		g := healthyGroup(StateProtected)
		if ch := Assess(g, testStale); len(ch.Events) != 0 {
			t.Fatalf("events = %+v, want none", ch.Events)
		}
	})
	t.Run("the same condition with new figures is not a change", func(t *testing.T) {
		g := healthyGroup(StateProtected)
		g.Primary.Report.HealthAPI.LatencyMs = 1500
		first := Assess(g, testStale)
		g.Verdict, g.Reasons = first.Verdict, first.Reasons
		g.Primary.Report.HealthAPI.LatencyMs = 2500
		if ch := Assess(g, testStale); len(ch.Events) != 0 {
			t.Fatalf("events = %+v, want none", ch.Events)
		}
	})
	t.Run("a new condition at the same verdict is recorded", func(t *testing.T) {
		g := healthyGroup(StateProtected)
		g.Primary.Report.HealthAPI.LatencyMs = 1500
		first := Assess(g, testStale)
		g.Verdict, g.Reasons = first.Verdict, first.Reasons
		g.Standby.Report.GPU.Count = 0
		ch := Assess(g, testStale)
		if len(ch.Events) == 0 || ch.Events[0].Kind != EventVerdictChanged {
			t.Fatalf("events = %+v, want a verdict change", ch.Events)
		}
	})
	t.Run("verdict and state changes are both recorded, verdict first", func(t *testing.T) {
		g := healthyGroup(StateProtected)
		g.Standby.Report.Replication.Connection = "Connecting"
		ch := Assess(g, testStale)
		if len(ch.Events) != 2 || ch.Events[0].Kind != EventVerdictChanged || ch.Events[1].Kind != EventStateChanged {
			t.Fatalf("events = %+v", ch.Events)
		}
		if ch.Events[1].From != string(StateProtected) || ch.Events[1].To != string(StateDegraded) {
			t.Errorf("state event = %+v", ch.Events[1])
		}
	})
}
