package job

import (
	"context"
	"fmt"
	"time"
)

// Sweep applies the timeouts that move jobs nobody is reporting on.
//
// Each transition below is chosen by what the agent may already have written,
// not by how long it has been quiet:
//
//   - A queued job was never taken, so nothing happened and it simply expires.
//   - A claimed job was taken but never reported starting. Nothing has been
//     written to the repository yet, so returning it to the queue is safe and
//     another agent can pick it up.
//   - A running job's agent stopped reporting while it may have been reading
//     disks and uploading. Its lease expiring does not stop it. Handing the
//     job to a second agent would put two writers on the same target, so it is
//     failed instead and a person decides whether to ask again.
//   - A cancelling job's agent never confirmed it stopped, so the outcome is
//     unknown rather than cancelled.
func (s *PGStore) Sweep(ctx context.Context, claimedTimeout time.Duration) (SweepCounts, error) {
	var counts SweepCounts

	steps := []struct {
		name  string
		sql   string
		args  []any
		count *int
	}{
		{
			name: "queued expired",
			sql: `
UPDATE jobs
   SET status = 'failed', finished_at = now(),
       message = 'No agent took this job before it expired'
 WHERE status = 'queued' AND expires_at <= now()`,
			count: &counts.QueuedExpired,
		},
		{
			// Back to the queue, not failed: the agent never reported starting,
			// so nothing was written and another agent may still run it.
			name: "claimed but never started",
			sql: `
UPDATE jobs j
   SET status = 'queued', current_attempt_id = NULL
  FROM attempts a
 WHERE a.attempt_id = j.current_attempt_id
   AND j.status = 'claimed'
   AND j.expires_at > now()
   AND a.created_at <= now() - ($1::bigint * interval '1 millisecond')`,
			args:  []any{claimedTimeout.Milliseconds()},
			count: &counts.ClaimedReturned,
		},
		{
			name: "running but abandoned",
			sql: `
UPDATE jobs j
   SET status = 'failed', finished_at = now(),
       message = 'The agent stopped reporting; the outcome of this attempt is unknown'
  FROM attempts a
 WHERE a.attempt_id = j.current_attempt_id
   AND j.status = 'running'
   AND a.lease_expires_at <= now()`,
			count: &counts.RunningAbandoned,
		},
		{
			name: "cancel unconfirmed",
			sql: `
UPDATE jobs j
   SET status = 'failed', finished_at = now(),
       message = 'The agent did not confirm it stopped; the outcome is unknown'
  FROM attempts a
 WHERE a.attempt_id = j.current_attempt_id
   AND j.status = 'cancelling'
   AND a.lease_expires_at <= now()`,
			count: &counts.CancelUnconfirmed,
		},
	}

	for _, step := range steps {
		tag, err := s.pool.Exec(ctx, step.sql, step.args...)
		if err != nil {
			return counts, fmt.Errorf("sweep %s: %w", step.name, err)
		}
		*step.count = int(tag.RowsAffected())
	}

	// Close the attempts behind the jobs just ended, so that a late report
	// from one of them finds nothing to move. The outcome is "unknown", not
	// "failed": the framework lost contact, which is not the same as knowing
	// the work went wrong.
	const closeAbandoned = `
UPDATE attempts a
   SET outcome = 'unknown', finished_at = now(),
       message = COALESCE(a.message, 'Contact was lost before a result arrived')
  FROM jobs j
 WHERE j.job_id = a.job_id
   AND a.finished_at IS NULL
   AND a.lease_expires_at <= now()
   AND j.status IN ('failed', 'cancelled')`
	if _, err := s.pool.Exec(ctx, closeAbandoned); err != nil {
		return counts, fmt.Errorf("sweep abandoned attempts: %w", err)
	}

	// An attempt that was returned to the queue is marked superseded, so its
	// reports are refused if it comes back.
	const supersedeReturned = `
UPDATE attempts a
   SET superseded = true
  FROM jobs j
 WHERE j.job_id = a.job_id
   AND a.superseded = false
   AND a.finished_at IS NULL
   AND j.current_attempt_id IS DISTINCT FROM a.attempt_id`
	if _, err := s.pool.Exec(ctx, supersedeReturned); err != nil {
		return counts, fmt.Errorf("sweep superseded attempts: %w", err)
	}

	return counts, nil
}
