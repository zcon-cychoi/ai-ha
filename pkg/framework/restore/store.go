package restore

import (
	"context"
	"encoding/json"
	"time"
)

// Status is the lifecycle state of a restore job.
type Status string

const (
	// StatusAwaitingAgent means the job waits for its rescue agent to claim it.
	StatusAwaitingAgent Status = "awaiting_agent"
	// StatusClaimed means an agent claimed the job. It does not mean the
	// response reached the agent or that restoring has started; "restoring" is
	// set once the agent reports it.
	StatusClaimed Status = "claimed"
	// StatusRestoring means the agent reported that the restore is under way.
	StatusRestoring Status = "restoring"
	// StatusAwaitingBoot means the disk was restored and the restored system's
	// boot has yet to be confirmed.
	StatusAwaitingBoot Status = "awaiting_boot"
	// StatusFailed means the agent reported that the restore failed.
	StatusFailed Status = "failed"
)

// active reports whether a job in status s still accepts reports from its agent.
func (s Status) active() bool {
	return s == StatusClaimed || s == StatusRestoring
}

// Outcome is how the agent reported the restore ended. It is kept apart from
// Status because a restored disk is not yet a system that boots.
type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
)

// Progress is a progress report from the agent.
type Progress struct {
	Seq            int64
	Phase          string
	Step           int
	TotalSteps     int
	Item           string
	ProcessedBytes int64
	TotalBytes     int64
}

// Result is the agent's report of how the restore ended.
type Result struct {
	Outcome Outcome
	Phase   string
	Message string
}

// Job is a restore job as stored by the framework.
type Job struct {
	RestoreID      string
	RecoveryID     string
	ClaimTokenHash []byte
	ClaimExpiresAt time.Time
	Status         Status
	// Plan and RepositoryID never change once the job is claimed, so a
	// redelivered response carries the same content as the first one.
	Plan json.RawMessage
	// RepositoryID is the registered repository holding the set. Its
	// credentials are not stored here; they are decrypted for each claim.
	// Empty for a job created before restore jobs named one.
	RepositoryID string
	// ClaimRequestKey is empty until the job is claimed.
	ClaimRequestKey     string
	ClaimedAt           *time.Time
	RedeliveryExpiresAt *time.Time

	// ReportTokenHash authenticates the agent's reports. Empty until claimed.
	ReportTokenHash []byte
	// Progress is the latest progress report; its Seq is zero before the first.
	Progress       Progress
	LastProgressAt *time.Time
	// Outcome, Message and FinishedAt are set once the agent reports the end.
	Outcome    Outcome
	Message    string
	FinishedAt *time.Time
	CreatedAt  time.Time
}

// Store persists restore jobs. All time comparisons use the store's clock.
type Store interface {
	// Insert stores a new job awaiting its agent, whose claim window closes ttl
	// after the store's current time. It returns that closing time.
	Insert(ctx context.Context, job Job, ttl time.Duration) (time.Time, error)
	// Get returns the job for recoveryID together with the store's current
	// time. It returns ErrNotFound when there is no such job.
	Get(ctx context.Context, recoveryID string) (Job, time.Time, error)
	// TryClaim atomically marks the job claimed by requestKey if it is still
	// awaiting its agent, unclaimed and inside its claim window, allowing the
	// same key to fetch the response again for redelivery. It reports whether
	// this call made the claim.
	TryClaim(ctx context.Context, recoveryID, requestKey string, redelivery time.Duration) (bool, error)
	// SetReportToken stores the hash of a newly issued report token for the job
	// claimed by requestKey, replacing any earlier one. It returns ErrNotFound
	// when no job is claimed by that key.
	SetReportToken(ctx context.Context, recoveryID, requestKey string, hash []byte) error
	// UpdateProgress records p if the job still accepts reports and p is newer
	// than the last report, moving a claimed job to restoring. It reports
	// whether p was recorded.
	UpdateProgress(ctx context.Context, recoveryID string, p Progress) (bool, error)
	// Finish records r and moves the job to status if it still accepts
	// reports. It reports whether this call ended the job.
	Finish(ctx context.Context, recoveryID string, status Status, r Result) (bool, error)
	// Reopen records a failure that changed nothing on disk and returns the
	// job to claimed, with its progress cleared, so that the agent can run the
	// restore again. It applies only while the job accepts reports and
	// reports whether it did.
	Reopen(ctx context.Context, recoveryID string, r Result) (bool, error)
	// Lookup returns the job whose restore ID or recovery ID is id, or
	// ErrNotFound. Both are random 16-digit hex values, so one cannot be
	// mistaken for the other.
	Lookup(ctx context.Context, id string) (Job, error)
}
