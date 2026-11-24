package ha

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
)

// The rules of DecideFlow, without a database: when a step waits, fails or
// moves on.

func decideGroup(now time.Time) GroupStatus {
	return GroupStatus{
		Group: Group{GroupID: "g1", State: StateConfiguring,
			Resource: ResourceSpec{Name: "aiha-g1", Device: "/dev/drbd0", MountPoint: "/data", Port: 7789},
			Service:  ServiceSpec{Unit: "ai.service", Port: 8000}},
		Primary: Node{SystemID: "p", Role: RolePrimary, Address: "10.0.1.10", NodeID: 0, Disk: "/dev/disk/by-id/p"},
		Standby: Node{SystemID: "s", Role: RoleStandby, Address: "10.0.2.10", NodeID: 1, Disk: "/dev/disk/by-id/s"},
		Now:     now,
	}
}

func withReport(n Node, at time.Time, r *model.HAReplicationStatus) Node {
	n.Report = &model.AgentHealthReq{Hostname: "host-" + n.SystemID, Replication: r}
	n.ReceivedAt = &at
	return n
}

func stepJob(step int, system string, status job.Status, finished *time.Time, spec any, result any) StepJob {
	sj := StepJob{FlowJob: FlowJob{Step: step, SystemID: system, JobID: "j" + system}}
	sj.Job = job.Job{JobID: sj.JobID, SystemID: system, Status: status, FinishedAt: finished, Message: "boom"}
	if spec != nil {
		sj.Job.Spec, _ = json.Marshal(spec)
	}
	if result != nil {
		b, _ := json.Marshal(result)
		sj.Attempt = &job.Attempt{Result: b}
	}
	return sj
}

func TestDecideFlow(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-30 * time.Second)
	long := now.Add(-5 * time.Minute)
	stale := 90 * time.Second
	secondary := &model.HAReplicationStatus{Role: "Secondary", Connection: "Connecting", Disk: "Inconsistent"}
	primary := &model.HAReplicationStatus{Role: "Primary", Connection: "Connected", Disk: "UpToDate"}
	gone := &model.HAReplicationStatus{Error: "drbdsetup: No such resource"}

	standbySpec := model.ReplicationSetupSpec{FlowID: "f", Step: 1, Resource: "aiha-g1",
		Self: model.ReplicationNode{Hostname: "host-s", NodeID: 1, Disk: "/dev/disk/by-id/s"},
		Peer: model.ReplicationNode{Hostname: "host-p", NodeID: 0, Disk: "/dev/disk/by-id/p"}}

	cases := []struct {
		name     string
		kind     FlowKind
		step     int
		prepare  func(*GroupStatus)
		sj       StepJob
		want     MoveKind
		wantText string
	}{
		{name: "job still queued", kind: FlowSetup, step: 1,
			sj: stepJob(1, "s", job.StatusQueued, nil, standbySpec, nil), want: MoveWait},
		{name: "job running", kind: FlowSetup, step: 1,
			sj: stepJob(1, "s", job.StatusRunning, nil, standbySpec, nil), want: MoveWait},
		{name: "job failed", kind: FlowSetup, step: 1,
			sj: stepJob(1, "s", job.StatusFailed, &finished, standbySpec, nil), want: MoveFail, wantText: "failed: boom"},
		{name: "job cancelled", kind: FlowTeardown, step: 1,
			sj: stepJob(1, "p", job.StatusCancelled, &finished, nil, nil), want: MoveFail},
		{name: "completed, no report since", kind: FlowSetup, step: 1,
			prepare: func(gs *GroupStatus) { gs.Standby = withReport(gs.Standby, finished.Add(-time.Second), secondary) },
			sj:      stepJob(1, "s", job.StatusCompleted, &finished, standbySpec, nil), want: MoveWait},
		{name: "completed, the node disagrees, still in time", kind: FlowSetup, step: 1,
			prepare: func(gs *GroupStatus) { gs.Standby = withReport(gs.Standby, now, gone) },
			sj:      stepJob(1, "s", job.StatusCompleted, &finished, standbySpec, nil), want: MoveWait},
		{name: "completed, the node never bore it out", kind: FlowSetup, step: 1,
			prepare: func(gs *GroupStatus) { gs.Standby = withReport(gs.Standby, now, gone) },
			sj:      stepJob(1, "s", job.StatusCompleted, &long, standbySpec, nil), want: MoveFail, wantText: "do not show"},
		{name: "standby borne out", kind: FlowSetup, step: 1,
			prepare: func(gs *GroupStatus) { gs.Standby = withReport(gs.Standby, now, secondary) },
			sj: stepJob(1, "s", job.StatusCompleted, &finished, standbySpec,
				model.ReplicationJobResult{DiskBytes: 8 << 30}), want: MoveNext},
		{name: "standby claiming to be primary", kind: FlowSetup, step: 1,
			prepare: func(gs *GroupStatus) { gs.Standby = withReport(gs.Standby, now, primary) },
			sj:      stepJob(1, "s", job.StatusCompleted, &long, standbySpec, nil), want: MoveFail},
		{name: "primary borne out", kind: FlowSetup, step: 2,
			prepare: func(gs *GroupStatus) { gs.Primary = withReport(gs.Primary, now, primary) },
			sj:      stepJob(2, "p", job.StatusCompleted, &finished, nil, nil), want: MoveComplete},
		{name: "primary down", kind: FlowTeardown, step: 1,
			prepare: func(gs *GroupStatus) { gs.Primary = withReport(gs.Primary, now, gone) },
			sj:      stepJob(1, "p", job.StatusCompleted, &finished, nil, nil), want: MoveNext},
		{name: "primary still up after teardown", kind: FlowTeardown, step: 1,
			prepare: func(gs *GroupStatus) { gs.Primary = withReport(gs.Primary, now, primary) },
			sj:      stepJob(1, "p", job.StatusCompleted, &long, nil, nil), want: MoveFail, wantText: "the resource gone"},
		{name: "standby down", kind: FlowTeardown, step: 2,
			prepare: func(gs *GroupStatus) { gs.Standby = withReport(gs.Standby, now, nil) },
			sj:      stepJob(2, "s", job.StatusCompleted, &finished, nil, nil), want: MoveComplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gs := decideGroup(now)
			if tc.prepare != nil {
				tc.prepare(&gs)
			}
			c := AdvanceContext{Group: gs, Flow: Flow{FlowID: "f", Kind: tc.kind, Step: tc.step}, Step: []StepJob{tc.sj}}
			mv, err := DecideFlow(c, stale, func() string { return "new-job" })
			if err != nil {
				t.Fatalf("DecideFlow: %v", err)
			}
			if mv.Kind != tc.want || !strings.Contains(mv.Message, tc.wantText) {
				t.Fatalf("move = %+v, want kind %d mentioning %q", mv, tc.want, tc.wantText)
			}

			switch {
			case mv.Kind == MoveComplete && tc.kind == FlowSetup && mv.To != StateSyncing,
				mv.Kind == MoveComplete && tc.kind == FlowTeardown && mv.To != StateRegistered:
				t.Errorf("completes to %s", mv.To)
			case mv.Kind == MoveNext && tc.kind == FlowSetup:
				// The primary's job is the standby's turned around, with the
				// standby's disk size to check against.
				var spec model.ReplicationSetupSpec
				if err := json.Unmarshal(mv.Jobs[0].Spec, &spec); err != nil {
					t.Fatal(err)
				}
				if mv.Jobs[0].SystemID != "p" || mv.Jobs[0].Kind != job.KindReplicationSetup || spec.Step != 2 ||
					!spec.InitialPrimary || spec.Self.Hostname != "host-p" || spec.Peer.Hostname != "host-s" ||
					spec.PeerDiskBytes != 8<<30 {
					t.Errorf("next job = %+v / %+v", mv.Jobs[0], spec)
				}
			case mv.Kind == MoveNext && tc.kind == FlowTeardown:
				if mv.Jobs[0].SystemID != "s" || mv.Jobs[0].Kind != job.KindReplicationTeardown {
					t.Errorf("next job = %+v", mv.Jobs[0])
				}
			}
		})
	}
}
