package job

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The functions here act inside a transaction the caller holds, as InsertTx
// does. They are for the HA flows (pkg/framework/ha): a flow moves its own
// record and the jobs of its steps together, so that a framework that stops
// in between leaves neither a step without its job nor a job of a flow that
// has already ended.

// GetTx reads a job and its current attempt inside tx. It returns
// ErrJobNotFound for a job that does not exist.
func GetTx(ctx context.Context, tx pgx.Tx, jobID string) (Job, *Attempt, error) {
	return getJob(ctx, tx, jobID)
}

// RejectTx ends a job that has not started, as Store.Reject does, inside tx.
// A job that has started or ended is left as it is.
func RejectTx(ctx context.Context, tx pgx.Tx, jobID, message string) error {
	if err := rejectJob(ctx, tx, jobID, message); err != nil {
		return fmt.Errorf("reject job %s: %w", jobID, err)
	}
	return nil
}

// activeSQL selects the jobs of some systems that have not ended. A queued
// job past its expiry counts as ended: no agent can claim it, and the sweeper
// is about to say so.
const activeSQL = `SELECT ` + jobColumns + `
  FROM jobs
 WHERE system_id = ANY($1)
   AND (status IN ('claimed', 'running', 'cancelling')
        OR (status = 'queued' AND expires_at > now()))
 ORDER BY created_at, job_id`

// ActiveTx returns the jobs of systemIDs that have not ended, oldest first,
// inside tx.
func ActiveTx(ctx context.Context, tx pgx.Tx, systemIDs []string) ([]Job, error) {
	rows, err := tx.Query(ctx, activeSQL, systemIDs)
	if err != nil {
		return nil, fmt.Errorf("list unfinished jobs: %w", err)
	}
	jobs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Job, error) {
		return scanJob(row)
	})
	if err != nil {
		return nil, fmt.Errorf("list unfinished jobs: %w", err)
	}
	return jobs, nil
}
