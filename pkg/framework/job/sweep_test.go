package job

import (
	"context"
	"testing"

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
