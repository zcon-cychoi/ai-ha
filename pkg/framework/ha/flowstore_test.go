package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/cloud-barista/ai-ha/pkg/framework/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests run the flows the way the server does: the job queue claims,
// starts and ends each step's job through the flows' own checks, the nodes
// report, and AdvanceAll moves the flow on.

const flowStale = 90 * time.Second

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("AIHA_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("AIHA_TEST_DB_DSN not set")
	}
	return dsn
}

// noRepos stands in for the repository registry, which HA jobs never use.
type noRepos struct{}

func (noRepos) Get(context.Context, string) (model.RepositoryInfo, error) {
	return model.RepositoryInfo{}, errors.New("no repositories in these tests")
}

func (noRepos) Access(context.Context, string) (model.RepositoryAccess, error) {
	return model.RepositoryAccess{}, errors.New("no repositories in these tests")
}

type flowEnv struct {
	t     *testing.T
	ctx   context.Context
	store *PGStore
	flows *FlowService
	jobs  *job.Service
	gs    GroupStatus
}

func newFlowEnv(t *testing.T) *flowEnv {
	t.Helper()
	store := testPGStore(t)
	ctx := context.Background()
	if err := job.Migrate(ctx, store.pool); err != nil {
		t.Fatalf("migrate jobs: %v", err)
	}
	key := make([]byte, secret.KeySize)
	copy(key, randomHex(t, secret.KeySize/2))
	box, err := secret.NewBox(key)
	if err != nil {
		t.Fatal(err)
	}
	e := &flowEnv{t: t, ctx: ctx, store: store}
	e.flows = NewFlowService(store, box, Timings{ReportInterval: 30 * time.Second, StaleAfter: flowStale},
		5*time.Minute, func() string { return randomHex(t, 8) })
	e.jobs = job.NewService(job.NewPGStore(store.pool), noRepos{}, e.flows,
		job.TTLs{Backup: time.Hour, HA: 5 * time.Minute}, 5*time.Minute,
		job.Timings{ProgressInterval: 10 * time.Second, LeaseTTL: time.Minute})
	e.gs = newSetupGroup(t, store)
	return e
}

// newSetupGroup creates a group that can be set up: it has its data disks.
func newSetupGroup(t *testing.T, store *PGStore) GroupStatus {
	t.Helper()
	id := randomHex(t, 8)
	spec := GroupSpec{
		Name:     "flow-" + id,
		Primary:  NodeSpec{SystemID: "sys-p-" + id, Address: "10.0.1.10", Disk: "/dev/disk/by-id/nvme-vol-p-" + id},
		Standby:  NodeSpec{SystemID: "sys-s-" + id, Address: "10.0.2.10", Disk: "/dev/disk/by-id/nvme-vol-s-" + id},
		Resource: ResourceSpec{Name: "aiha-" + id[:8], Device: "/dev/drbd0", MountPoint: "/data", Port: 7789},
		Service:  ServiceSpec{Unit: "ai.service", Port: 8000, HealthPath: "/healthz"},
	}
	gs, err := store.Create(context.Background(), NewGroup{GroupID: id, Spec: spec, CreatedBy: "test"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = store.pool.Exec(ctx, `DELETE FROM jobs WHERE system_id = ANY($1)`,
			[]string{gs.Primary.SystemID, gs.Standby.SystemID})
		_, _ = store.pool.Exec(ctx, `DELETE FROM ha_groups WHERE group_id = $1`, id)
	})
	return gs
}

func (e *flowEnv) node(role Role) Node {
	if role == RolePrimary {
		return e.gs.Primary
	}
	return e.gs.Standby
}

// report has a node report under its role.
func (e *flowEnv) report(role Role, r *model.HAReplicationStatus) {
	e.t.Helper()
	n := e.node(role)
	req := model.AgentHealthReq{CollectedAt: time.Now().UTC(), GroupID: e.gs.GroupID, Role: string(role),
		Hostname: "host-" + string(role), Replication: r}
	if _, recorded, err := e.store.Report(e.ctx, n.SystemID, "agent-"+string(role), req,
		func(g GroupStatus) Change { return Assess(g, flowStale) }); err != nil || !recorded {
		e.t.Fatalf("report %s: recorded=%v err=%v", role, recorded, err)
	}
}

var (
	resourceGone = &model.HAReplicationStatus{Error: "drbdsetup: aiha: No such resource"}
	upSecondary  = &model.HAReplicationStatus{Role: "Secondary", Connection: "Connecting", Disk: "Inconsistent"}
	upPrimary    = &model.HAReplicationStatus{Role: "Primary", PeerRole: "Secondary", Connection: "Connected",
		Disk: "UpToDate", PeerDisk: "Inconsistent"}
)

// runJob carries a job through the queue as an agent would: claim, start,
// result. It returns what the claim handed over.
func (e *flowEnv) runJob(role Role, jobID string, outcome job.Outcome, detail any) job.Claimed {
	e.t.Helper()
	who := job.Identity{AgentID: "agent-" + string(role), SystemID: e.node(role).SystemID}
	claimed, err := e.jobs.Claim(e.ctx, who, jobID, randomHex(e.t, 16))
	if err != nil {
		e.t.Fatalf("claim %s: %v", jobID, err)
	}
	if _, err := e.jobs.RecordProgress(e.ctx, who, jobID, claimed.Attempt.AttemptID, claimed.LeaseToken,
		job.Progress{Seq: 1, Phase: "start", Advanced: true}); err != nil {
		e.t.Fatalf("start %s: %v", jobID, err)
	}
	b, _ := json.Marshal(detail)
	if _, err := e.jobs.RecordResult(e.ctx, who, jobID, claimed.Attempt.AttemptID, claimed.LeaseToken,
		job.Result{Outcome: outcome, Message: "agent says " + string(outcome), Detail: b}); err != nil {
		e.t.Fatalf("result %s: %v", jobID, err)
	}
	return claimed
}

func (e *flowEnv) advance() {
	e.t.Helper()
	if _, err := e.flows.AdvanceAll(e.ctx); err != nil {
		e.t.Fatalf("advance: %v", err)
	}
}

func (e *flowEnv) latest() Flow {
	e.t.Helper()
	f, err := e.flows.LatestFlow(e.ctx, e.gs.GroupID)
	if err != nil || f == nil {
		e.t.Fatalf("latest flow: %v (%v)", f, err)
	}
	return *f
}

func (e *flowEnv) state() State {
	e.t.Helper()
	gs, err := e.store.Get(e.ctx, e.gs.GroupID)
	if err != nil {
		e.t.Fatal(err)
	}
	return gs.State
}

func (e *flowEnv) sealed() []byte {
	e.t.Helper()
	var b []byte
	if err := e.store.pool.QueryRow(e.ctx, `SELECT replication_secret FROM ha_groups WHERE group_id = $1`,
		e.gs.GroupID).Scan(&b); err != nil {
		e.t.Fatal(err)
	}
	return b
}

// The completion condition of H3b, short of the real nodes: setup reaches
// syncing with the standby first, each step issued once and only after the
// node bore the previous one out.
func TestSetupFlowRunsTheStandbyThenThePrimary(t *testing.T) {
	e := newFlowEnv(t)
	e.report(RolePrimary, resourceGone)
	e.report(RoleStandby, resourceGone)

	gs, flow, err := e.flows.Setup(e.ctx, e.gs.Name, "test")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if gs.State != StateConfiguring || flow.Step != 1 || len(flow.Jobs) != 1 || flow.Jobs[0].SystemID != e.gs.Standby.SystemID {
		t.Fatalf("after setup: state %s, flow %+v", gs.State, flow)
	}
	if _, _, err := e.flows.Setup(e.ctx, e.gs.Name, "test"); !errors.Is(err, ErrFlowInProgress) {
		t.Fatalf("second setup: err = %v, want ErrFlowInProgress", err)
	}
	sealed := e.sealed()
	if len(sealed) == 0 {
		t.Fatal("no shared secret stored")
	}
	if in, err := e.flows.InProgress(e.ctx, e.gs.Primary.SystemID); err != nil || !in {
		t.Fatalf("in progress = %v, %v", in, err)
	}

	// The standby's claim carries the secret, and the job's own spec does not.
	standbyJob := flow.Jobs[0].JobID
	claimed := e.runJob(RoleStandby, standbyJob, job.OutcomeCompleted, model.ReplicationJobResult{DiskBytes: 8 << 30})
	if claimed.Replication == nil || len(claimed.Replication.SharedSecret) != 2*secretBytes {
		t.Fatalf("claim replication = %+v", claimed.Replication)
	}
	if bytes.Contains(claimed.Job.Spec, []byte(claimed.Replication.SharedSecret)) {
		t.Fatal("the job's spec holds the secret")
	}
	var spec model.ReplicationSetupSpec
	if err := json.Unmarshal(claimed.Job.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.InitialPrimary || spec.Self.Hostname != "host-standby" || spec.Self.NodeID != 1 ||
		spec.Self.Disk != e.gs.Standby.Disk || spec.Peer.Disk != e.gs.Primary.Disk || spec.FlowID != flow.FlowID {
		t.Fatalf("standby spec = %+v", spec)
	}

	// Completed, but the standby has not reported since: the flow waits.
	e.advance()
	if f := e.latest(); f.Step != 1 {
		t.Fatalf("moved on before the standby bore its setup out: %+v", f)
	}
	e.report(RoleStandby, upSecondary)
	// The framework restarts here: a new FlowService over the same database
	// picks the flow up where it was, and a second tick must not issue the
	// step again.
	e.flows = NewFlowService(e.store, e.flows.box, e.flows.timings, e.flows.jobTTL, e.flows.newID)
	e.advance()
	e.advance()
	f := e.latest()
	if f.Step != 2 || len(f.Jobs) != 2 || f.Jobs[1].SystemID != e.gs.Primary.SystemID {
		t.Fatalf("after the standby: %+v", f)
	}
	if err := e.flows.Current(e.ctx, job.Job{JobID: standbyJob, Kind: job.KindReplicationSetup}); !errors.Is(err, job.ErrFlowMoved) {
		t.Fatalf("the standby's job is still current: %v", err)
	}

	claimed = e.runJob(RolePrimary, f.Jobs[1].JobID, job.OutcomeCompleted, model.ReplicationJobResult{DiskBytes: 8 << 30})
	if err := json.Unmarshal(claimed.Job.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if !spec.InitialPrimary || spec.Self.NodeID != 0 || spec.Self.Disk != e.gs.Primary.Disk || spec.PeerDiskBytes != 8<<30 {
		t.Fatalf("primary spec = %+v", spec)
	}
	e.report(RolePrimary, upPrimary)
	e.advance()

	if f := e.latest(); f.Status != FlowCompleted {
		t.Fatalf("flow = %+v", f)
	}
	if s := e.state(); s != StateSyncing {
		t.Fatalf("state = %s, want syncing", s)
	}
	if in, _ := e.flows.InProgress(e.ctx, e.gs.Standby.SystemID); in {
		t.Error("still in progress after completing")
	}

	// The events tell the story in order, numbered without a gap.
	_, events, err := NewService(e.store, nil, Timings{ReportInterval: 30 * time.Second, StaleAfter: flowStale},
		nil).Events(e.ctx, e.gs.GroupID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var story []string
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, ev.Seq)
		}
		if ev.Kind != EventVerdictChanged {
			story = append(story, ev.Kind+":"+ev.To)
		}
	}
	want := "group_created:registered flow_started:setup state_changed:configuring flow_ended:completed state_changed:syncing"
	if strings.Join(story, " ") != want {
		t.Errorf("events = %v, want %s", story, want)
	}
}

// The roadmap's mid-failure case: the primary's step fails. The flow stops in
// error; acknowledged, the group can be set up again with the same secret, so
// that the standby, already set up, finds nothing to change.
func TestFailedSetupStopsAndCanBeAskedAgain(t *testing.T) {
	e := newFlowEnv(t)
	e.report(RolePrimary, resourceGone)
	e.report(RoleStandby, resourceGone)
	_, flow, err := e.flows.Setup(e.ctx, e.gs.Name, "test")
	if err != nil {
		t.Fatal(err)
	}
	sealed := e.sealed()
	e.runJob(RoleStandby, flow.Jobs[0].JobID, job.OutcomeCompleted, model.ReplicationJobResult{DiskBytes: 1 << 30})
	e.report(RoleStandby, upSecondary)
	e.advance()
	primaryJob := e.latest().Jobs[1].JobID
	e.runJob(RolePrimary, primaryJob, job.OutcomeFailed, model.ReplicationJobResult{})
	e.advance()

	f := e.latest()
	if f.Status != FlowFailed || !strings.Contains(f.Message, "agent says failed") {
		t.Fatalf("flow = %+v", f)
	}
	if s := e.state(); s != StateError {
		t.Fatalf("state = %s, want error", s)
	}
	if _, _, err := e.flows.Setup(e.ctx, e.gs.Name, "test"); !errors.Is(err, ErrWrongState) {
		t.Fatalf("setup from error: %v", err)
	}

	gs, err := e.flows.Acknowledge(e.ctx, e.gs.Name, "operator")
	if err != nil || gs.State != StateRegistered {
		t.Fatalf("acknowledge: %s, %v", gs.State, err)
	}
	if _, flow2, err := e.flows.Setup(e.ctx, e.gs.Name, "test"); err != nil || flow2.FlowID == flow.FlowID {
		t.Fatalf("setup again: %+v, %v", flow2, err)
	}
	if !bytes.Equal(e.sealed(), sealed) {
		t.Error("asking again changed the shared secret, which would rewrite the standby's resource file")
	}
}

// A job that reports success but the node never bears out stops the flow:
// the next step would rest on something nobody can see.
func TestAStepTheNodeNeverBearsOutStopsTheFlow(t *testing.T) {
	e := newFlowEnv(t)
	e.report(RolePrimary, resourceGone)
	e.report(RoleStandby, resourceGone)
	_, flow, err := e.flows.Setup(e.ctx, e.gs.Name, "test")
	if err != nil {
		t.Fatal(err)
	}
	e.runJob(RoleStandby, flow.Jobs[0].JobID, job.OutcomeCompleted, model.ReplicationJobResult{})
	if _, err := e.store.pool.Exec(e.ctx, `UPDATE jobs SET finished_at = now() - interval '10 minutes' WHERE job_id = $1`,
		flow.Jobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	e.report(RoleStandby, resourceGone)
	e.advance()
	if f := e.latest(); f.Status != FlowFailed || !strings.Contains(f.Message, "do not show") {
		t.Fatalf("flow = %+v", f)
	}
	if s := e.state(); s != StateError {
		t.Fatalf("state = %s", s)
	}
}

func TestTeardownTakesThePrimaryDownFirst(t *testing.T) {
	e := newFlowEnv(t)
	if _, err := e.store.pool.Exec(e.ctx, `UPDATE ha_groups SET state = 'protected' WHERE group_id = $1`, e.gs.GroupID); err != nil {
		t.Fatal(err)
	}
	e.report(RolePrimary, upPrimary)
	e.report(RoleStandby, upSecondary)

	_, flow, err := e.flows.Teardown(e.ctx, e.gs.Name, "test")
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if flow.Jobs[0].SystemID != e.gs.Primary.SystemID {
		t.Fatalf("first job on %s, want the primary", flow.Jobs[0].SystemID)
	}
	claimed := e.runJob(RolePrimary, flow.Jobs[0].JobID, job.OutcomeCompleted, model.ReplicationJobResult{})
	if claimed.Replication != nil {
		t.Error("a teardown job was handed the shared secret")
	}
	e.report(RolePrimary, resourceGone)
	e.advance()
	f := e.latest()
	if f.Step != 2 || f.Jobs[1].SystemID != e.gs.Standby.SystemID {
		t.Fatalf("after the primary: %+v", f)
	}
	e.runJob(RoleStandby, f.Jobs[1].JobID, job.OutcomeCompleted, model.ReplicationJobResult{})
	e.report(RoleStandby, resourceGone)
	e.advance()
	if f := e.latest(); f.Status != FlowCompleted {
		t.Fatalf("flow = %+v", f)
	}
	if s := e.state(); s != StateRegistered {
		t.Fatalf("state = %s, want registered", s)
	}
}

func TestFlowStartRefusals(t *testing.T) {
	t.Run("silent node", func(t *testing.T) {
		e := newFlowEnv(t)
		e.report(RolePrimary, resourceGone)
		if _, _, err := e.flows.Setup(e.ctx, e.gs.Name, "test"); !errors.Is(err, ErrNodeSilent) {
			t.Fatalf("err = %v, want ErrNodeSilent", err)
		}
	})
	t.Run("a node has a job", func(t *testing.T) {
		e := newFlowEnv(t)
		e.report(RolePrimary, resourceGone)
		e.report(RoleStandby, resourceGone)
		if _, err := job.NewPGStore(e.store.pool).Insert(e.ctx, job.NewJob{
			JobID: randomHex(t, 8), SystemID: e.gs.Primary.SystemID, Kind: job.KindBackup,
			Spec: json.RawMessage(`{}`), RequestedBy: "test",
		}, time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.flows.Setup(e.ctx, e.gs.Name, "test"); !errors.Is(err, ErrSystemBusy) {
			t.Fatalf("err = %v, want ErrSystemBusy", err)
		}
		if s := e.state(); s != StateRegistered {
			t.Errorf("a refused setup moved the group to %s", s)
		}
	})
	t.Run("no data disks", func(t *testing.T) {
		e := newFlowEnv(t)
		if _, err := e.store.pool.Exec(e.ctx, `UPDATE ha_nodes SET disk = NULL WHERE group_id = $1`, e.gs.GroupID); err != nil {
			t.Fatal(err)
		}
		e.report(RolePrimary, resourceGone)
		e.report(RoleStandby, resourceGone)
		if _, _, err := e.flows.Setup(e.ctx, e.gs.Name, "test"); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("err = %v, want ErrInvalidRequest", err)
		}
	})
	t.Run("no key", func(t *testing.T) {
		e := newFlowEnv(t)
		e.report(RolePrimary, resourceGone)
		e.report(RoleStandby, resourceGone)
		keyless := NewFlowService(e.store, nil, Timings{ReportInterval: 30 * time.Second, StaleAfter: flowStale},
			5*time.Minute, func() string { return randomHex(t, 8) })
		if _, _, err := keyless.Setup(e.ctx, e.gs.Name, "test"); !errors.Is(err, ErrNoSecretKey) {
			t.Fatalf("err = %v, want ErrNoSecretKey", err)
		}
	})
	t.Run("teardown while configuring", func(t *testing.T) {
		e := newFlowEnv(t)
		e.report(RolePrimary, resourceGone)
		e.report(RoleStandby, resourceGone)
		if _, _, err := e.flows.Setup(e.ctx, e.gs.Name, "test"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.flows.Teardown(e.ctx, e.gs.Name, "test"); !errors.Is(err, ErrFlowInProgress) {
			t.Fatalf("err = %v, want ErrFlowInProgress", err)
		}
	})
}

// A replication job that is no flow's is never run.
func TestAnHAJobOfNoFlowIsRefusedAtTheClaim(t *testing.T) {
	e := newFlowEnv(t)
	j, err := job.NewPGStore(e.store.pool).Insert(e.ctx, job.NewJob{
		JobID: randomHex(t, 8), SystemID: e.gs.Standby.SystemID, Kind: job.KindReplicationSetup,
		Spec: json.RawMessage(`{"flowId":"nope","step":1}`), RequestedBy: "test",
	}, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	who := job.Identity{AgentID: "agent", SystemID: e.gs.Standby.SystemID}
	if _, err := e.jobs.Claim(e.ctx, who, j.JobID, randomHex(t, 16)); !errors.Is(err, job.ErrFlowMoved) {
		t.Fatalf("err = %v, want ErrFlowMoved", err)
	}
}

func TestAcknowledgeRules(t *testing.T) {
	setState := func(e *flowEnv, s State) {
		if _, err := e.store.pool.Exec(e.ctx, `UPDATE ha_groups SET state = $2 WHERE group_id = $1`, e.gs.GroupID, string(s)); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("not in error", func(t *testing.T) {
		e := newFlowEnv(t)
		if _, err := e.flows.Acknowledge(e.ctx, e.gs.Name, "op"); !errors.Is(err, ErrWrongState) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("standby claims to be primary", func(t *testing.T) {
		e := newFlowEnv(t)
		e.report(RoleStandby, upPrimary)
		setState(e, StateError)
		if _, err := e.flows.Acknowledge(e.ctx, e.gs.Name, "op"); !errors.Is(err, ErrRolesDisagree) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an error that was no flow's goes to degraded", func(t *testing.T) {
		e := newFlowEnv(t)
		setState(e, StateError)
		gs, err := e.flows.Acknowledge(e.ctx, e.gs.Name, "op")
		if err != nil || gs.State != StateDegraded {
			t.Fatalf("state = %s, err = %v", gs.State, err)
		}
	})
	t.Run("an HA job still to run", func(t *testing.T) {
		e := newFlowEnv(t)
		setState(e, StateError)
		if _, err := job.NewPGStore(e.store.pool).Insert(e.ctx, job.NewJob{
			JobID: randomHex(t, 8), SystemID: e.gs.Primary.SystemID, Kind: job.KindReplicationTeardown,
			Spec: json.RawMessage(`{}`), RequestedBy: "test",
		}, 5*time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := e.flows.Acknowledge(e.ctx, e.gs.Name, "op"); !errors.Is(err, ErrSystemBusy) {
			t.Fatalf("err = %v", err)
		}
	})
}

// A database made by the H1 release has the HA tables without the columns
// and tables setup added. Migrate adds them, numbers the existing nodes by
// their role, and is a no-op when run again.
func TestMigrateAddsReplicationToAnOlderDatabase(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	schema := "ha_migrate_" + randomHex(t, 4)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	h1, _, ok := strings.Cut(schemaSQL, "-- Added with replication setup")
	if !ok {
		t.Fatal("schema.sql no longer marks where the replication setup additions begin")
	}
	if _, err := pool.Exec(ctx, h1); err != nil {
		t.Fatalf("create the H1 tables: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO ha_groups (group_id, name, state, resource, service) VALUES ('g', 'old', 'registered', '{}', '{}');
INSERT INTO ha_nodes (system_id, group_id, role, address) VALUES ('p', 'g', 'primary', '10.0.1.10'), ('s', 'g', 'standby', '10.0.2.10');`); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	rows, err := pool.Query(ctx, `SELECT system_id, node_id, disk IS NULL FROM ha_nodes ORDER BY system_id`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id     string
		nodeID int
		noDisk bool
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.id, &x.nodeID, &x.noDisk)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (row{"p", 0, true}) || got[1] != (row{"s", 1, true}) {
		t.Errorf("nodes = %+v", got)
	}
	var flows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ha_flows`).Scan(&flows); err != nil {
		t.Fatalf("ha_flows missing: %v", err)
	}
}
