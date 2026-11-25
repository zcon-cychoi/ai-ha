// Package job runs the framework's side of the job queue: a user or an HA flow
// asks for work, an agent waits for it, claims it, reports on it and reports
// how it ended.
//
// The shape follows one rule that is easy to get wrong. A lease proves which
// attempt's reports count; it does not stop the previous attempt from still
// reading disks and writing to the repository. Token checks prevent state
// pollution and nothing else. Until an agent can be made to stop on demand, a
// lease that expires while work was running therefore fails the job rather
// than handing it to someone else.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
)

// Kind is what a job asks for. Restores have their own path and their own
// table, because a rescue agent is not an enrolled system.
type Kind string

const (
	// KindBackup is a backup of an enrolled system.
	KindBackup Kind = model.JobKindBackup

	// The HA kinds. An HA flow creates them, one per step and node, and the
	// queue checks with the flow before an agent may run one (Flows).
	KindReplicationSetup    Kind = model.JobKindReplicationSetup
	KindReplicationTeardown Kind = model.JobKindReplicationTeardown
	KindHADemote            Kind = model.JobKindHADemote
	KindHAPromote           Kind = model.JobKindHAPromote
	KindHARejoin            Kind = model.JobKindHARejoin
)

// HA reports whether k is one of the kinds an HA flow creates.
func (k Kind) HA() bool {
	switch k {
	case KindReplicationSetup, KindReplicationTeardown, KindHADemote, KindHAPromote, KindHARejoin:
		return true
	}
	return false
}

// Status is where a job is in its lifecycle.
type Status string

const (
	// StatusQueued means the job is waiting for its agent.
	StatusQueued Status = "queued"
	// StatusClaimed means an agent took the job but has not reported starting.
	// Nothing has been written yet, so this state is safe to undo.
	StatusClaimed Status = "claimed"
	// StatusRunning means the agent reported that it began. From here the
	// agent may be writing to the repository.
	StatusRunning Status = "running"
	// StatusCompleted, StatusFailed and StatusCancelled are terminal.
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
	// StatusCancelling means a cancel was asked for and the agent has not yet
	// confirmed it stopped. A cancel request alone is not a stop.
	StatusCancelling Status = "cancelling"
)

// Terminal reports whether a status ends the job.
func (s Status) Terminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCancelled
}

// Known reports whether s is a status a job can have.
func (s Status) Known() bool {
	switch s {
	case StatusQueued, StatusClaimed, StatusRunning, StatusCancelling,
		StatusCompleted, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// Outcome is how one attempt ended.
type Outcome string

const (
	OutcomeCompleted Outcome = "completed"
	OutcomeFailed    Outcome = "failed"
	OutcomeCancelled Outcome = "cancelled"
	// OutcomeUnknown means the agent stopped reporting. The work may have
	// finished, may still be running; the framework cannot tell. It is not the
	// same as failure and is not reported as one.
	OutcomeUnknown Outcome = "unknown"
)

var (
	// ErrJobNotFound reports that no job has the given id.
	ErrJobNotFound = errors.New("job not found")
	// ErrNotClaimable reports a job that cannot be claimed: it is gone from
	// the queue, already claimed by another attempt, or no longer queued.
	ErrNotClaimable = errors.New("job is not claimable")
	// ErrLeaseInvalid reports a report quoting an attempt that is not current,
	// or a lease that has expired. The report is discarded.
	ErrLeaseInvalid = errors.New("lease is not valid for this attempt")
	// ErrResultConflict reports a second, different result for one attempt.
	ErrResultConflict = errors.New("a different result was already recorded")
	// ErrNotOwned reports that the job belongs to another agent's system.
	// Holding a valid token is not authority over someone else's work.
	ErrNotOwned = errors.New("job does not belong to this agent")
	// ErrInvalidRequest reports a job that cannot be accepted as described.
	ErrInvalidRequest = errors.New("invalid job request")
	// ErrFlowMoved reports an HA job whose flow is no longer at the step the
	// job was created for: the flow ended, failed or went on without it. The
	// job is ended when this is found, before its agent starts anything.
	ErrFlowMoved = errors.New("the HA operation this job belongs to has moved on")
	// ErrSystemInFlow reports a backup asked for while the system takes part
	// in an HA operation, which may be stopping its service or changing which
	// node holds the data.
	ErrSystemInFlow = errors.New("the system is taking part in an HA operation")
	// ErrNotCancellable reports a cancel asked for an HA job. It ends with its
	// HA operation: stopping one step alone would leave the operation waiting
	// on a job that never finishes.
	ErrNotCancellable = errors.New("an HA job cannot be cancelled on its own")
)

// Job is a unit of work as the framework holds it.
type Job struct {
	JobID    string
	SystemID string
	Kind     Kind
	Status   Status
	// Spec is what the agent is asked to do, passed through unread.
	Spec             json.RawMessage
	CurrentAttemptID string
	CancelRequested  bool
	Message          string
	RequestedBy      string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	StartedAt        *time.Time
	FinishedAt       *time.Time
}

// Attempt is one execution of a job.
type Attempt struct {
	AttemptID       string
	JobID           string
	AgentID         string
	ClaimRequestKey string
	LeaseExpiresAt  time.Time
	LastSeenAt      time.Time
	LastProgressAt  *time.Time
	LastSeq         int64
	Phase           string
	ProcessedBytes  int64
	Outcome         Outcome
	Message         string
	Result          json.RawMessage
	Superseded      bool
	CreatedAt       time.Time
	FinishedAt      *time.Time
}

// NewJob describes a job to enqueue.
type NewJob struct {
	JobID       string
	SystemID    string
	Kind        Kind
	Spec        json.RawMessage
	RequestedBy string
}

// NewAttempt describes the attempt a successful claim creates.
type NewAttempt struct {
	AttemptID       string
	AgentID         string
	LeaseTokenHash  []byte
	ClaimRequestKey string
}

// Progress is one report from a running attempt.
type Progress struct {
	Seq            int64
	Phase          string
	ProcessedBytes int64
	// Advanced says whether the work itself moved since the last report. A
	// report that only proves the agent is alive does not touch
	// last_progress_at, which is what separates "stuck" from "gone".
	Advanced bool
}

// Result is the agent's final report for an attempt.
type Result struct {
	Outcome Outcome
	Message string
	// Detail is what the agent produced, recorded as reported.
	Detail json.RawMessage
}

// BackupSetOrigin is the backup job that produced a backup set: the system it
// captured and the repository it wrote to.
type BackupSetOrigin struct {
	JobID    string
	SystemID string
	// RepositoryID is empty for a job created before jobs named a repository.
	RepositoryID string
}

// ListFilter narrows a list of jobs. A zero field does not filter.
type ListFilter struct {
	SystemID string
	Kind     Kind
	Status   Status
	// From and To bound the creation time, From inclusive and To exclusive.
	From time.Time
	To   time.Time
	// Limit defaults to defaultListLimit. Offset skips that many jobs.
	Limit  int
	Offset int
}

// BackupRecord is a backup job with the attempt that carried it out. Attempt
// is nil until an agent claims the job.
type BackupRecord struct {
	Job     Job
	Attempt *Attempt
}

// SweepCounts reports what one sweep changed.
type SweepCounts struct {
	// QueuedExpired are jobs no agent ever came for.
	QueuedExpired int
	// ClaimedReturned are jobs whose agent took them but never started. Safe
	// to requeue: nothing was written.
	ClaimedReturned int
	// RunningAbandoned are jobs whose agent stopped reporting while it may
	// have been writing. They are failed, not requeued.
	RunningAbandoned int
	// CancelUnconfirmed are cancels the agent never confirmed.
	CancelUnconfirmed int
}

// Total reports whether the sweep changed anything.
func (c SweepCounts) Total() int {
	return c.QueuedExpired + c.ClaimedReturned + c.RunningAbandoned + c.CancelUnconfirmed
}

// Store persists jobs and attempts. All time comparisons use the store's
// clock, so that an agent's clock cannot extend its own lease.
type Store interface {
	// Insert enqueues a job that expires ttl after the store's current time.
	Insert(ctx context.Context, j NewJob, ttl time.Duration) (Job, error)

	// NextQueued returns the oldest queued job for systemID, or ErrJobNotFound
	// when there is none. It changes nothing: a long-poll response that never
	// arrives must not leave a job marked as taken.
	NextQueued(ctx context.Context, systemID string) (Job, error)

	// Claim atomically takes a queued job and creates its attempt, giving the
	// attempt a lease of the given duration.
	//
	// A retry carrying the same claim request key returns the attempt it
	// already created, so that a lost response does not cost the job. Any
	// other caller finding the job claimed is refused.
	Claim(ctx context.Context, jobID string, a NewAttempt, lease time.Duration) (Job, Attempt, error)

	// RecordProgress applies a report from the current attempt and extends its
	// lease. A report from a superseded attempt, or one whose lease has
	// expired, is refused with ErrLeaseInvalid. A seq not newer than the last
	// is ignored, and the lease is still extended: the agent is alive.
	RecordProgress(ctx context.Context, jobID, attemptID string, tokenHash []byte, p Progress, lease time.Duration) (Job, error)

	// RecordResult finishes an attempt and its job.
	//
	// Repeating the same result is accepted, since the agent cannot know its
	// report arrived. A different result for an already finished attempt is
	// refused with ErrResultConflict.
	RecordResult(ctx context.Context, jobID, attemptID string, tokenHash []byte, r Result) (Job, error)

	// Get returns a job with its current attempt, if any.
	Get(ctx context.Context, jobID string) (Job, *Attempt, error)

	// FindCompletedBackupSet returns every completed backup job whose
	// completed attempt reported setID. Normally there is one; the caller
	// decides what more or none means.
	FindCompletedBackupSet(ctx context.Context, setID string) ([]BackupSetOrigin, error)

	// List returns the jobs f selects, newest first.
	List(ctx context.Context, f ListFilter) ([]Job, error)

	// ListBackups returns the backup jobs f selects, newest first, each with
	// its current attempt. f.Kind is ignored.
	ListBackups(ctx context.Context, f ListFilter) ([]BackupRecord, error)

	// FindBackups returns every backup job with an attempt that reported setID,
	// whatever its outcome: a set that failed partway is still reported. An
	// attempt's result is what holds a set ID, so each record carries that
	// attempt. Normally there is one; the caller decides what more or none means.
	FindBackups(ctx context.Context, setID string) ([]BackupRecord, error)

	// RequestCancel marks a job for cancellation. A queued job is cancelled
	// outright; a running one is only marked, because stopping it requires the
	// agent to act.
	RequestCancel(ctx context.Context, jobID string) (Job, error)

	// Reject fails a job that has not started, with message as the reason,
	// and closes the attempt that claimed it, if any, so that its agent's next
	// report is refused. A job that has started or ended is left as it is.
	Reject(ctx context.Context, jobID, message string) error

	// Sweep applies the timeouts that move jobs nobody is reporting on.
	Sweep(ctx context.Context, claimedTimeout time.Duration) (SweepCounts, error)
}
