package job

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

const (
	uniqueViolation = "23505"
	claimKeyIndex   = "attempts_claim_key"
)

// Migrate creates the job tables if they do not exist.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply job schema: %w", err)
	}
	return nil
}

// PGStore is a Store backed by PostgreSQL.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore returns a Store using pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// Durations are passed as milliseconds and turned into an interval in SQL, so
// the conversion does not depend on how the driver encodes time.Duration.

// jobColumns is the column list every job read shares.
const jobColumns = `job_id, system_id, kind, status, spec, current_attempt_id,
                    cancel_requested, message, requested_by, created_at,
                    expires_at, started_at, finished_at`

const insertSQL = `
INSERT INTO jobs (job_id, system_id, kind, status, spec, requested_by, expires_at)
VALUES ($1, $2, $3, 'queued', $4, $5, now() + ($6::bigint * interval '1 millisecond'))
RETURNING ` + jobColumns

// Insert implements Store.
func (s *PGStore) Insert(ctx context.Context, j NewJob, ttl time.Duration) (Job, error) {
	row := s.pool.QueryRow(ctx, insertSQL,
		j.JobID, j.SystemID, string(j.Kind), []byte(j.Spec), j.RequestedBy, ttl.Milliseconds())

	job, err := scanJob(row)
	if err != nil {
		return Job{}, fmt.Errorf("enqueue job: %w", err)
	}
	return job, nil
}

const nextQueuedSQL = `
SELECT ` + jobColumns + `
  FROM jobs
 WHERE system_id = $1 AND status = 'queued' AND expires_at > now()
 ORDER BY created_at
 LIMIT 1`

// NextQueued implements Store.
func (s *PGStore) NextQueued(ctx context.Context, systemID string) (Job, error) {
	job, err := scanJob(s.pool.QueryRow(ctx, nextQueuedSQL, systemID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrJobNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("read the next queued job: %w", err)
	}
	return job, nil
}

// claimSQL takes the job and creates its attempt in one statement.
//
// The UPDATE only matches a job still queued and unexpired, and the INSERT
// draws from it, so an attempt cannot exist for a job that was not taken and a
// job cannot be taken without an attempt to show for it. A repeated claim key
// violates attempts_claim_key and rolls the whole statement back, which is how
// a retry is recognised without costing the job its queued state.
const claimSQL = `
WITH taken AS (
    UPDATE jobs
       SET status = 'claimed', current_attempt_id = $2
     WHERE job_id = $1 AND status = 'queued' AND expires_at > now()
    RETURNING job_id
)
INSERT INTO attempts (attempt_id, job_id, agent_id, lease_token_hash,
                      claim_request_key, lease_expires_at)
SELECT $2, job_id, $3, $4, $5, now() + ($6::bigint * interval '1 millisecond')
  FROM taken
RETURNING attempt_id`

// Claim implements Store.
func (s *PGStore) Claim(ctx context.Context, jobID string, a NewAttempt, lease time.Duration) (Job, Attempt, error) {
	var attemptID string
	err := s.pool.QueryRow(ctx, claimSQL,
		jobID, a.AttemptID, a.AgentID, a.LeaseTokenHash, a.ClaimRequestKey, lease.Milliseconds(),
	).Scan(&attemptID)

	switch {
	case err == nil:
		return s.jobAndAttempt(ctx, jobID, attemptID)

	case errors.Is(err, pgx.ErrNoRows), isUniqueViolation(err, claimKeyIndex):
		// The job was not claimable as new. Either it is taken by someone else,
		// or this very key already claimed it and the caller is retrying a
		// response it never received.
		return s.reclaim(ctx, jobID, a, lease)

	default:
		return Job{}, Attempt{}, fmt.Errorf("claim job: %w", err)
	}
}

// reissueLeaseSQL hands a retrying caller a lease it can actually use.
//
// Only the hash of a lease token is stored, so the token from the first claim
// cannot be given back. A caller retrying because it lost that response never
// saw it either, so it gets a new one instead, replacing the old hash.
//
// That is only safe while the attempt has made no report. A report proves the
// agent received its token and started work, and a claim arriving after that
// is a stale duplicate: reissuing then would invalidate the token a running
// attempt is using, and it would abort itself mid-backup. last_seq = 0 is what
// keeps this from happening.
const reissueLeaseSQL = `
UPDATE attempts
   SET lease_token_hash = $3,
       lease_expires_at = now() + ($4::bigint * interval '1 millisecond')
 WHERE job_id = $1 AND claim_request_key = $2
   AND last_seq = 0 AND finished_at IS NULL AND superseded = false
RETURNING attempt_id`

// reclaim answers a claim for a job that was not claimable as new.
func (s *PGStore) reclaim(ctx context.Context, jobID string, a NewAttempt, lease time.Duration) (Job, Attempt, error) {
	var attemptID string
	err := s.pool.QueryRow(ctx, reissueLeaseSQL,
		jobID, a.ClaimRequestKey, a.LeaseTokenHash, lease.Milliseconds(),
	).Scan(&attemptID)
	if errors.Is(err, pgx.ErrNoRows) {
		// No attempt of this key that is still waiting to start: the job
		// belongs to another claim, or this one already began reporting.
		return Job{}, Attempt{}, ErrNotClaimable
	}
	if err != nil {
		return Job{}, Attempt{}, fmt.Errorf("reissue a lease: %w", err)
	}
	return s.jobAndAttempt(ctx, jobID, attemptID)
}

// progressSQL applies a report only while the attempt is current, holds a live
// lease and matches the presented token, and extends the lease in the same
// statement.
//
// last_progress_at moves only when the work actually advanced. A report that
// merely proves the agent is alive refreshes the lease and last_seen_at,
// leaving an agent that is up but stuck distinguishable from one that is gone.
const progressSQL = `
UPDATE attempts a
   SET last_seen_at     = now(),
       lease_expires_at = now() + ($5::bigint * interval '1 millisecond'),
       last_seq         = GREATEST(a.last_seq, $4),
       phase            = COALESCE(NULLIF($6, ''), a.phase),
       processed_bytes  = CASE WHEN $4 > a.last_seq THEN $7 ELSE a.processed_bytes END,
       last_progress_at = CASE WHEN $8 THEN now() ELSE a.last_progress_at END
  FROM jobs j
 WHERE a.attempt_id = $2 AND a.job_id = $1
   AND j.job_id = a.job_id AND j.current_attempt_id = a.attempt_id
   AND a.superseded = false AND a.finished_at IS NULL
   AND a.lease_expires_at > now()
   AND a.lease_token_hash = $3
RETURNING a.attempt_id`

// startRunningSQL moves a claimed job to running on its first report.
const startRunningSQL = `
UPDATE jobs SET status = 'running', started_at = COALESCE(started_at, now())
 WHERE job_id = $1 AND status = 'claimed' AND current_attempt_id = $2`

// RecordProgress implements Store.
func (s *PGStore) RecordProgress(ctx context.Context, jobID, attemptID string, tokenHash []byte, p Progress, lease time.Duration) (Job, error) {
	var updated string
	err := s.pool.QueryRow(ctx, progressSQL,
		jobID, attemptID, tokenHash, p.Seq, lease.Milliseconds(),
		p.Phase, p.ProcessedBytes, p.Advanced,
	).Scan(&updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrLeaseInvalid
	}
	if err != nil {
		return Job{}, fmt.Errorf("record progress: %w", err)
	}

	// A first report is what turns a claimed job into a running one: from here
	// the agent may be writing, so the job can no longer be safely requeued.
	if _, err := s.pool.Exec(ctx, startRunningSQL, jobID, attemptID); err != nil {
		return Job{}, fmt.Errorf("mark job running: %w", err)
	}

	job, _, err := s.Get(ctx, jobID)
	return job, err
}

// findBackupSetSQL looks a set up through the attempt that reported it. Every
// attempt keeps its own result, so a set reported by an attempt is found
// however the job was retried afterwards; only an attempt that completed, of
// a job that completed, counts.
const findBackupSetSQL = `
SELECT j.job_id, j.system_id, COALESCE(j.spec->>'repositoryId', '')
  FROM jobs j
  JOIN attempts a ON a.job_id = j.job_id
 WHERE j.kind = 'backup' AND j.status = 'completed'
   AND a.outcome = 'completed' AND a.result->>'backupSetId' = $1
 ORDER BY j.created_at`

// FindCompletedBackupSet implements Store.
func (s *PGStore) FindCompletedBackupSet(ctx context.Context, setID string) ([]BackupSetOrigin, error) {
	rows, err := s.pool.Query(ctx, findBackupSetSQL, setID)
	if err != nil {
		return nil, fmt.Errorf("find backup set: %w", err)
	}
	defer rows.Close()
	var out []BackupSetOrigin
	for rows.Next() {
		var o BackupSetOrigin
		if err := rows.Scan(&o.JobID, &o.SystemID, &o.RepositoryID); err != nil {
			return nil, fmt.Errorf("find backup set: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find backup set: %w", err)
	}
	return out, nil
}

// finishAttemptSQL closes an attempt under the same lease guard as progress.
const finishAttemptSQL = `
UPDATE attempts a
   SET outcome     = $4,
       message     = $5,
       result      = $6,
       finished_at = now(),
       last_seen_at = now()
  FROM jobs j
 WHERE a.attempt_id = $2 AND a.job_id = $1
   AND j.job_id = a.job_id AND j.current_attempt_id = a.attempt_id
   AND a.superseded = false AND a.finished_at IS NULL
   AND a.lease_expires_at > now()
   AND a.lease_token_hash = $3
RETURNING a.attempt_id`

// finishJobSQL moves the job to the status its attempt ended with.
const finishJobSQL = `
UPDATE jobs
   SET status = $2, message = $3, finished_at = now()
 WHERE job_id = $1 AND status IN ('claimed', 'running', 'cancelling')`

// RecordResult implements Store.
func (s *PGStore) RecordResult(ctx context.Context, jobID, attemptID string, tokenHash []byte, r Result) (Job, error) {
	detail := []byte(r.Detail)
	if len(detail) == 0 {
		detail = nil
	}

	var closed string
	err := s.pool.QueryRow(ctx, finishAttemptSQL,
		jobID, attemptID, tokenHash, string(r.Outcome), r.Message, detail).Scan(&closed)

	if errors.Is(err, pgx.ErrNoRows) {
		// Either the lease is no longer valid, or this attempt already
		// finished and the agent is repeating itself. Repeating the same
		// result is fine; a different one is not.
		return s.reconcileRepeatedResult(ctx, jobID, attemptID, tokenHash, r)
	}
	if err != nil {
		return Job{}, fmt.Errorf("record result: %w", err)
	}

	if _, err := s.pool.Exec(ctx, finishJobSQL, jobID, string(jobStatusFor(r.Outcome)), r.Message); err != nil {
		return Job{}, fmt.Errorf("finish job: %w", err)
	}

	job, _, err := s.Get(ctx, jobID)
	return job, err
}

// reconcileRepeatedResult answers a result the attempt could not accept.
func (s *PGStore) reconcileRepeatedResult(ctx context.Context, jobID, attemptID string, tokenHash []byte, r Result) (Job, error) {
	const q = `
SELECT outcome, message, lease_token_hash, finished_at
  FROM attempts WHERE attempt_id = $1 AND job_id = $2`

	var (
		outcome    *string
		message    *string
		storedHash []byte
		finishedAt *time.Time
	)
	err := s.pool.QueryRow(ctx, q, attemptID, jobID).Scan(&outcome, &message, &storedHash, &finishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrLeaseInvalid
	}
	if err != nil {
		return Job{}, fmt.Errorf("read the recorded result: %w", err)
	}

	// An attempt that never finished was refused for some other reason: a bad
	// token, a superseded attempt, an expired lease.
	if finishedAt == nil || string(storedHash) != string(tokenHash) {
		return Job{}, ErrLeaseInvalid
	}
	if outcome == nil || Outcome(*outcome) != r.Outcome || derefString(message) != r.Message {
		return Job{}, ErrResultConflict
	}

	// The same report again. The agent could not know the first one arrived.
	job, _, err := s.Get(ctx, jobID)
	return job, err
}

// jobStatusFor maps an attempt's outcome to the job's terminal status.
func jobStatusFor(o Outcome) Status {
	switch o {
	case OutcomeCompleted:
		return StatusCompleted
	case OutcomeCancelled:
		return StatusCancelled
	default:
		return StatusFailed
	}
}

// Get implements Store.
func (s *PGStore) Get(ctx context.Context, jobID string) (Job, *Attempt, error) {
	const q = `SELECT ` + jobColumns + ` FROM jobs WHERE job_id = $1`

	job, err := scanJob(s.pool.QueryRow(ctx, q, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, nil, ErrJobNotFound
	}
	if err != nil {
		return Job{}, nil, fmt.Errorf("read job: %w", err)
	}
	if job.CurrentAttemptID == "" {
		return job, nil, nil
	}

	const aq = `
SELECT attempt_id, job_id, agent_id, claim_request_key, lease_expires_at,
       last_seen_at, last_progress_at, last_seq, phase, processed_bytes,
       outcome, message, result, superseded, created_at, finished_at
  FROM attempts WHERE attempt_id = $1`
	attempt, err := scanAttempt(s.pool.QueryRow(ctx, aq, job.CurrentAttemptID))
	if err != nil {
		return job, nil, fmt.Errorf("read the current attempt: %w", err)
	}
	return job, &attempt, nil
}

// List implements Store.
func (s *PGStore) List(ctx context.Context, systemID string, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT ` + jobColumns + ` FROM jobs`
	args := []any{limit}
	if systemID != "" {
		q += ` WHERE system_id = $2`
		args = append(args, systemID)
	}
	q += ` ORDER BY created_at DESC LIMIT $1`

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// RequestCancel implements Store.
func (s *PGStore) RequestCancel(ctx context.Context, jobID string) (Job, error) {
	// A queued job has no agent to stop, so it ends here. A job in flight is
	// only marked: the agent has to confirm it stopped, and a cancel request
	// is not a confirmation.
	const q = `
UPDATE jobs
   SET cancel_requested = true,
       status = CASE WHEN status = 'queued' THEN 'cancelled'
                     WHEN status IN ('claimed', 'running') THEN 'cancelling'
                     ELSE status END,
       finished_at = CASE WHEN status = 'queued' THEN now() ELSE finished_at END,
       message = CASE WHEN status = 'queued' THEN 'Cancelled before an agent took it'
                      ELSE message END
 WHERE job_id = $1
RETURNING ` + jobColumns

	job, err := scanJob(s.pool.QueryRow(ctx, q, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrJobNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("request cancel: %w", err)
	}
	return job, nil
}

// scanner abstracts pgx.Row and pgx.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanJob(sc scanner) (Job, error) {
	var (
		j           Job
		kind        string
		status      string
		spec        []byte
		attemptID   *string
		message     *string
		requestedBy *string
	)
	if err := sc.Scan(&j.JobID, &j.SystemID, &kind, &status, &spec, &attemptID,
		&j.CancelRequested, &message, &requestedBy, &j.CreatedAt, &j.ExpiresAt,
		&j.StartedAt, &j.FinishedAt); err != nil {
		return Job{}, err
	}
	j.Kind = Kind(kind)
	j.Status = Status(status)
	j.Spec = json.RawMessage(spec)
	j.CurrentAttemptID = derefString(attemptID)
	j.Message = derefString(message)
	j.RequestedBy = derefString(requestedBy)
	return j, nil
}

func scanAttempt(sc scanner) (Attempt, error) {
	var (
		a       Attempt
		phase   *string
		outcome *string
		message *string
		result  []byte
	)
	if err := sc.Scan(&a.AttemptID, &a.JobID, &a.AgentID, &a.ClaimRequestKey,
		&a.LeaseExpiresAt, &a.LastSeenAt, &a.LastProgressAt, &a.LastSeq,
		&phase, &a.ProcessedBytes, &outcome, &message, &result,
		&a.Superseded, &a.CreatedAt, &a.FinishedAt); err != nil {
		return Attempt{}, err
	}
	a.Phase = derefString(phase)
	a.Message = derefString(message)
	if outcome != nil {
		a.Outcome = Outcome(*outcome)
	}
	a.Result = json.RawMessage(result)
	return a, nil
}

func (s *PGStore) jobAndAttempt(ctx context.Context, jobID, attemptID string) (Job, Attempt, error) {
	job, attempt, err := s.Get(ctx, jobID)
	if err != nil {
		return Job{}, Attempt{}, err
	}
	if attempt == nil || attempt.AttemptID != attemptID {
		return Job{}, Attempt{}, fmt.Errorf("claimed attempt %s is not current", attemptID)
	}
	return job, *attempt, nil
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == uniqueViolation &&
		pgErr.ConstraintName == constraint
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
