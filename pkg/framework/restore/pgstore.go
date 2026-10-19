package restore

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaSQL creates the restore job table and adds the columns a table created
// by an earlier version lacks.
//
//go:embed schema.sql
var schemaSQL string

// Migrate creates the restore job table if it does not exist and adds missing
// columns to an existing one. It never drops or changes a column.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply restore schema: %w", err)
	}
	return nil
}

// PGStore is a Store backed by PostgreSQL.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore returns a Store using pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

// Durations are passed as milliseconds and turned into an interval in SQL, so
// the conversion does not depend on how the driver encodes time.Duration.

const insertSQL = `
INSERT INTO restore_jobs (restore_id, recovery_id, claim_token_hash, claim_expires_at, status, plan, repository_id)
VALUES ($1, $2, $3, now() + ($4::bigint * interval '1 millisecond'), $5, $6, $7)
RETURNING claim_expires_at`

// Insert implements Store.
func (s *PGStore) Insert(ctx context.Context, job Job, ttl time.Duration) (time.Time, error) {
	var expiresAt time.Time
	err := s.pool.QueryRow(ctx, insertSQL,
		job.RestoreID, job.RecoveryID, job.ClaimTokenHash, ttl.Milliseconds(),
		string(job.Status), job.Plan, job.RepositoryID,
	).Scan(&expiresAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("insert restore job: %w", err)
	}
	return expiresAt, nil
}

const jobColumns = `restore_id, recovery_id, claim_token_hash, claim_expires_at, status, plan, repository_id,
       claim_request_key, claimed_at, redelivery_expires_at, report_token_hash,
       phase, step, total_steps, current_item, processed_bytes, total_bytes, last_seq, last_progress_at,
       restore_outcome, message, finished_at, created_at, now()`

const getSQL = `SELECT ` + jobColumns + ` FROM restore_jobs WHERE recovery_id = $1`

const lookupSQL = `SELECT ` + jobColumns + ` FROM restore_jobs WHERE restore_id = $1 OR recovery_id = $1`

// Get implements Store.
func (s *PGStore) Get(ctx context.Context, recoveryID string) (Job, time.Time, error) {
	return scanJob(s.pool.QueryRow(ctx, getSQL, recoveryID))
}

// Lookup implements Store.
func (s *PGStore) Lookup(ctx context.Context, id string) (Job, error) {
	job, _, err := scanJob(s.pool.QueryRow(ctx, lookupSQL, id))
	return job, err
}

// scanJob reads a row selected with jobColumns.
func scanJob(row pgx.Row) (Job, time.Time, error) {
	var (
		job              Job
		status           string
		key, phase, item *string
		repositoryID     *string
		outcome, message *string
		now              time.Time
	)
	err := row.Scan(
		&job.RestoreID, &job.RecoveryID, &job.ClaimTokenHash, &job.ClaimExpiresAt, &status,
		&job.Plan, &repositoryID, &key, &job.ClaimedAt, &job.RedeliveryExpiresAt, &job.ReportTokenHash,
		&phase, &job.Progress.Step, &job.Progress.TotalSteps, &item, &job.Progress.ProcessedBytes,
		&job.Progress.TotalBytes, &job.Progress.Seq, &job.LastProgressAt,
		&outcome, &message, &job.FinishedAt, &job.CreatedAt, &now,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, time.Time{}, ErrNotFound
	}
	if err != nil {
		return Job{}, time.Time{}, fmt.Errorf("get restore job: %w", err)
	}
	job.Status = Status(status)
	job.ClaimRequestKey = deref(key)
	job.RepositoryID = deref(repositoryID)
	job.Progress.Phase = deref(phase)
	job.Progress.Item = deref(item)
	job.Outcome = Outcome(deref(outcome))
	job.Message = deref(message)
	return job, now, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// tryClaimSQL claims the job only while it is unclaimed and inside its window.
// Under concurrent claims the row lock lets one update win; the others
// re-evaluate the WHERE clause against the committed row and match nothing.
const tryClaimSQL = `
UPDATE restore_jobs
   SET status = $3, claim_request_key = $2, claimed_at = now(),
       redelivery_expires_at = now() + ($4::bigint * interval '1 millisecond')
 WHERE recovery_id = $1 AND status = $5
   AND claim_request_key IS NULL AND claim_expires_at > now()`

// TryClaim implements Store.
func (s *PGStore) TryClaim(ctx context.Context, recoveryID, requestKey string, redelivery time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx, tryClaimSQL,
		recoveryID, requestKey, string(StatusClaimed), redelivery.Milliseconds(), string(StatusAwaitingAgent),
	)
	if err != nil {
		return false, fmt.Errorf("claim restore job: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

const setReportTokenSQL = `
UPDATE restore_jobs SET report_token_hash = $3
 WHERE recovery_id = $1 AND claim_request_key = $2`

// SetReportToken implements Store.
func (s *PGStore) SetReportToken(ctx context.Context, recoveryID, requestKey string, hash []byte) error {
	tag, err := s.pool.Exec(ctx, setReportTokenSQL, recoveryID, requestKey, hash)
	if err != nil {
		return fmt.Errorf("store report token: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// updateProgressSQL records a report only while the job accepts reports and
// only if it is newer than the last one, so a delayed retry cannot move the
// recorded progress backwards.
const updateProgressSQL = `
UPDATE restore_jobs
   SET status = CASE WHEN status = $9 THEN $10 ELSE status END,
       last_seq = $2, phase = $3, step = $4, total_steps = $5, current_item = NULLIF($6, ''),
       processed_bytes = $7, total_bytes = $8, last_progress_at = now()
 WHERE recovery_id = $1 AND status IN ($9, $10) AND last_seq < $2`

// UpdateProgress implements Store.
func (s *PGStore) UpdateProgress(ctx context.Context, recoveryID string, p Progress) (bool, error) {
	tag, err := s.pool.Exec(ctx, updateProgressSQL,
		recoveryID, p.Seq, p.Phase, p.Step, p.TotalSteps, p.Item, p.ProcessedBytes, p.TotalBytes,
		string(StatusClaimed), string(StatusRestoring),
	)
	if err != nil {
		return false, fmt.Errorf("record restore progress: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// reopenSQL clears the progress along with the status: the next run numbers
// its reports from 1 again, and a last_seq left from this one would make the
// framework ignore them.
const reopenSQL = `
UPDATE restore_jobs
   SET status = $4, phase = NULLIF($2, ''), message = NULLIF($3, ''),
       step = 0, total_steps = 0, current_item = NULL, processed_bytes = 0, total_bytes = 0,
       last_seq = 0, last_progress_at = now()
 WHERE recovery_id = $1 AND status IN ($4, $5)`

// Reopen implements Store.
func (s *PGStore) Reopen(ctx context.Context, recoveryID string, r Result) (bool, error) {
	tag, err := s.pool.Exec(ctx, reopenSQL, recoveryID, r.Phase, r.Message,
		string(StatusClaimed), string(StatusRestoring))
	if err != nil {
		return false, fmt.Errorf("reopen restore job: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

const finishSQL = `
UPDATE restore_jobs
   SET status = $2, restore_outcome = $3, phase = COALESCE(NULLIF($4, ''), phase),
       message = NULLIF($5, ''), finished_at = now()
 WHERE recovery_id = $1 AND status IN ($6, $7)`

// Finish implements Store.
func (s *PGStore) Finish(ctx context.Context, recoveryID string, status Status, r Result) (bool, error) {
	tag, err := s.pool.Exec(ctx, finishSQL,
		recoveryID, string(status), string(r.Outcome), r.Phase, r.Message,
		string(StatusClaimed), string(StatusRestoring),
	)
	if err != nil {
		return false, fmt.Errorf("record restore result: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
