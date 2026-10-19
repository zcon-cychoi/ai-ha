package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	backupmodel "github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/token"
)

// Timings the framework hands to an agent when it claims a job.
//
// They come down in the claim response rather than being compiled into the
// agent, so that they can be adjusted without redeploying a fleet.
type Timings struct {
	// ProgressInterval is how often the agent reports, whether or not the work
	// has advanced. A report is what renews the lease.
	ProgressInterval time.Duration
	// LeaseTTL is how long a report buys. It must comfortably exceed
	// ProgressInterval, or an agent that is working correctly loses its lease
	// to one slow round trip.
	LeaseTTL time.Duration
}

// claimKeyPattern is the agent-generated key that scopes claim idempotency to
// a single request, in the same shape as the rescue agent's request key.
var claimKeyPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Repositories is what the job queue needs from the repository registry:
// resolving the repository a job is created for, and the access an agent
// receives when it claims the job.
type Repositories interface {
	// Get returns the repository whose ID or name is idOrName, without
	// credentials.
	Get(ctx context.Context, idOrName string) (model.RepositoryInfo, error)
	// Access returns the repository with ID id and its credentials.
	Access(ctx context.Context, id string) (model.RepositoryAccess, error)
}

var (
	// ErrLegacyJob reports a backup job created before jobs named a
	// repository. It can never run: there is no repository to hand its agent.
	ErrLegacyJob = errors.New("job predates repository assignment")
	// ErrBackupSetNotFound reports a set no completed backup job reported:
	// one taken by hand outside the framework, one that failed, or a wrong ID.
	ErrBackupSetNotFound = errors.New("no completed backup job recorded this backup set")
	// ErrBackupSetAmbiguous reports a set ID reported by more than one job.
	ErrBackupSetAmbiguous = errors.New("more than one backup job recorded this backup set")
)

// Service creates jobs and serves the agents carrying them out.
type Service struct {
	store   Store
	repos   Repositories
	jobTTL  time.Duration
	claimed time.Duration
	timings Timings
}

// NewService returns a Service over store.
//
// jobTTL bounds how long a job waits for an agent; claimedTimeout bounds how
// long a claimed job may sit before it is returned to the queue. repos
// resolves the repository each backup job writes to.
func NewService(store Store, repos Repositories, jobTTL, claimedTimeout time.Duration, t Timings) *Service {
	return &Service{store: store, repos: repos, jobTTL: jobTTL, claimed: claimedTimeout, timings: t}
}

// Timings returns what agents are told about reporting.
func (s *Service) Timings() Timings { return s.timings }

// BackupSpec is what a backup job asks the agent to do. It is the wire
// contract shared with the agent, so it is defined in the model package.
type BackupSpec = model.BackupJobSpec

// CreateBackup enqueues a backup job for a system.
//
// It checks that the spec is well formed, not that it is right: this package
// knows nothing about what partitions a system has. Deciding that is the
// inventory's job, and every caller must go through it first —
// inventory.Service.BackupDevices to choose the partitions, or
// ValidateDevices when an operator named them. Skipping that step produces a
// job that looks valid and fails, or captures the wrong thing.
//
// The check is left to the caller rather than done here so that the queue
// keeps working when no inventory has been reported yet. That moves the
// dependency into a contract instead of removing it, which is why it is
// written down here and in protocol.md rather than only implied.
func (s *Service) CreateBackup(ctx context.Context, systemID string, spec BackupSpec, requestedBy string) (Job, error) {
	if systemID == "" {
		return Job{}, fmt.Errorf("%w: a system id is required", ErrInvalidRequest)
	}
	if len(spec.Devices) == 0 {
		return Job{}, fmt.Errorf("%w: at least one device is required", ErrInvalidRequest)
	}
	for _, d := range spec.Devices {
		if d == "" {
			return Job{}, fmt.Errorf("%w: a device cannot be empty", ErrInvalidRequest)
		}
	}
	if spec.RepositoryID == "" {
		return Job{}, fmt.Errorf("%w: a repository is required", ErrInvalidRequest)
	}
	// Stored by ID: a name could be given to another repository by the time
	// the job runs, and the restore of the set looks the repository up by
	// what the job recorded.
	repo, err := s.repos.Get(ctx, spec.RepositoryID)
	if err != nil {
		return Job{}, fmt.Errorf("%w: repository %q: %w", ErrInvalidRequest, spec.RepositoryID, err)
	}
	spec.RepositoryID = repo.ID

	encoded, err := json.Marshal(spec)
	if err != nil {
		return Job{}, fmt.Errorf("encode the job spec: %w", err)
	}
	return s.store.Insert(ctx, NewJob{
		JobID:       backupmodel.NewID(),
		SystemID:    systemID,
		Kind:        KindBackup,
		Spec:        encoded,
		RequestedBy: requestedBy,
	}, s.jobTTL)
}

// Waiting returns the job waiting for a system, or ErrJobNotFound.
//
// It changes nothing. Handing a job out in a long-poll response is not the
// same as an agent taking it: the response may never arrive, and a job marked
// as taken on the strength of a message nobody received would never run.
func (s *Service) Waiting(ctx context.Context, systemID string) (Job, error) {
	return s.store.NextQueued(ctx, systemID)
}

// Claimed is what a successful claim hands back.
type Claimed struct {
	Job     Job
	Attempt Attempt
	// LeaseToken proves this attempt is the current one. It is returned only
	// here: the store keeps its hash.
	LeaseToken string
	Timings    Timings
	// Repository is where the job writes, with its credentials. It is a
	// secret, decrypted for this response and stored nowhere with the job.
	Repository model.RepositoryAccess
}

// Claim takes a job on behalf of an agent.
//
// The agent must call this and see it succeed before it starts work. Receiving
// a job from the long poll confers no authority: two agents can be handed the
// same job, and only the claim decides between them.
func (s *Service) Claim(ctx context.Context, agent Identity, jobID, claimRequestKey string) (Claimed, error) {
	if !claimKeyPattern.MatchString(claimRequestKey) {
		return Claimed{}, fmt.Errorf("%w: the claim request key must be 32 hexadecimal digits", ErrInvalidRequest)
	}

	// Authentication says who is calling; it does not say the job is theirs.
	// Checked before the repository credentials are decrypted, so they are
	// never opened for a caller who could not receive them.
	owned, err := s.ownedJob(ctx, agent, jobID)
	if err != nil {
		return Claimed{}, err
	}

	// Resolved before the claim is recorded. A failure here changes nothing:
	// no attempt is created and an existing one is left as it was, so the
	// agent retries the same claim once the repository can be reached. Done
	// the other way round, a claim could be recorded for a job whose agent
	// never received anything to run it with.
	repo, err := s.repositoryFor(ctx, owned)
	if err != nil {
		return Claimed{}, err
	}

	leaseToken, err := token.New()
	if err != nil {
		return Claimed{}, err
	}

	job, attempt, err := s.store.Claim(ctx, jobID, NewAttempt{
		AttemptID:       backupmodel.NewID(),
		AgentID:         agent.AgentID,
		LeaseTokenHash:  token.Hash(leaseToken),
		ClaimRequestKey: claimRequestKey,
	}, s.timings.LeaseTTL)
	if err != nil {
		return Claimed{}, err
	}

	// A retry that found its own earlier attempt gets that attempt back, but
	// its lease token was only ever returned once and cannot be recovered.
	if attempt.AgentID != agent.AgentID {
		return Claimed{}, ErrNotClaimable
	}

	return Claimed{Job: job, Attempt: attempt, LeaseToken: leaseToken, Timings: s.timings, Repository: repo}, nil
}

// repositoryFor returns the repository access a job's agent needs.
func (s *Service) repositoryFor(ctx context.Context, j Job) (model.RepositoryAccess, error) {
	var spec BackupSpec
	if err := json.Unmarshal(j.Spec, &spec); err != nil {
		return model.RepositoryAccess{}, fmt.Errorf("decode the spec of job %s: %w", j.JobID, err)
	}
	if spec.RepositoryID == "" {
		return model.RepositoryAccess{}, ErrLegacyJob
	}
	access, err := s.repos.Access(ctx, spec.RepositoryID)
	if err != nil {
		return model.RepositoryAccess{}, fmt.Errorf("repository %s of job %s: %w", spec.RepositoryID, j.JobID, err)
	}
	return access, nil
}

// Reported is what a progress report gets back.
type Reported struct {
	// CancelRequested tells the agent to stop. There is no inbound path to an
	// agent, so a cancel can only travel on a reply the agent asked for.
	CancelRequested bool
	Timings         Timings
}

// RecordProgress applies a report and renews the attempt's lease.
func (s *Service) RecordProgress(ctx context.Context, agent Identity, jobID, attemptID, leaseToken string, p Progress) (Reported, error) {
	if err := s.assertOwned(ctx, agent, jobID); err != nil {
		return Reported{}, err
	}

	job, err := s.store.RecordProgress(ctx, jobID, attemptID, token.Hash(leaseToken), p, s.timings.LeaseTTL)
	if err != nil {
		return Reported{}, err
	}
	return Reported{CancelRequested: job.CancelRequested, Timings: s.timings}, nil
}

// RecordResult finishes an attempt and the job behind it.
func (s *Service) RecordResult(ctx context.Context, agent Identity, jobID, attemptID, leaseToken string, r Result) (Job, error) {
	owned, err := s.ownedJob(ctx, agent, jobID)
	if err != nil {
		return Job{}, err
	}
	switch r.Outcome {
	case OutcomeCompleted, OutcomeFailed, OutcomeCancelled:
	default:
		return Job{}, fmt.Errorf("%w: the outcome must be completed, failed or cancelled", ErrInvalidRequest)
	}
	// A completed backup is found again by its set ID when it is restored. A
	// completion without one would be a backup nobody can name.
	if owned.Kind == KindBackup && r.Outcome == OutcomeCompleted {
		var detail struct {
			BackupSetID string `json:"backupSetId"`
		}
		if err := json.Unmarshal(r.Detail, &detail); err != nil || detail.BackupSetID == "" {
			return Job{}, fmt.Errorf("%w: a completed backup must report its backupSetId", ErrInvalidRequest)
		}
	}
	return s.store.RecordResult(ctx, jobID, attemptID, token.Hash(leaseToken), r)
}

// FindBackupSet returns the backup job that produced setID, which tells which
// system it captured and which repository holds it.
//
// It answers from the framework's records: "completed" means the agent
// reported the set captured. Whether the files are still in the repository is
// not known here; the rescue agent finds out when it reads the set's manifest.
func (s *Service) FindBackupSet(ctx context.Context, setID string) (BackupSetOrigin, error) {
	if setID == "" {
		return BackupSetOrigin{}, fmt.Errorf("%w: a backup set id is required", ErrInvalidRequest)
	}
	found, err := s.store.FindCompletedBackupSet(ctx, setID)
	if err != nil {
		return BackupSetOrigin{}, err
	}
	switch {
	case len(found) == 0:
		return BackupSetOrigin{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, setID)
	case len(found) > 1:
		// Set IDs are random, so this is a fault somewhere; picking one would
		// restore whichever the query happened to return first.
		return BackupSetOrigin{}, fmt.Errorf("%w: %s (%d jobs)", ErrBackupSetAmbiguous, setID, len(found))
	case found[0].RepositoryID == "":
		return BackupSetOrigin{}, fmt.Errorf("%w: backup set %s was written by job %s, which names no repository",
			ErrLegacyJob, setID, found[0].JobID)
	}
	return found[0], nil
}

// Get returns a job and its current attempt.
func (s *Service) Get(ctx context.Context, jobID string) (Job, *Attempt, error) {
	return s.store.Get(ctx, jobID)
}

// List returns jobs, newest first.
func (s *Service) List(ctx context.Context, systemID string, limit int) ([]Job, error) {
	return s.store.List(ctx, systemID, limit)
}

// RequestCancel asks for a job to stop.
func (s *Service) RequestCancel(ctx context.Context, jobID string) (Job, error) {
	if jobID == "" {
		return Job{}, fmt.Errorf("%w: a job id is required", ErrInvalidRequest)
	}
	return s.store.RequestCancel(ctx, jobID)
}

// Sweep applies the timeouts. It is called periodically by the server.
func (s *Service) Sweep(ctx context.Context) (SweepCounts, error) {
	return s.store.Sweep(ctx, s.claimed)
}

// Identity is the authenticated agent making a request.
type Identity struct {
	AgentID  string
	SystemID string
}

// assertOwned refuses a job that belongs to another agent's system.
//
// This is the second half of authorization. The token establishes which agent
// is calling; it says nothing about whose work this is. Without this check any
// enrolled agent could claim, report on, or finish another system's jobs.
func (s *Service) assertOwned(ctx context.Context, agent Identity, jobID string) error {
	_, err := s.ownedJob(ctx, agent, jobID)
	return err
}

// ownedJob returns the job if it belongs to the calling agent's system.
func (s *Service) ownedJob(ctx context.Context, agent Identity, jobID string) (Job, error) {
	job, _, err := s.store.Get(ctx, jobID)
	if err != nil {
		return Job{}, err
	}
	if job.SystemID != agent.SystemID {
		return Job{}, ErrNotOwned
	}
	return job, nil
}
