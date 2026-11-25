package ha

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests exercise the SQL against a real PostgreSQL, because what matters
// is a property of the statements together: that a report and the evaluation
// it triggers are one step, and that a group's events are numbered without
// gaps however its reports interleave.

func testPGStore(t *testing.T) *PGStore {
	t.Helper()
	dsn := os.Getenv("AIHA_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("AIHA_TEST_DB_DSN not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	// Applied twice: Migrate runs on every CLI invocation, so it must be safe
	// to repeat against a database that already has the tables.
	for range 2 {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	return NewPGStore(pool)
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}

// newTestGroup creates a group with fresh identifiers and removes it after the
// test.
func newTestGroup(t *testing.T, store *PGStore) GroupStatus {
	t.Helper()
	id := randomHex(t, 8)
	spec := GroupSpec{
		Name:     "test-" + id,
		Primary:  NodeSpec{SystemID: "sys-p-" + id, Address: "10.0.1.10"},
		Standby:  NodeSpec{SystemID: "sys-s-" + id, Address: "10.0.2.10"},
		Resource: ResourceSpec{Name: "r0", Device: "/dev/drbd0", MountPoint: "/data", Port: 7789},
		Service:  ServiceSpec{Unit: "ai.service", Port: 8000, HealthPath: "/healthz"},
	}
	gs, err := store.Create(context.Background(), NewGroup{GroupID: id, Spec: spec, CreatedBy: "test"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.pool.Exec(context.Background(), `DELETE FROM ha_groups WHERE group_id = $1`, id)
	})
	return gs
}

func reportFor(gs GroupStatus, role Role) model.AgentHealthReq {
	return model.AgentHealthReq{CollectedAt: time.Now().UTC(), GroupID: gs.GroupID, Role: string(role)}
}

func TestPGStoreCreateAndGet(t *testing.T) {
	store := testPGStore(t)
	ctx := context.Background()
	created := newTestGroup(t, store)

	if created.State != StateRegistered || created.Verdict != VerdictUnknown || created.EventSeq != 1 {
		t.Errorf("created = %+v", created.Group)
	}
	if created.Primary.Address != "10.0.1.10" || created.Standby.Role != RoleStandby {
		t.Errorf("nodes = %+v / %+v", created.Primary, created.Standby)
	}

	for _, key := range []string{created.GroupID, created.Name} {
		got, err := store.Get(ctx, key)
		if err != nil {
			t.Fatalf("get %q: %v", key, err)
		}
		if got.GroupID != created.GroupID || got.Resource.Name != "r0" || got.Service.Unit != "ai.service" {
			t.Errorf("get %q = %+v", key, got.Group)
		}
	}
	if _, err := store.Get(ctx, "no-such-group"); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("get unknown: err = %v, want ErrGroupNotFound", err)
	}

	events, err := store.Events(ctx, created.GroupID, 0, 10)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 1 || events[0].Seq != 1 || events[0].Kind != EventGroupCreated || events[0].To != "registered" {
		t.Errorf("events = %+v", events)
	}
}

func TestPGStoreCreateConflicts(t *testing.T) {
	store := testPGStore(t)
	ctx := context.Background()
	existing := newTestGroup(t, store)

	sameName := NewGroup{GroupID: randomHex(t, 8), Spec: GroupSpec{
		Name:    existing.Name,
		Primary: NodeSpec{SystemID: "sys-x-" + randomHex(t, 4), Address: "10.0.1.11"},
		Standby: NodeSpec{SystemID: "sys-y-" + randomHex(t, 4), Address: "10.0.2.11"},
	}}
	if _, err := store.Create(ctx, sameName); !errors.Is(err, ErrNameTaken) {
		t.Errorf("same name: err = %v, want ErrNameTaken", err)
	}

	// A system already holds a role in another group.
	taken := NewGroup{GroupID: randomHex(t, 8), Spec: GroupSpec{
		Name:    "other-" + randomHex(t, 4),
		Primary: NodeSpec{SystemID: existing.Standby.SystemID, Address: "10.0.1.12"},
		Standby: NodeSpec{SystemID: "sys-z-" + randomHex(t, 4), Address: "10.0.2.12"},
	}}
	if _, err := store.Create(ctx, taken); !errors.Is(err, ErrSystemInGroup) {
		t.Errorf("system in a group: err = %v, want ErrSystemInGroup", err)
	}
	// Nothing of the refused group may remain.
	if _, err := store.Get(ctx, taken.GroupID); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("refused group: err = %v, want ErrGroupNotFound", err)
	}
}

func TestPGStoreReport(t *testing.T) {
	store := testPGStore(t)
	ctx := context.Background()
	gs := newTestGroup(t, store)

	decided := 0
	decide := func(g GroupStatus) Change {
		decided++
		return Change{State: g.State, Verdict: VerdictAttention,
			Reasons: []Reason{{Code: "c", Severity: VerdictAttention, Message: "m"}},
			Events:  []NewEvent{{Kind: EventVerdictChanged, From: string(g.Verdict), To: "attention"}}}
	}

	if _, _, err := store.Report(ctx, "sys-none-"+randomHex(t, 4), "agent", model.AgentHealthReq{}, decide); !errors.Is(err, ErrNotAssigned) {
		t.Fatalf("unassigned: err = %v, want ErrNotAssigned", err)
	}

	// A node that has not learned its assignment is answered, not recorded.
	got, recorded, err := store.Report(ctx, gs.Primary.SystemID, "agent-p", model.AgentHealthReq{CollectedAt: time.Now()}, decide)
	if err != nil || recorded || decided != 0 || got.GroupID != gs.GroupID {
		t.Fatalf("no assignment: recorded=%v decided=%d err=%v group=%s", recorded, decided, err, got.GroupID)
	}

	// Measurements taken for the other role are not this node's state.
	if _, recorded, err = store.Report(ctx, gs.Primary.SystemID, "agent-p", reportFor(gs, RoleStandby), decide); err != nil || recorded {
		t.Fatalf("wrong role: recorded=%v err=%v", recorded, err)
	}

	got, recorded, err = store.Report(ctx, gs.Primary.SystemID, "agent-p", reportFor(gs, RolePrimary), decide)
	if err != nil || !recorded || decided != 1 {
		t.Fatalf("own role: recorded=%v decided=%d err=%v", recorded, decided, err)
	}
	if got.Verdict != VerdictAttention || got.EventSeq != 2 || got.Primary.Report == nil || got.Primary.AgentID != "agent-p" {
		t.Errorf("after report = %+v / %+v", got.Group, got.Primary)
	}

	stored, err := store.Get(ctx, gs.GroupID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Verdict != VerdictAttention || len(stored.Reasons) != 1 || stored.EvaluatedAt == nil ||
		stored.Primary.ReceivedAt == nil || stored.Standby.Report != nil {
		t.Errorf("stored = %+v / %+v / %+v", stored.Group, stored.Primary, stored.Standby)
	}
}

// TestPGStoreEventSequence has both nodes and the server's evaluator move one
// group at once. Every step records an event, and the group's sequence must
// come out with no gap and no repeat: a reader resuming after the last number
// it saw would otherwise miss one.
func TestPGStoreEventSequence(t *testing.T) {
	store := testPGStore(t)
	ctx := context.Background()
	gs := newTestGroup(t, store)

	decide := func(g GroupStatus) Change {
		return Change{State: g.State, Verdict: VerdictOK,
			Events: []NewEvent{{Kind: EventVerdictChanged, To: "ok"}}}
	}

	const perWriter = 15
	var wg sync.WaitGroup
	errs := make(chan error, 3*perWriter)
	for _, w := range []func() error{
		func() error {
			_, _, err := store.Report(ctx, gs.Primary.SystemID, "a", reportFor(gs, RolePrimary), decide)
			return err
		},
		func() error {
			_, _, err := store.Report(ctx, gs.Standby.SystemID, "b", reportFor(gs, RoleStandby), decide)
			return err
		},
		func() error {
			_, err := store.Evaluate(ctx, gs.GroupID, decide)
			return err
		},
	} {
		wg.Go(func() {
			for range perWriter {
				if err := w(); err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write: %v", err)
	}

	events, err := store.Events(ctx, gs.GroupID, 0, 1000)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	want := 1 + 3*perWriter
	if len(events) != want {
		t.Fatalf("events = %d, want %d", len(events), want)
	}
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
	}
	stored, err := store.Get(ctx, gs.GroupID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.EventSeq != int64(want) {
		t.Errorf("group event_seq = %d, want %d", stored.EventSeq, want)
	}

	// Resuming after a number returns exactly what followed it.
	tail, err := store.Events(ctx, gs.GroupID, int64(want-3), 10)
	if err != nil || len(tail) != 3 || tail[0].Seq != int64(want-2) {
		t.Errorf("resume: %d events from %v, err %v", len(tail), tail, err)
	}
}

func TestPGStoreStateCondition(t *testing.T) {
	store := testPGStore(t)
	ctx := context.Background()
	gs := newTestGroup(t, store)

	// The evaluator's move lands only on the state it was decided from.
	_, err := store.Evaluate(ctx, gs.GroupID, func(g GroupStatus) Change {
		return Change{State: StateSyncing, Verdict: VerdictOK,
			Events: []NewEvent{{Kind: EventStateChanged, From: string(g.State), To: string(StateSyncing)}}}
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	got, err := store.Get(ctx, gs.GroupID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != StateSyncing || got.EventSeq != 2 {
		t.Errorf("after move = %s seq %d", got.State, got.EventSeq)
	}

	if _, err := store.Evaluate(ctx, "no-such-group", func(g GroupStatus) Change { return Change{State: g.State} }); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("unknown group: err = %v, want ErrGroupNotFound", err)
	}
}

func TestPGStoreList(t *testing.T) {
	store := testPGStore(t)
	ctx := context.Background()
	a, b := newTestGroup(t, store), newTestGroup(t, store)

	groups, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := map[string]bool{}
	for _, g := range groups {
		if g.Primary.SystemID == "" || g.Standby.SystemID == "" {
			t.Errorf("group %s listed without its nodes", g.GroupID)
		}
		found[g.GroupID] = true
	}
	if !found[a.GroupID] || !found[b.GroupID] {
		t.Errorf("list is missing a test group")
	}

	ids, err := store.GroupIDs(ctx)
	if err != nil || len(ids) < 2 {
		t.Errorf("group ids = %v, err %v", ids, err)
	}
}

// TestServiceOverPostgres runs the whole H1 path on a real database: both
// nodes report healthy, the pair is judged ok and the sync confirmed; then the
// primary goes quiet, which no report can announce, and the server's own
// evaluation has to notice it.
func TestServiceOverPostgres(t *testing.T) {
	store := testPGStore(t)
	ctx := context.Background()
	id := randomHex(t, 8)
	primary, standby := "sys-p-"+id, "sys-s-"+id
	svc := NewService(store, systemSet{primary: true, standby: true}, testTimings, func() string { return id })

	spec := validSpec()
	spec.Name = "svc-" + id
	spec.Primary.SystemID, spec.Standby.SystemID = primary, standby
	gs, err := svc.Create(ctx, spec, "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.pool.Exec(context.Background(), `DELETE FROM ha_groups WHERE group_id = $1`, gs.GroupID)
	})

	report := func(system string, req *model.AgentHealthReq, role Role) {
		t.Helper()
		req.CollectedAt, req.GroupID, req.Role = time.Now().UTC(), gs.GroupID, string(role)
		resp, err := svc.Report(ctx, system, "agent-"+system, *req)
		if err != nil || !resp.Assigned || resp.Assignment.Role != string(role) {
			t.Fatalf("report from %s: %+v, err %v", system, resp, err)
		}
	}
	report(primary, healthyPrimary(), RolePrimary)
	report(standby, healthyStandby(), RoleStandby)

	got, err := svc.Get(ctx, gs.GroupID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Verdict != VerdictOK || got.LastSyncConfirmedAt == nil || got.State != StateRegistered {
		t.Fatalf("after both reports: %s %s %v (reasons %v)", got.State, got.Verdict, got.LastSyncConfirmedAt, got.Reasons)
	}

	// The primary's last report is pushed into the past, as if it had gone
	// quiet a while ago.
	if _, err := store.pool.Exec(ctx,
		`UPDATE ha_nodes SET report_received_at = now() - interval '5 minutes' WHERE system_id = $1`, primary); err != nil {
		t.Fatalf("age the report: %v", err)
	}
	if _, err := svc.EvaluateAll(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	got, err = svc.Get(ctx, gs.GroupID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Verdict != VerdictError || len(got.Reasons) == 0 || got.Reasons[0].Code != codePrimaryReportLost {
		t.Fatalf("after silence: %s %v", got.Verdict, got.Reasons)
	}

	_, events, err := svc.Events(ctx, gs.GroupID, 0, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var verdicts []string
	for _, e := range events {
		if e.Kind == EventVerdictChanged {
			verdicts = append(verdicts, e.From+">"+e.To)
		}
	}
	// unknown>attention: the primary alone; attention>ok: both; ok>error:
	// the primary silent.
	want := []string{"unknown>attention", "attention>ok", "ok>error"}
	if len(verdicts) != len(want) {
		t.Fatalf("verdict events = %v, want %v", verdicts, want)
	}
	for i := range want {
		if verdicts[i] != want[i] {
			t.Fatalf("verdict events = %v, want %v", verdicts, want)
		}
	}
}
