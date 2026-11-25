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

// Flows is what the job queue needs from the HA flows that create HA jobs.
// It is declared here so that this package does not import the HA package.
//
// An HA job belongs to one step of one flow. Its spec names the flow and the
// step; this package does not read it, and leaves judging it to Flows.
type Flows interface {
	// InProgress reports whether systemID takes part in an HA flow that has
	// not ended. Its backups are refused until it has.
	InProgress(ctx context.Context, systemID string) (bool, error)
	// Admit is asked when an HA job is claimed, before the claim is recorded.
	// It returns an error wrapping ErrFlowMoved when the flow is no longer at
	// the job's step, and otherwise what the agent needs to run the job: the
	// replication secret, or nil for a step that needs none. Any other error
	// leaves the job as it was, to be claimed again.
	Admit(ctx context.Context, j Job) (*model.ReplicationAccess, error)
	// Current is asked when an HA job's agent reports that it is starting.
	// It returns an error wrapping ErrFlowMoved when the flow is no longer at
	// the job's step.
	Current(ctx context.Context, j Job) error
}

// TTLs bounds how long a job of each kind waits for its agent.
type TTLs struct {
	// Backup is for backup jobs.
	Backup time.Duration
	// HA is for every HA kind. It is short: an HA job belongs to one step of
	// an HA operation, and one that waits long would act on a state the
	// operator has stopped watching.
	HA time.Duration
}

var (
	// ErrLegacyJob reports a backup job created before jobs named a
	// repository. It can never run: there is no repository to hand its agent.
	ErrLegacyJob = errors.New("job predates repository assignment")
	// errNoFlows reports an HA job on a server running without HA flows. No
	// such server creates one, so finding one is a fault, not a refusal.
	errNoFlows = errors.New("this server runs no HA flows")
	// ErrBackupSetNotFound reports a set the framework has no usable record
	// of: one taken by hand outside the framework, or a wrong ID. FindBackupSet
	// also returns it for a set whose backup did not complete.
	ErrBackupSetNotFound = errors.New("no completed backup job recorded this backup set")
	// ErrBackupSetAmbiguous reports a set ID reported by more than one job.
	ErrBackupSetAmbiguous = errors.New("more than one backup job recorded this backup set")
)

// Service creates jobs and serves the agents carrying them out.
type Service struct {
	store   Store
	repos   Repositories
	flows   Flows
	ttls    TTLs
	claimed time.Duration
	timings Timings
}

// NewService returns a Service over store.
//
// ttls bound how long a job waits for an agent; claimedTimeout bounds how long
// a claimed job may sit before it is returned to the queue. repos resolves the
// repository each backup job writes to, and flows judges HA jobs. flows is nil
// on a server without HA flows: no system is then in one, and an HA job found
// in the queue is refused at claim.
func NewService(store Store, repos Repositories, flows Flows, ttls TTLs, claimedTimeout time.Duration, t Timings) *Service {
	return &Service{store: store, repos: repos, flows: flows, ttls: ttls, claimed: claimedTimeout, timings: t}
}

// Timings returns what agents are told about reporting.
func (s *Service) Timings() Timings { return s.timings }

// TTL returns how long a job of kind k waits for its agent. An HA flow
// enqueues its jobs with it (InsertTx).
func (s *Service) TTL(k Kind) time.Duration {
	if k.HA() {
		return s.ttls.HA
	}
	return s.ttls.Backup
}

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

	// An HA operation may be stopping the service or moving the data to the
	// other node, and a backup taken in the middle would capture neither the
	// old state nor the new one.
	if s.flows != nil {
		busy, err := s.flows.InProgress(ctx, systemID)
		if err != nil {
			return Job{}, fmt.Errorf("check the HA operations of system %s: %w", systemID, err)
		}
		if busy {
			return Job{}, fmt.Errorf("%w: %s", ErrSystemInFlow, systemID)
		}
	}

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
	}, s.TTL(KindBackup))
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
	// Repository is where a backup job writes, with its credentials. It is a
	// secret, decrypted for this response and stored nowhere with the job.
	// Zero for the other kinds.
	Repository model.RepositoryAccess
	// Replication is the secret an HA job needs, decrypted in the same way,
	// or nil when its step needs none.
	Replication *model.ReplicationAccess
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
	// never received anything to run it with. The one exception is an HA job
	// whose flow has moved on, which is ended: it can never run, and left in
	// the queue it would be handed to its agent on every poll.
	repo, replication, err := s.accessFor(ctx, owned)
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

	return Claimed{
		Job: job, Attempt: attempt, LeaseToken: leaseToken, Timings: s.timings,
		Repository: repo, Replication: replication,
	}, nil
}

// accessFor returns what a job's agent needs to run it: the repository for a
// backup, and whatever the HA flow hands out for an HA job.
func (s *Service) accessFor(ctx context.Context, j Job) (model.RepositoryAccess, *model.ReplicationAccess, error) {
	switch {
	case j.Kind == KindBackup:
		repo, err := s.repositoryFor(ctx, j)
		return repo, nil, err
	case j.Kind.HA():
		if s.flows == nil {
			return model.RepositoryAccess{}, nil, fmt.Errorf("HA job %s: %w", j.JobID, errNoFlows)
		}
		replication, err := s.flows.Admit(ctx, j)
		if err != nil {
			return model.RepositoryAccess{}, nil, s.refuseMoved(ctx, j, err)
		}
		return model.RepositoryAccess{}, replication, nil
	default:
		// The schema admits only the kinds above, so this is a kind added to
		// the schema and not here.
		return model.RepositoryAccess{}, nil, fmt.Errorf("job %s has kind %q, which this server does not run", j.JobID, j.Kind)
	}
}

// refuseMoved ends j when err says its HA flow has moved on, and returns err.
// Other errors are returned as they are, leaving the job to be claimed again.
func (s *Service) refuseMoved(ctx context.Context, j Job, err error) error {
	if !errors.Is(err, ErrFlowMoved) {
		return fmt.Errorf("check HA job %s with its flow: %w", j.JobID, err)
	}
	if rejectErr := s.store.Reject(ctx, j.JobID, "Not run: the HA operation it belonged to had moved on"); rejectErr != nil {
		// Still refused. The job stays queued until the next attempt to end
		// it, or until it expires.
		return fmt.Errorf("%w; ending job %s also failed: %w", err, j.JobID, rejectErr)
	}
	return err
}

// repositoryFor returns the repository access a backup job's agent needs.
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
//
// The first report of an HA job is its agent asking to start, and is refused
// when the job's flow has moved on since the claim: the agent does nothing
// until its start is acknowledged, so the job ends without having touched the
// node. Later reports are not checked against the flow. Refusing one would
// stop the agent partway through a step, which is worse than letting the step
// finish and the flow judge its result.
func (s *Service) RecordProgress(ctx context.Context, agent Identity, jobID, attemptID, leaseToken string, p Progress) (Reported, error) {
	owned, err := s.ownedJob(ctx, agent, jobID)
	if err != nil {
		return Reported{}, err
	}
	if owned.Kind.HA() && owned.Status == StatusClaimed {
		if s.flows == nil {
			return Reported{}, fmt.Errorf("HA job %s: %w", jobID, errNoFlows)
		}
		if err := s.flows.Current(ctx, owned); err != nil {
			return Reported{}, s.refuseMoved(ctx, owned, err)
		}
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

// List returns the jobs f selects, newest first.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Job, error) {
	return s.store.List(ctx, f)
}

// ListBackups returns the backup history f selects, newest first. It includes
// backups that failed or have not run yet: what a user asked for and what became
// of it, rather than only the sets that exist.
func (s *Service) ListBackups(ctx context.Context, f ListFilter) ([]BackupRecord, error) {
	return s.store.ListBackups(ctx, f)
}

// GetBackup returns the backup that produced setID.
//
// Unlike FindBackupSet it does not require the backup to have completed: this
// answers what became of a set, not whether it can be restored.
func (s *Service) GetBackup(ctx context.Context, setID string) (BackupRecord, error) {
	found, err := s.store.FindBackups(ctx, setID)
	if err != nil {
		return BackupRecord{}, err
	}
	switch len(found) {
	case 0:
		return BackupRecord{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, setID)
	case 1:
		return found[0], nil
	default:
		return BackupRecord{}, fmt.Errorf("%w: %s (%d jobs)", ErrBackupSetAmbiguous, setID, len(found))
	}
}

// RequestCancel asks for a job to stop. An HA job is refused: it ends with
// its HA operation.
func (s *Service) RequestCancel(ctx context.Context, jobID string) (Job, error) {
	if jobID == "" {
		return Job{}, fmt.Errorf("%w: a job id is required", ErrInvalidRequest)
	}
	// A job's kind never changes, so reading it first leaves no gap.
	j, _, err := s.store.Get(ctx, jobID)
	if err != nil {
		return Job{}, err
	}
	if j.Kind.HA() {
		return Job{}, fmt.Errorf("%w: job %s is a %s job", ErrNotCancellable, jobID, j.Kind)
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

// ownedJob returns the job if it belongs to the calling agent's system, and
// refuses it otherwise.
//
// This is the second half of authorization. The token establishes which agent
// is calling; it says nothing about whose work this is. Without this check any
// enrolled agent could claim, report on, or finish another system's jobs.
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
