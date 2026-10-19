package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests exercise the SQL against a real PostgreSQL, because the
// properties that matter are properties of the statements: that a claim is
// atomic, that a lease actually gates a report, and that a sweep moves each
// state to the one its safety allows.

const (
	testSystemID = "sys-0123456789abcdef"
	testAgentID  = "agent-0123456789abcd"
	testClaimKey = "0123456789abcdef0123456789abcdef"
)

func testTimings() Timings {
	return Timings{ProgressInterval: 10 * time.Second, LeaseTTL: 90 * time.Second}
}

const testRepoID = "0123456789abcdef"

// fakeRepos stands in for the repository registry. failAccess makes Access
// fail, as a missing key or an undecryptable row would; accessCalls counts
// how often credentials were opened.
type fakeRepos struct {
	mu          sync.Mutex
	failAccess  bool
	accessCalls int
}

func (f *fakeRepos) Get(_ context.Context, idOrName string) (model.RepositoryInfo, error) {
	if idOrName == testRepoID || idOrName == "minio-main" {
		return model.RepositoryInfo{ID: testRepoID, Name: "minio-main"}, nil
	}
	return model.RepositoryInfo{}, errors.New("repository not found")
}

func (f *fakeRepos) Access(_ context.Context, id string) (model.RepositoryAccess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accessCalls++
	if f.failAccess || id != testRepoID {
		return model.RepositoryAccess{}, errors.New("secret cannot be decrypted with this key")
	}
	return model.RepositoryAccess{Type: "s3", Provider: "Minio", Bucket: "aiha-backup", AccessKey: "a", SecretKey: "test-secret"}, nil
}

func (f *fakeRepos) setFailAccess(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAccess = v
}

func (f *fakeRepos) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accessCalls
}

// completedDetail is what an agent reports for a captured set.
var completedDetail = json.RawMessage(`{"backupSetId":"9c0d1e2f3a4b5c6d","status":"completed"}`)

func testService(t *testing.T) (*Service, *PGStore, *pgxpool.Pool) {
	t.Helper()
	svc, store, pool, _ := testServiceWithRepos(t)
	return svc, store, pool
}

func testServiceWithRepos(t *testing.T) (*Service, *PGStore, *pgxpool.Pool, *fakeRepos) {
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
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewPGStore(pool)
	repos := &fakeRepos{}
	return NewService(store, repos, time.Hour, 5*time.Minute, testTimings()), store, pool, repos
}

func identity() Identity {
	return Identity{AgentID: testAgentID, SystemID: testSystemID}
}

// enqueue creates a job and schedules it for cleanup.
func enqueue(t *testing.T, svc *Service, pool *pgxpool.Pool) Job {
	t.Helper()
	job, err := svc.CreateBackup(context.Background(), testSystemID,
		BackupSpec{Devices: []string{"/dev/sda1"}, RepositoryID: testRepoID}, "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM jobs WHERE job_id = $1`, job.JobID)
	})
	return job
}

func statusOf(t *testing.T, svc *Service, jobID string) Status {
	t.Helper()
	job, _, err := svc.Get(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return job.Status
}

func TestQueueRoundTrip(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	if job.Status != StatusQueued {
		t.Fatalf("new job status = %s, want queued", job.Status)
	}

	waiting, err := svc.Waiting(ctx, testSystemID)
	if err != nil {
		t.Fatalf("waiting: %v", err)
	}
	if waiting.JobID != job.JobID {
		t.Fatalf("waiting returned %s, want %s", waiting.JobID, job.JobID)
	}
	// Handing a job out must not take it: a long-poll response can be lost.
	if statusOf(t, svc, job.JobID) != StatusQueued {
		t.Fatal("a job was taken merely by being handed out")
	}

	claimed, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.LeaseToken == "" || claimed.Attempt.AttemptID == "" {
		t.Fatalf("incomplete claim: %+v", claimed)
	}
	if statusOf(t, svc, job.JobID) != StatusClaimed {
		t.Fatal("a claimed job is not marked claimed")
	}
	// The agent is told how to report rather than compiling it in.
	if claimed.Timings.LeaseTTL != testTimings().LeaseTTL {
		t.Errorf("timings not handed to the agent: %+v", claimed.Timings)
	}

	// The first report is what starts the job running.
	if _, err := svc.RecordProgress(ctx, identity(), job.JobID, claimed.Attempt.AttemptID,
		claimed.LeaseToken, Progress{Seq: 1, Phase: "archiving", ProcessedBytes: 10, Advanced: true}); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if statusOf(t, svc, job.JobID) != StatusRunning {
		t.Fatal("a job that reported progress is not running")
	}

	finished, err := svc.RecordResult(ctx, identity(), job.JobID, claimed.Attempt.AttemptID,
		claimed.LeaseToken, Result{Outcome: OutcomeCompleted, Message: "done", Detail: completedDetail})
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if finished.Status != StatusCompleted || finished.FinishedAt == nil {
		t.Fatalf("finished job = %+v", finished)
	}
}

// Two agents handed the same job must not both run it.
func TestClaimIsAtomic(t *testing.T) {
	svc, _, pool := testService(t)
	job := enqueue(t, svc, pool)

	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			// Distinct keys, so each is a separate claim rather than a retry.
			_, errs[i] = svc.Claim(context.Background(), identity(), job.JobID, fmt.Sprintf("%032x", i))
		})
	}
	wg.Wait()

	var succeeded int
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrNotClaimable):
		default:
			t.Fatalf("goroutine %d: unexpected error %v", i, err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d agents claimed the same job, want 1", succeeded)
	}

	var attempts int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM attempts WHERE job_id = $1`, job.JobID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("%d attempts created, want 1", attempts)
	}
}

// A retry of a claim whose response was lost has to return the same attempt,
// or the job is stranded: it is no longer queued and the agent has no lease.
func TestClaimRetryReturnsTheSameAttempt(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	first, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}

	again, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again.Attempt.AttemptID != first.Attempt.AttemptID {
		t.Fatalf("retry made a new attempt: %s then %s",
			first.Attempt.AttemptID, again.Attempt.AttemptID)
	}

	var attempts int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM attempts WHERE job_id = $1`, job.JobID).Scan(&attempts); err != nil {
		t.Fatalf("count: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("%d attempts after a retry, want 1", attempts)
	}
}

// A different key finding the job claimed is refused: idempotency is scoped to
// one request, not to an agent, so an agent's stale request cannot join a
// newer attempt.
func TestClaimRefusesADifferentKey(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	if _, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	_, err := svc.Claim(ctx, identity(), job.JobID, "ffffffffffffffffffffffffffffffff")
	if !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("err = %v, want ErrNotClaimable", err)
	}
}

func TestProgressRequiresTheLease(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	claimed, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// A wrong lease token is refused, so a stale attempt cannot move the job.
	_, err = svc.RecordProgress(ctx, identity(), job.JobID, claimed.Attempt.AttemptID,
		"not-the-lease-token", Progress{Seq: 1})
	if !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("err = %v, want ErrLeaseInvalid", err)
	}

	// So is a report quoting an attempt that is not current.
	_, err = svc.RecordProgress(ctx, identity(), job.JobID, "some-other-attempt",
		claimed.LeaseToken, Progress{Seq: 1})
	if !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("err = %v, want ErrLeaseInvalid for an unknown attempt", err)
	}
}

// A report that only proves the agent is alive renews the lease without
// claiming the work moved. That separation is what tells a stuck agent from a
// vanished one.
func TestProgressSeparatesLivenessFromAdvance(t *testing.T) {
	svc, store, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	claimed, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	attemptID := claimed.Attempt.AttemptID

	if _, err := svc.RecordProgress(ctx, identity(), job.JobID, attemptID, claimed.LeaseToken,
		Progress{Seq: 1, ProcessedBytes: 100, Advanced: true}); err != nil {
		t.Fatalf("first progress: %v", err)
	}
	_, advanced, err := attemptTimes(ctx, pool, attemptID)
	if err != nil {
		t.Fatalf("read times: %v", err)
	}
	if advanced == nil {
		t.Fatal("an advancing report did not set last_progress_at")
	}

	time.Sleep(20 * time.Millisecond)

	// Alive but not advancing.
	if _, err := svc.RecordProgress(ctx, identity(), job.JobID, attemptID, claimed.LeaseToken,
		Progress{Seq: 2, ProcessedBytes: 100, Advanced: false}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	seen2, advanced2, err := attemptTimes(ctx, pool, attemptID)
	if err != nil {
		t.Fatalf("read times: %v", err)
	}
	if !advanced2.Equal(*advanced) {
		t.Error("a heartbeat moved last_progress_at, hiding a stuck agent")
	}
	if !seen2.After(*advanced) {
		t.Error("a heartbeat did not refresh last_seen_at")
	}

	// The lease must have been renewed even though nothing advanced.
	_, attempt, err := store.Get(ctx, job.JobID)
	if err != nil || attempt == nil {
		t.Fatalf("get: %v", err)
	}
	if !attempt.LeaseExpiresAt.After(time.Now()) {
		t.Error("a heartbeat did not renew the lease")
	}
}

func attemptTimes(ctx context.Context, pool *pgxpool.Pool, attemptID string) (*time.Time, *time.Time, error) {
	var seen, advanced *time.Time
	err := pool.QueryRow(ctx,
		`SELECT last_seen_at, last_progress_at FROM attempts WHERE attempt_id = $1`,
		attemptID).Scan(&seen, &advanced)
	return seen, advanced, err
}

// An out-of-order report is ignored rather than rewinding the recorded
// progress, but it still counts as contact.
func TestProgressIgnoresAnOlderSeq(t *testing.T) {
	svc, store, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	claimed, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	attemptID := claimed.Attempt.AttemptID

	for _, p := range []Progress{
		{Seq: 1, ProcessedBytes: 100, Advanced: true},
		{Seq: 5, ProcessedBytes: 500, Advanced: true},
		{Seq: 2, ProcessedBytes: 200, Advanced: true}, // late, out of order
	} {
		if _, err := svc.RecordProgress(ctx, identity(), job.JobID, attemptID, claimed.LeaseToken, p); err != nil {
			t.Fatalf("progress seq %d: %v", p.Seq, err)
		}
	}

	_, attempt, err := store.Get(ctx, job.JobID)
	if err != nil || attempt == nil {
		t.Fatalf("get: %v", err)
	}
	if attempt.LastSeq != 5 {
		t.Errorf("last_seq = %d, want 5", attempt.LastSeq)
	}
	if attempt.ProcessedBytes != 500 {
		t.Errorf("processed = %d, want 500: a late report rewound progress", attempt.ProcessedBytes)
	}
}

// The agent cannot know its result arrived, so repeating it is accepted.
// Contradicting it is not.
func TestResultIsIdempotentButNotContradictable(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	claimed, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	result := Result{Outcome: OutcomeCompleted, Message: "done", Detail: completedDetail}

	if _, err := svc.RecordResult(ctx, identity(), job.JobID, claimed.Attempt.AttemptID,
		claimed.LeaseToken, result); err != nil {
		t.Fatalf("first result: %v", err)
	}
	if _, err := svc.RecordResult(ctx, identity(), job.JobID, claimed.Attempt.AttemptID,
		claimed.LeaseToken, result); err != nil {
		t.Fatalf("repeating the same result was refused: %v", err)
	}

	_, err = svc.RecordResult(ctx, identity(), job.JobID, claimed.Attempt.AttemptID,
		claimed.LeaseToken, Result{Outcome: OutcomeFailed, Message: "actually it broke"})
	if !errors.Is(err, ErrResultConflict) {
		t.Fatalf("err = %v, want ErrResultConflict", err)
	}

	if statusOf(t, svc, job.JobID) != StatusCompleted {
		t.Fatal("a contradicting result changed the job")
	}
}

// Holding a valid agent token is not authority over another system's work.
func TestJobsAreScopedToTheOwningSystem(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	stranger := Identity{AgentID: "agent-stranger00000", SystemID: "sys-someone-else000"}

	if _, err := svc.Claim(ctx, stranger, job.JobID, testClaimKey); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("claim by a stranger: err = %v, want ErrNotOwned", err)
	}

	claimed, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := svc.RecordProgress(ctx, stranger, job.JobID, claimed.Attempt.AttemptID,
		claimed.LeaseToken, Progress{Seq: 1}); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("progress by a stranger: err = %v, want ErrNotOwned", err)
	}
	if _, err := svc.RecordResult(ctx, stranger, job.JobID, claimed.Attempt.AttemptID,
		claimed.LeaseToken, Result{Outcome: OutcomeCompleted}); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("result by a stranger: err = %v, want ErrNotOwned", err)
	}
}

func TestCancelQueuedEndsImmediately(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	cancelled, err := svc.RequestCancel(ctx, job.JobID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// No agent has it, so there is nothing to confirm.
	if cancelled.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", cancelled.Status)
	}
}

// A running job cannot be cancelled by decree: the agent has to stop, and only
// it can say that it did.
func TestCancelRunningWaitsForTheAgent(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	claimed, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := svc.RecordProgress(ctx, identity(), job.JobID, claimed.Attempt.AttemptID,
		claimed.LeaseToken, Progress{Seq: 1, Advanced: true}); err != nil {
		t.Fatalf("progress: %v", err)
	}

	marked, err := svc.RequestCancel(ctx, job.JobID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if marked.Status != StatusCancelling {
		t.Fatalf("status = %s, want cancelling", marked.Status)
	}

	// The agent learns of it on its next report — there is no inbound path.
	reported, err := svc.RecordProgress(ctx, identity(), job.JobID, claimed.Attempt.AttemptID,
		claimed.LeaseToken, Progress{Seq: 2, Advanced: false})
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if !reported.CancelRequested {
		t.Fatal("the cancel was not delivered on the progress response")
	}

	final, err := svc.RecordResult(ctx, identity(), job.JobID, claimed.Attempt.AttemptID,
		claimed.LeaseToken, Result{Outcome: OutcomeCancelled, Message: "stopped"})
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if final.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", final.Status)
	}
}

func TestSpecReachesTheAgentUnchanged(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()

	spec := BackupSpec{Devices: []string{"/dev/nvme0n1p1", "/dev/nvme0n1p16"}, Compression: "zstd", RepositoryID: "minio-main"}
	job, err := svc.CreateBackup(ctx, testSystemID, spec, "operator")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE job_id = $1`, job.JobID) })

	waiting, err := svc.Waiting(ctx, testSystemID)
	if err != nil {
		t.Fatalf("waiting: %v", err)
	}
	var got BackupSpec
	if err := json.Unmarshal(waiting.Spec, &got); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if len(got.Devices) != 2 || got.Devices[1] != "/dev/nvme0n1p16" || got.Compression != "zstd" {
		t.Fatalf("spec changed in storage: %+v", got)
	}
	// A repository named by name is stored by ID, and no credential is.
	if got.RepositoryID != testRepoID {
		t.Fatalf("repository stored as %q, want its ID", got.RepositoryID)
	}
	var raw string
	if err := pool.QueryRow(ctx, `SELECT spec::text FROM jobs WHERE job_id = $1`, job.JobID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "test-secret") || strings.Contains(strings.ToLower(raw), "secretkey") {
		t.Fatalf("the stored spec holds a credential: %s", raw)
	}
}

func TestCreateBackupValidates(t *testing.T) {
	svc, _, _ := testService(t)
	ctx := context.Background()

	for _, tt := range []struct {
		name     string
		systemID string
		spec     BackupSpec
	}{
		{"no system", "", BackupSpec{Devices: []string{"/dev/sda1"}}},
		{"no devices", testSystemID, BackupSpec{}},
		{"empty device", testSystemID, BackupSpec{Devices: []string{""}, RepositoryID: testRepoID}},
		{"no repository", testSystemID, BackupSpec{Devices: []string{"/dev/sda1"}}},
		{"unknown repository", testSystemID, BackupSpec{Devices: []string{"/dev/sda1"}, RepositoryID: "nope"}},
	} {
		if _, err := svc.CreateBackup(ctx, tt.systemID, tt.spec, "test"); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", tt.name, err)
		}
	}
}

// A retried claim is only useful if the lease it returns actually works. The
// agent that lost the first response never saw the first token.
func TestRetriedClaimReturnsAWorkingLease(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	if _, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// The response to that claim is lost; the agent retries with the same key.
	retried, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}

	if _, err := svc.RecordProgress(ctx, identity(), job.JobID, retried.Attempt.AttemptID,
		retried.LeaseToken, Progress{Seq: 1, Advanced: true}); err != nil {
		t.Fatalf("the lease from a retried claim does not work: %v", err)
	}
}

// Once an attempt has reported, the agent evidently holds its token. A claim
// with the same key arriving after that is a stale duplicate, and answering it
// with a fresh lease would invalidate the token the running attempt is using —
// it would then fail its next report and abort itself mid-backup.
func TestStaleClaimDoesNotDisturbARunningAttempt(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	running, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := svc.RecordProgress(ctx, identity(), job.JobID, running.Attempt.AttemptID,
		running.LeaseToken, Progress{Seq: 1, Advanced: true}); err != nil {
		t.Fatalf("progress: %v", err)
	}

	// A duplicate of the original claim request turns up late.
	if _, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("a stale claim was answered: err = %v, want ErrNotClaimable", err)
	}

	// The running attempt's token must still work.
	if _, err := svc.RecordProgress(ctx, identity(), job.JobID, running.Attempt.AttemptID,
		running.LeaseToken, Progress{Seq: 2, Advanced: true}); err != nil {
		t.Fatalf("a stale claim broke the running attempt's lease: %v", err)
	}
}

// The repository is resolved before the claim is recorded: if its credentials
// cannot be opened, nothing changes, and the same claim succeeds once they can.
func TestClaimChangesNothingWhenTheRepositoryIsUnavailable(t *testing.T) {
	svc, _, pool, repos := testServiceWithRepos(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	repos.setFailAccess(true)
	if _, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey); err == nil {
		t.Fatal("claim succeeded without repository access")
	}
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM attempts WHERE job_id = $1`, job.JobID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || statusOf(t, svc, job.JobID) != StatusQueued {
		t.Fatalf("a failed claim left %d attempt(s), status %s", attempts, statusOf(t, svc, job.JobID))
	}

	repos.setFailAccess(false)
	claimed, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim after recovery: %v", err)
	}
	if claimed.Repository.SecretKey != "test-secret" || claimed.Repository.Bucket != "aiha-backup" {
		t.Fatalf("claim carries repository %+v", claimed.Repository)
	}

	// A retry whose repository lookup fails keeps the attempt it already has
	// and creates none; the next retry gets that same attempt back.
	repos.setFailAccess(true)
	if _, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey); err == nil {
		t.Fatal("retry succeeded without repository access")
	}
	repos.setFailAccess(false)
	again, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil || again.Attempt.AttemptID != claimed.Attempt.AttemptID {
		t.Fatalf("retry = %v, %v; want attempt %s", again.Attempt.AttemptID, err, claimed.Attempt.AttemptID)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM attempts WHERE job_id = $1`, job.JobID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("%d attempts, want 1", attempts)
	}
}

// Credentials are not opened for an agent that does not own the job.
func TestClaimByAnotherSystemOpensNoCredentials(t *testing.T) {
	svc, _, pool, repos := testServiceWithRepos(t)
	job := enqueue(t, svc, pool)
	stranger := Identity{AgentID: "agent-other", SystemID: "sys-other"}
	if _, err := svc.Claim(context.Background(), stranger, job.JobID, testClaimKey); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("err = %v, want ErrNotOwned", err)
	}
	if repos.calls() != 0 {
		t.Fatalf("credentials were opened %d time(s) for a stranger", repos.calls())
	}
}

// A job from before repositories were assigned can never run, and says so.
func TestClaimRefusesALegacyJob(t *testing.T) {
	svc, store, pool := testService(t)
	ctx := context.Background()
	legacy, err := store.Insert(ctx, NewJob{JobID: "legacy0123456789", SystemID: testSystemID, Kind: KindBackup,
		Spec: json.RawMessage(`{"devices":["/dev/sda1"]}`)}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE job_id = $1`, legacy.JobID) })
	if _, err := svc.Claim(ctx, identity(), legacy.JobID, testClaimKey); !errors.Is(err, ErrLegacyJob) {
		t.Fatalf("err = %v, want ErrLegacyJob", err)
	}
}

// A completed backup must name its set, or nothing can restore it.
func TestCompletedBackupMustReportItsSet(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)
	claimed, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, detail := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"backupSetId":""}`)} {
		_, err := svc.RecordResult(ctx, identity(), job.JobID, claimed.Attempt.AttemptID, claimed.LeaseToken,
			Result{Outcome: OutcomeCompleted, Detail: detail})
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("detail %s: err = %v, want ErrInvalidRequest", detail, err)
		}
	}
	if statusOf(t, svc, job.JobID) == StatusCompleted {
		t.Fatal("a completion without a set was recorded")
	}
}

// completeJob runs a job to completion reporting setID.
func completeJob(t *testing.T, svc *Service, job Job, setID string) {
	t.Helper()
	ctx := context.Background()
	claimed, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	detail := json.RawMessage(fmt.Sprintf(`{"backupSetId":%q,"status":"completed"}`, setID))
	if _, err := svc.RecordResult(ctx, identity(), job.JobID, claimed.Attempt.AttemptID, claimed.LeaseToken,
		Result{Outcome: OutcomeCompleted, Detail: detail}); err != nil {
		t.Fatalf("result: %v", err)
	}
}

func TestFindBackupSet(t *testing.T) {
	svc, store, pool := testService(t)
	ctx := context.Background()
	setID := fmt.Sprintf("set-%d", time.Now().UnixNano())

	if _, err := svc.FindBackupSet(ctx, setID); !errors.Is(err, ErrBackupSetNotFound) {
		t.Fatalf("unknown set: err = %v, want ErrBackupSetNotFound", err)
	}

	job := enqueue(t, svc, pool)
	completeJob(t, svc, job, setID)
	got, err := svc.FindBackupSet(ctx, setID)
	if err != nil || got.JobID != job.JobID || got.SystemID != testSystemID || got.RepositoryID != testRepoID {
		t.Fatalf("found = %+v, %v", got, err)
	}

	// A failed set is not restorable from the framework's records.
	failedSet := setID + "-failed"
	failed := enqueue(t, svc, pool)
	claimed, err := svc.Claim(ctx, identity(), failed.JobID, testClaimKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordResult(ctx, identity(), failed.JobID, claimed.Attempt.AttemptID, claimed.LeaseToken,
		Result{Outcome: OutcomeFailed, Detail: json.RawMessage(fmt.Sprintf(`{"backupSetId":%q,"status":"failed"}`, failedSet))}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FindBackupSet(ctx, failedSet); !errors.Is(err, ErrBackupSetNotFound) {
		t.Fatalf("failed set: err = %v, want ErrBackupSetNotFound", err)
	}

	// Two jobs claiming the same set: refuse rather than pick one.
	dup := enqueue(t, svc, pool)
	completeJob(t, svc, dup, setID)
	if _, err := svc.FindBackupSet(ctx, setID); !errors.Is(err, ErrBackupSetAmbiguous) {
		t.Fatalf("duplicate set: err = %v, want ErrBackupSetAmbiguous", err)
	}

	// A completed job from before repositories were assigned.
	legacySet := setID + "-legacy"
	legacy, err := store.Insert(ctx, NewJob{JobID: fmt.Sprintf("legacy%d", time.Now().UnixNano()), SystemID: testSystemID,
		Kind: KindBackup, Spec: json.RawMessage(`{"devices":["/dev/sda1"]}`)}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE job_id = $1`, legacy.JobID) })
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'completed' WHERE job_id = $1`, legacy.JobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO attempts (attempt_id, job_id, agent_id, lease_token_hash, claim_request_key,
		lease_expires_at, outcome, result) VALUES ($1, $2, 'a', '\x00', 'k', now(), 'completed', $3)`,
		legacy.JobID+"-a", legacy.JobID, fmt.Sprintf(`{"backupSetId":%q}`, legacySet)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FindBackupSet(ctx, legacySet); !errors.Is(err, ErrLegacyJob) {
		t.Fatalf("legacy set: err = %v, want ErrLegacyJob", err)
	}
}
