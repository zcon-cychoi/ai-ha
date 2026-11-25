package job

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	backupmodel "github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// allKinds lists every Kind constant. The schema must admit each of them.
var allKinds = []Kind{
	KindBackup, KindReplicationSetup, KindReplicationTeardown,
	KindHADemote, KindHAPromote, KindHARejoin,
}

// fakeFlows stands in for the HA flows, answering every question the same way.
type fakeFlows struct {
	mu         sync.Mutex
	inProgress bool
	admitErr   error
	currentErr error
	secret     *model.ReplicationAccess
	admitted   int
	checked    int
}

func (f *fakeFlows) InProgress(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inProgress, nil
}

func (f *fakeFlows) Admit(context.Context, Job) (*model.ReplicationAccess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.admitted++
	if f.admitErr != nil {
		return nil, f.admitErr
	}
	return f.secret, nil
}

func (f *fakeFlows) Current(context.Context, Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked++
	return f.currentErr
}

func (f *fakeFlows) set(fn func(*fakeFlows)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// testServiceWithFlows returns a service whose HA questions flows answers.
func testServiceWithFlows(t *testing.T, flows Flows) (*Service, *PGStore, *pgxpool.Pool, *fakeRepos) {
	t.Helper()
	_, store, pool, repos := testServiceWithRepos(t)
	return NewService(store, repos, flows, testTTLs(), 5*time.Minute, testTimings()), store, pool, repos
}

// enqueueHA creates an HA job as a flow would, and schedules it for cleanup.
func enqueueHA(t *testing.T, svc *Service, store *PGStore, pool *pgxpool.Pool, kind Kind) Job {
	t.Helper()
	j, err := store.Insert(context.Background(), NewJob{
		JobID:       backupmodel.NewID(),
		SystemID:    testSystemID,
		Kind:        kind,
		Spec:        json.RawMessage(`{"flowId":"flow-1","step":1}`),
		RequestedBy: "ha-flow",
	}, svc.TTL(kind))
	if err != nil {
		t.Fatalf("enqueue %s: %v", kind, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM jobs WHERE job_id = $1`, j.JobID)
	})
	return j
}

var errFlowsDown = errors.New("flow store unavailable")

func TestTTLByKind(t *testing.T) {
	svc := NewService(nil, nil, nil, TTLs{Backup: 24 * time.Hour, HA: 5 * time.Minute}, time.Minute, testTimings())
	for _, k := range allKinds {
		want := 5 * time.Minute
		if k == KindBackup {
			want = 24 * time.Hour
		}
		if got := svc.TTL(k); got != want {
			t.Errorf("TTL(%s) = %s, want %s", k, got, want)
		}
	}
}

// The tables of a database created when only backups existed must take the HA
// kinds after Migrate, keep their rows, and still refuse a kind nobody
// defined. Migrate runs on every command, so a second run must leave the
// constraint alone rather than rebuild it.
func TestMigrateWidensTheKindsOfAnOlderDatabase(t *testing.T) {
	dsn := os.Getenv("AIHA_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("AIHA_TEST_DB_DSN not set")
	}
	ctx := context.Background()
	pool := isolatedSchema(t, dsn)

	// The jobs table as the first release created it.
	const firstReleaseJobs = `
CREATE TABLE jobs (
    job_id             text        PRIMARY KEY,
    system_id          text        NOT NULL,
    kind               text        NOT NULL CHECK (kind IN ('backup')),
    status             text        NOT NULL
        CHECK (status IN ('queued', 'claimed', 'running', 'completed',
                          'failed', 'cancelling', 'cancelled')),
    spec               jsonb       NOT NULL,
    current_attempt_id text,
    cancel_requested   boolean     NOT NULL DEFAULT false,
    message            text,
    requested_by       text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL,
    started_at         timestamptz,
    finished_at        timestamptz
)`
	if _, err := pool.Exec(ctx, firstReleaseJobs); err != nil {
		t.Fatalf("create the first release's table: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO jobs (job_id, system_id, kind, status, spec, expires_at, finished_at)
VALUES ('old-backup', $1, 'backup', 'completed', '{"devices":["/dev/sda1"]}', now(), now())`, testSystemID); err != nil {
		t.Fatalf("seed an old backup: %v", err)
	}
	store := NewPGStore(pool)
	if _, err := store.Insert(ctx, NewJob{JobID: "before", SystemID: testSystemID, Kind: KindHAPromote, Spec: json.RawMessage(`{}`)}, time.Minute); err == nil {
		t.Fatal("the first release's table took an HA job: the test is not starting from the old schema")
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	widened := kindCheckOID(t, pool)
	for i := range 2 {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatalf("migrate again (%d): %v", i+1, err)
		}
	}
	if got := kindCheckOID(t, pool); got != widened {
		t.Error("a repeated Migrate rebuilt a constraint that already admitted every kind")
	}

	for _, k := range allKinds {
		if _, err := store.Insert(ctx, NewJob{JobID: "new-" + string(k), SystemID: testSystemID, Kind: k, Spec: json.RawMessage(`{}`)}, time.Minute); err != nil {
			t.Errorf("kind %s refused after Migrate: %v", k, err)
		}
	}
	if _, err := store.Insert(ctx, NewJob{JobID: "bogus", SystemID: testSystemID, Kind: "bogus", Spec: json.RawMessage(`{}`)}, time.Minute); err == nil {
		t.Error("an undefined kind was accepted")
	}

	old, _, err := store.Get(ctx, "old-backup")
	if err != nil {
		t.Fatalf("the old backup is gone: %v", err)
	}
	if old.Kind != KindBackup || old.Status != StatusCompleted {
		t.Errorf("old backup = %s/%s, want backup/completed", old.Kind, old.Status)
	}
}

// isolatedSchema returns a pool confined to a schema of its own, so that a
// test can create tables as an older release did without touching the ones
// other tests share.
func isolatedSchema(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	schema := "job_migrate_" + hex.EncodeToString(b)

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to %s: %v", schema, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func kindCheckOID(t *testing.T, pool *pgxpool.Pool) uint32 {
	t.Helper()
	var oid uint32
	err := pool.QueryRow(context.Background(), `
SELECT oid FROM pg_constraint WHERE conrelid = 'jobs'::regclass AND conname = 'jobs_kind_check'`).Scan(&oid)
	if err != nil {
		t.Fatalf("read jobs_kind_check: %v", err)
	}
	return oid
}

// A job enqueued in a flow's transaction exists only if that transaction
// commits, and waits as long as its kind allows.
func TestInsertTxFollowsTheCallersTransaction(t *testing.T) {
	svc, store, pool := testService(t)
	ctx := context.Background()
	newJob := func() NewJob {
		return NewJob{JobID: backupmodel.NewID(), SystemID: testSystemID, Kind: KindHAPromote, Spec: json.RawMessage(`{}`)}
	}

	rolledBack := newJob()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := InsertTx(ctx, tx, rolledBack, svc.TTL(rolledBack.Kind)); err != nil {
			t.Fatalf("insert: %v", err)
		}
		return errors.New("the flow's own change failed")
	})
	if err == nil {
		t.Fatal("the transaction was expected to roll back")
	}
	if _, _, err := store.Get(ctx, rolledBack.JobID); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("a job outlived the transaction that created it: %v", err)
	}

	committed := newJob()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := InsertTx(ctx, tx, committed, svc.TTL(committed.Kind))
		return err
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM jobs WHERE job_id = $1`, committed.JobID) })

	j, _, err := store.Get(ctx, committed.JobID)
	if err != nil {
		t.Fatalf("the committed job is missing: %v", err)
	}
	if ttl := j.ExpiresAt.Sub(j.CreatedAt); ttl < 4*time.Minute || ttl > 6*time.Minute {
		t.Errorf("an HA job waits %s, want the HA TTL of 5m", ttl)
	}
}

// An HA job's agent receives the replication secret its flow hands out, and
// no repository: none was opened for it.
func TestClaimingAnHAJobDeliversItsSecret(t *testing.T) {
	flows := &fakeFlows{secret: &model.ReplicationAccess{SharedSecret: "test-shared-secret"}}
	svc, store, pool, repos := testServiceWithFlows(t, flows)
	j := enqueueHA(t, svc, store, pool, KindReplicationSetup)

	claimed, err := svc.Claim(context.Background(), identity(), j.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.Replication == nil || claimed.Replication.SharedSecret != "test-shared-secret" {
		t.Errorf("replication = %+v, want the flow's secret", claimed.Replication)
	}
	if claimed.Repository != (model.RepositoryAccess{}) {
		t.Errorf("an HA job was handed a repository: %+v", claimed.Repository)
	}
	if repos.calls() != 0 {
		t.Errorf("repository credentials were opened %d time(s) for an HA job", repos.calls())
	}
	if claimed.Job.Status != StatusClaimed {
		t.Errorf("status = %s, want claimed", claimed.Job.Status)
	}
}

// A job whose flow has moved on can never run. It is ended at the claim,
// before any secret leaves, so that it is not handed out on every poll.
func TestClaimEndsAnHAJobWhoseFlowMovedOn(t *testing.T) {
	flows := &fakeFlows{admitErr: ErrFlowMoved}
	svc, store, pool, _ := testServiceWithFlows(t, flows)
	j := enqueueHA(t, svc, store, pool, KindHAPromote)

	_, err := svc.Claim(context.Background(), identity(), j.JobID, testClaimKey)
	if !errors.Is(err, ErrFlowMoved) {
		t.Fatalf("err = %v, want ErrFlowMoved", err)
	}
	got, attempt, err := store.Get(context.Background(), j.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || got.FinishedAt == nil || got.Message == "" {
		t.Errorf("job = %s (finished %v, %q), want failed with a reason", got.Status, got.FinishedAt, got.Message)
	}
	if attempt != nil {
		t.Errorf("a refused claim created attempt %s", attempt.AttemptID)
	}
	if _, err := svc.Waiting(context.Background(), testSystemID); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("the ended job is still handed out: %v", err)
	}
}

// A flow that cannot be asked is not a flow that said no: the claim changes
// nothing and may be repeated.
func TestClaimLeavesAnHAJobQueuedWhenItsFlowCannotBeChecked(t *testing.T) {
	flows := &fakeFlows{admitErr: errFlowsDown}
	svc, store, pool, _ := testServiceWithFlows(t, flows)
	j := enqueueHA(t, svc, store, pool, KindReplicationSetup)

	if _, err := svc.Claim(context.Background(), identity(), j.JobID, testClaimKey); err == nil || errors.Is(err, ErrFlowMoved) {
		t.Fatalf("err = %v, want the flow store's error", err)
	}
	if s := statusOf(t, svc, j.JobID); s != StatusQueued {
		t.Fatalf("status = %s, want queued", s)
	}

	flows.set(func(f *fakeFlows) { f.admitErr = nil })
	if _, err := svc.Claim(context.Background(), identity(), j.JobID, testClaimKey); err != nil {
		t.Fatalf("the claim could not be repeated once the flow answered: %v", err)
	}
}

// The agent does nothing until its start is acknowledged, so a flow that moved
// on between the claim and the start report ends the job untouched. The
// attempt is closed with it: whatever the agent sends next is refused.
func TestStartReportEndsAnHAJobWhoseFlowMovedOn(t *testing.T) {
	flows := &fakeFlows{}
	svc, store, pool, _ := testServiceWithFlows(t, flows)
	j := enqueueHA(t, svc, store, pool, KindHAPromote)
	ctx := context.Background()

	claimed, err := svc.Claim(ctx, identity(), j.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	flows.set(func(f *fakeFlows) { f.currentErr = ErrFlowMoved })

	_, err = svc.RecordProgress(ctx, identity(), j.JobID, claimed.Attempt.AttemptID, claimed.LeaseToken, Progress{Seq: 1, Phase: "starting"})
	if !errors.Is(err, ErrFlowMoved) {
		t.Fatalf("err = %v, want ErrFlowMoved", err)
	}
	got, attempt, err := store.Get(ctx, j.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || got.StartedAt != nil {
		t.Errorf("job = %s (started %v), want failed and never started", got.Status, got.StartedAt)
	}
	if attempt == nil || attempt.FinishedAt == nil || attempt.Outcome != OutcomeFailed {
		t.Fatalf("attempt = %+v, want closed as failed", attempt)
	}

	flows.set(func(f *fakeFlows) { f.currentErr = nil })
	if _, err := svc.RecordProgress(ctx, identity(), j.JobID, claimed.Attempt.AttemptID, claimed.LeaseToken, Progress{Seq: 2}); !errors.Is(err, ErrLeaseInvalid) {
		t.Errorf("a report after the refusal: err = %v, want ErrLeaseInvalid", err)
	}
	// The framework recorded the attempt as failed, so a result claiming
	// anything else contradicts it.
	if _, err := svc.RecordResult(ctx, identity(), j.JobID, claimed.Attempt.AttemptID, claimed.LeaseToken,
		Result{Outcome: OutcomeCompleted}); !errors.Is(err, ErrResultConflict) {
		t.Errorf("a result after the refusal: err = %v, want ErrResultConflict", err)
	}
	if s := statusOf(t, svc, j.JobID); s != StatusFailed {
		t.Errorf("status after a late result = %s, want failed", s)
	}
}

// Once a step has started, the flow is not asked again: refusing a report
// would stop the agent partway through the step.
func TestReportsAfterTheStartAreNotCheckedAgainstTheFlow(t *testing.T) {
	flows := &fakeFlows{}
	svc, store, pool, _ := testServiceWithFlows(t, flows)
	j := enqueueHA(t, svc, store, pool, KindHADemote)
	ctx := context.Background()

	claimed, err := svc.Claim(ctx, identity(), j.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := svc.RecordProgress(ctx, identity(), j.JobID, claimed.Attempt.AttemptID, claimed.LeaseToken, Progress{Seq: 1}); err != nil {
		t.Fatalf("start: %v", err)
	}
	flows.set(func(f *fakeFlows) { f.currentErr = ErrFlowMoved })
	if _, err := svc.RecordProgress(ctx, identity(), j.JobID, claimed.Attempt.AttemptID, claimed.LeaseToken, Progress{Seq: 2, Advanced: true}); err != nil {
		t.Fatalf("a report from a started step was refused: %v", err)
	}
	if s := statusOf(t, svc, j.JobID); s != StatusRunning {
		t.Errorf("status = %s, want running", s)
	}
	flows.mu.Lock()
	defer flows.mu.Unlock()
	if flows.checked != 1 {
		t.Errorf("the flow was asked %d times, want once, at the start", flows.checked)
	}
}

func TestBackupIsRefusedWhileTheSystemIsInAnHAOperation(t *testing.T) {
	flows := &fakeFlows{inProgress: true}
	svc, _, pool, _ := testServiceWithFlows(t, flows)

	_, err := svc.CreateBackup(context.Background(), testSystemID,
		BackupSpec{Devices: []string{"/dev/sda1"}, RepositoryID: testRepoID}, "test")
	if !errors.Is(err, ErrSystemInFlow) {
		t.Fatalf("err = %v, want ErrSystemInFlow", err)
	}

	flows.set(func(f *fakeFlows) { f.inProgress = false })
	j := enqueue(t, svc, pool)
	if j.Kind != KindBackup {
		t.Fatalf("kind = %s", j.Kind)
	}
}

func TestHAJobCannotBeCancelled(t *testing.T) {
	svc, store, pool, _ := testServiceWithFlows(t, &fakeFlows{})
	j := enqueueHA(t, svc, store, pool, KindReplicationTeardown)

	if _, err := svc.RequestCancel(context.Background(), j.JobID); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("err = %v, want ErrNotCancellable", err)
	}
	if s := statusOf(t, svc, j.JobID); s != StatusQueued {
		t.Errorf("status = %s, want queued", s)
	}
}

// A server without HA flows creates no HA jobs. Finding one is a fault: it is
// refused without being ended, and no secret is handed out.
func TestHAJobOnAServerWithoutFlowsIsRefused(t *testing.T) {
	svc, store, pool := testService(t)
	j := enqueueHA(t, svc, store, pool, KindReplicationSetup)

	_, err := svc.Claim(context.Background(), identity(), j.JobID, testClaimKey)
	if err == nil || errors.Is(err, ErrFlowMoved) {
		t.Fatalf("err = %v, want a refusal that is not ErrFlowMoved", err)
	}
	if s := statusOf(t, svc, j.JobID); s != StatusQueued {
		t.Errorf("status = %s, want queued", s)
	}
}
