package job

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// age moves a timestamp column into the past, standing in for time passing.
func age(t *testing.T, pool *pgxpool.Pool, table, column, idColumn, id string) {
	t.Helper()
	q := "UPDATE " + table + " SET " + column + " = now() - interval '1 day' WHERE " + idColumn + " = $1"
	if _, err := pool.Exec(context.Background(), q, id); err != nil {
		t.Fatalf("age %s.%s: %v", table, column, err)
	}
}

// A job nobody came for simply ends.
func TestSweepExpiresUnclaimedJobs(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	age(t, pool, "jobs", "expires_at", "job_id", job.JobID)

	counts, err := svc.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if counts.QueuedExpired < 1 {
		t.Errorf("counts = %+v, want a queued expiry", counts)
	}
	if statusOf(t, svc, job.JobID) != StatusFailed {
		t.Fatal("an expired queued job was not failed")
	}
}

// A claimed job whose agent never started has written nothing, so it goes
// back to the queue for another agent. Failing it would lose work for no
// reason.
func TestSweepRequeuesAClaimNeverStarted(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	claimed, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	age(t, pool, "attempts", "created_at", "attempt_id", claimed.Attempt.AttemptID)

	if _, err := svc.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if statusOf(t, svc, job.JobID) != StatusQueued {
		t.Fatalf("a claim that never started was not requeued: %s", statusOf(t, svc, job.JobID))
	}

	// The abandoned attempt must be superseded, so it cannot report into the
	// job if its agent comes back.
	_, err = svc.RecordProgress(ctx, identity(), job.JobID, claimed.Attempt.AttemptID,
		claimed.LeaseToken, Progress{Seq: 1})
	if err == nil {
		t.Fatal("a superseded attempt could still report into the job")
	}

	// And another claim can take the requeued job.
	if _, err := svc.Claim(ctx, identity(), job.JobID, "ffffffffffffffffffffffffffffffff"); err != nil {
		t.Fatalf("the requeued job could not be claimed again: %v", err)
	}
}

// A claim that reaches the job's expiry without starting cannot be requeued,
// because the queue no longer offers an expired job. Left claimed, it would
// count as unfinished for ever and hold up an HA flow waiting on it. It fails,
// and its attempt is closed with it, so that a late start is refused.
func TestSweepFailsAClaimThatExpiredBeforeStarting(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	claimed, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	age(t, pool, "jobs", "expires_at", "job_id", job.JobID)

	counts, err := svc.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if counts.ClaimedExpired != 1 {
		t.Errorf("counts = %+v, want one claimed expiry", counts)
	}
	if status := statusOf(t, svc, job.JobID); status != StatusFailed {
		t.Fatalf("status = %s, want failed", status)
	}
	_, attempt, err := svc.Get(ctx, job.JobID)
	if err != nil || attempt == nil {
		t.Fatalf("get: %v", err)
	}
	if attempt.Outcome != OutcomeFailed || attempt.FinishedAt == nil {
		t.Errorf("attempt = %s finished %v, want failed and finished", attempt.Outcome, attempt.FinishedAt)
	}

	// The lease is still live, but the attempt is closed: the agent coming
	// back to start must be turned away.
	_, err = svc.RecordProgress(ctx, identity(), job.JobID, claimed.Attempt.AttemptID,
		claimed.LeaseToken, Progress{Seq: 1, Advanced: true})
	if !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("a late start report: err = %v, want ErrLeaseInvalid", err)
	}
	if status := statusOf(t, svc, job.JobID); status != StatusFailed {
		t.Fatalf("status after the late report = %s, want failed", status)
	}
}

// A start report can land on its attempt just as the job is ended or
// requeued, before the attempt is closed. The report must not be answered as
// a start: the agent would begin a job that is no longer its own.
func TestStartReportRefusedWhenTheJobEndedUnderIt(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	claimed, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// The job ends with its attempt still open: the moment between the two.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'failed', finished_at = now() WHERE job_id = $1`,
		job.JobID); err != nil {
		t.Fatalf("end the job: %v", err)
	}

	_, err = svc.RecordProgress(ctx, identity(), job.JobID, claimed.Attempt.AttemptID,
		claimed.LeaseToken, Progress{Seq: 1, Advanced: true})
	if !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("start report on an ended job: err = %v, want ErrLeaseInvalid", err)
	}
	if status := statusOf(t, svc, job.JobID); status != StatusFailed {
		t.Fatalf("status = %s, want failed", status)
	}
}

// Expiry is the deadline to start, whether or not the sweeper has run yet.
func TestStartReportRefusedOnceTheJobExpired(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	claimed, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	age(t, pool, "jobs", "expires_at", "job_id", job.JobID)

	_, err = svc.RecordProgress(ctx, identity(), job.JobID, claimed.Attempt.AttemptID,
		claimed.LeaseToken, Progress{Seq: 1, Advanced: true})
	if !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("start report after expiry: err = %v, want ErrLeaseInvalid", err)
	}
	if status := statusOf(t, svc, job.JobID); status != StatusClaimed {
		t.Fatalf("status = %s, want claimed until the sweeper ends it", status)
	}
	if _, err := svc.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if status := statusOf(t, svc, job.JobID); status != StatusFailed {
		t.Fatalf("status after sweep = %s, want failed", status)
	}
}

// The deadline is judged when the report holds the job, not when its
// transaction began. A start report that waits on the job's row while the
// claim expires must be refused, though its transaction opened before the
// expiry: now() would still be that earlier time.
func TestStartReportRefusedWhenTheJobExpiresWhileItWaits(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	claimed, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Another session holds the job's row and moves its expiry just ahead.
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var holderPID int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatalf("holder pid: %v", err)
	}
	var expiresAt time.Time
	if err := holder.QueryRow(ctx, `
UPDATE jobs SET expires_at = clock_timestamp() + interval '1 second'
 WHERE job_id = $1
RETURNING expires_at`, job.JobID).Scan(&expiresAt); err != nil {
		t.Fatalf("move the expiry: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := svc.RecordProgress(ctx, identity(), job.JobID, claimed.Attempt.AttemptID,
			claimed.LeaseToken, Progress{Seq: 1, Advanced: true})
		done <- err
	}()

	waitBlockedBy(t, pool, holderPID)
	var dbNow time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
		t.Fatalf("clock: %v", err)
	}
	if !dbNow.Before(expiresAt) {
		t.Fatal("the report began waiting only after the expiry; the test proves nothing")
	}

	// The expiry passes while the report waits; then the row is released.
	time.Sleep(expiresAt.Sub(dbNow) + 200*time.Millisecond)
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release the row: %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, ErrLeaseInvalid) {
			t.Fatalf("start report that waited past the expiry: err = %v, want ErrLeaseInvalid", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the start report did not return after the row was released")
	}
	if status := statusOf(t, svc, job.JobID); status != StatusClaimed {
		t.Fatalf("status = %s, want claimed until the sweeper ends it", status)
	}
}

// waitBlockedBy waits until some session waits on a lock that pid holds.
func waitBlockedBy(t *testing.T, pool *pgxpool.Pool, pid int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY (pg_blocking_pids(pid)))`,
			pid).Scan(&blocked); err != nil {
			t.Fatalf("look for a blocked session: %v", err)
		}
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no session began waiting on the held row")
}

// A job handed to a second attempt is that attempt's. The first one's report
// must be refused even though the job is running, which is what a check of
// the job's status alone would accept.
func TestReportRefusedWhenAnotherAttemptRunsTheJob(t *testing.T) {
	svc, _, pool := testService(t)
	ctx := context.Background()
	job := enqueue(t, svc, pool)

	first, err := svc.Claim(ctx, identity(), job.JobID, testClaimKey)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	age(t, pool, "attempts", "created_at", "attempt_id", first.Attempt.AttemptID)
	if _, err := svc.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	second, err := svc.Claim(ctx, identity(), job.JobID, "ffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if _, err := svc.RecordProgress(ctx, identity(), job.JobID, second.Attempt.AttemptID,
		second.LeaseToken, Progress{Seq: 1, Advanced: true}); err != nil {
		t.Fatalf("second attempt's start: %v", err)
	}

	_, err = svc.RecordProgress(ctx, identity(), job.JobID, first.Attempt.AttemptID,
		first.LeaseToken, Progress{Seq: 1, Advanced: true})
	if !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("first attempt's report: err = %v, want ErrLeaseInvalid", err)
	}
	j, attempt, err := svc.Get(ctx, job.JobID)
	if err != nil || attempt == nil {
		t.Fatalf("get: %v", err)
	}
	if j.Status != StatusRunning || attempt.AttemptID != second.Attempt.AttemptID {
		t.Fatalf("job %s on attempt %s, want running on %s", j.Status, attempt.AttemptID, second.Attempt.AttemptID)
	}
}

// This is the case the design turns on. A running agent that stops reporting
// may still be reading disks and uploading: its lease expiring does not stop
// it. Handing the job to a second agent would put two writers on one target,
// so it is failed instead.
func TestSweepFailsAbandonedRunningJobRatherThanRequeuing(t *testing.T) {
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
	age(t, pool, "attempts", "lease_expires_at", "attempt_id", claimed.Attempt.AttemptID)

	counts, err := svc.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if counts.RunningAbandoned < 1 {
		t.Errorf("counts = %+v, want an abandoned running job", counts)
	}

	status := statusOf(t, svc, job.JobID)
	if status == StatusQueued {
		t.Fatal("an abandoned running job was requeued: a second agent could now write alongside the first")
	}
	if status != StatusFailed {
		t.Fatalf("status = %s, want failed", status)
	}

	// The attempt's outcome is unknown, not failed: contact was lost, which
	// is not the same as knowing the work went wrong.
	_, attempt, err := svc.Get(ctx, job.JobID)
	if err != nil || attempt == nil {
		t.Fatalf("get: %v", err)
	}
	if attempt.Outcome != OutcomeUnknown {
		t.Errorf("attempt outcome = %q, want unknown", attempt.Outcome)
	}
}

// A cancel the agent never confirmed is not a cancellation.
func TestSweepDoesNotTreatAnUnconfirmedCancelAsCancelled(t *testing.T) {
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
	if _, err := svc.RequestCancel(ctx, job.JobID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	age(t, pool, "attempts", "lease_expires_at", "attempt_id", claimed.Attempt.AttemptID)

	if _, err := svc.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if status := statusOf(t, svc, job.JobID); status == StatusCancelled {
		t.Fatal("an unconfirmed cancel was recorded as cancelled")
	}
}

// A live job must be left alone.
func TestSweepLeavesHealthyJobsAlone(t *testing.T) {
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

	if _, err := svc.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if statusOf(t, svc, job.JobID) != StatusRunning {
		t.Fatalf("a healthy running job was moved to %s", statusOf(t, svc, job.JobID))
	}
}
