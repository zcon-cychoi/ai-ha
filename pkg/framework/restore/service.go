// Package restore manages restore jobs on the framework side: creating a job
// for a rescue instance and letting that instance's agent claim it to receive
// the restore plan and repository access.
//
// A job is created from a backup set ID alone. The backup job that produced
// the set says which system it captured and which registered repository holds
// it; the repository's credentials are decrypted only for the claim response
// and never stored with the restore job.
package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	backupmodel "github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/cloud-barista/ai-ha/pkg/framework/token"
)

var (
	// ErrNotFound reports that no restore job has the given recovery ID.
	ErrNotFound = errors.New("recovery not found")
	// ErrInvalidToken reports that the claim token does not match.
	ErrInvalidToken = errors.New("claim token rejected")
	// ErrClaimedByOther reports that a different request key already claimed the job.
	ErrClaimedByOther = errors.New("recovery already claimed by another request")
	// ErrClaimExpired reports that the job was never claimed and its claim window closed.
	ErrClaimExpired = errors.New("claim window expired")
	// ErrRedeliveryExpired reports that the claim's redelivery window closed.
	ErrRedeliveryExpired = errors.New("claim response no longer available")
	// ErrInvalidJob reports a restore job definition that cannot be registered.
	ErrInvalidJob = errors.New("invalid restore job")
	// ErrLegacyJob reports a restore job created before restore jobs named a
	// registered repository. It cannot be claimed: create it again.
	ErrLegacyJob = errors.New("restore job predates repository registration")
	// ErrReportTokenRejected reports a progress or result report whose token
	// is not the one last issued for the job.
	ErrReportTokenRejected = errors.New("report token rejected")
	// ErrRestoreFinished reports a progress report for a job that has ended.
	ErrRestoreFinished = errors.New("restore already finished")
	// ErrResultConflict reports a result that differs from the one recorded.
	ErrResultConflict = errors.New("a different result was already recorded")
)

// Service creates and claims restore jobs.
type Service struct {
	store      Store
	sets       BackupSets
	repos      RepositoryAccess
	claimTTL   time.Duration
	redelivery time.Duration
}

// BackupSets finds the backup job that produced a set.
// *job.Service implements it.
type BackupSets interface {
	FindBackupSet(ctx context.Context, setID string) (job.BackupSetOrigin, error)
}

// RepositoryAccess opens a registered repository's credentials.
// *repository.Service implements it.
type RepositoryAccess interface {
	Access(ctx context.Context, id string) (model.RepositoryAccess, error)
}

// NewService returns a Service backed by store. claimTTL bounds how long a new
// job accepts its first claim; redelivery bounds how long after the claim the
// same request key may fetch the response again.
func NewService(store Store, sets BackupSets, repos RepositoryAccess, claimTTL, redelivery time.Duration) *Service {
	return &Service{store: store, sets: sets, repos: repos, claimTTL: claimTTL, redelivery: redelivery}
}

// NewJob is the definition of a restore job to register.
type NewJob struct {
	// BackupSetID is the backup set to restore.
	BackupSetID string
}

// Created describes a registered job. ClaimToken is returned only here: the
// store keeps just its hash.
type Created struct {
	RestoreID      string
	RecoveryID     string
	ClaimToken     string
	ClaimExpiresAt time.Time
}

// Create registers a restore job awaiting its agent. The set must be one the
// framework recorded as completed; its system and repository come from there.
func (s *Service) Create(ctx context.Context, def NewJob) (Created, error) {
	origin, err := s.sets.FindBackupSet(ctx, def.BackupSetID)
	if err != nil {
		return Created{}, fmt.Errorf("%w: %w", ErrInvalidJob, err)
	}
	plan, err := json.Marshal(model.RestorePlan{SourceSystemID: origin.SystemID, SourceBackupSetID: def.BackupSetID})
	if err != nil {
		return Created{}, fmt.Errorf("encode restore plan: %w", err)
	}
	if _, err := model.DecodeRestorePlan(plan); err != nil {
		return Created{}, fmt.Errorf("%w: %w", ErrInvalidJob, err)
	}

	claimToken, err := token.New()
	if err != nil {
		return Created{}, err
	}

	job := Job{
		RestoreID:      backupmodel.NewID(),
		RecoveryID:     backupmodel.NewID(),
		ClaimTokenHash: token.Hash(claimToken),
		Status:         StatusAwaitingAgent,
		Plan:           plan,
		RepositoryID:   origin.RepositoryID,
	}
	expiresAt, err := s.store.Insert(ctx, job, s.claimTTL)
	if err != nil {
		return Created{}, fmt.Errorf("store restore job: %w", err)
	}
	return Created{
		RestoreID:      job.RestoreID,
		RecoveryID:     job.RecoveryID,
		ClaimToken:     claimToken,
		ClaimExpiresAt: expiresAt,
	}, nil
}

// Claim hands the restore plan and repository access to the agent presenting
// the job's claim token. The first claim must arrive inside the claim window.
// Afterwards the same request key receives the same response until the
// redelivery window closes, so an agent whose response was lost can retry.
func (s *Service) Claim(ctx context.Context, recoveryID, claimToken, requestKey string) (model.RestoreClaimResp, error) {
	job, now, err := s.store.Get(ctx, recoveryID)
	if err != nil {
		return model.RestoreClaimResp{}, err
	}
	if !token.Matches(job.ClaimTokenHash, claimToken) {
		return model.RestoreClaimResp{}, ErrInvalidToken
	}
	if job.RepositoryID == "" {
		return model.RestoreClaimResp{}, ErrLegacyJob
	}

	// Whether this request may have the job is settled before anything is
	// decrypted or recorded.
	if job.ClaimRequestKey != "" {
		if err := mayRedeliver(job, now, requestKey); err != nil {
			return model.RestoreClaimResp{}, err
		}
	} else if job.Status != StatusAwaitingAgent || !now.Before(job.ClaimExpiresAt) {
		return model.RestoreClaimResp{}, ErrClaimExpired
	}

	// The repository is opened before the claim is recorded. If it cannot be,
	// nothing changes and the agent retries the same claim later; recorded
	// first, the job would be claimed by an agent that received nothing.
	access, err := s.repos.Access(ctx, job.RepositoryID)
	if err != nil {
		return model.RestoreClaimResp{}, fmt.Errorf("repository %s of restore %s: %w", job.RepositoryID, job.RestoreID, err)
	}
	resp := response(job, access)

	if job.ClaimRequestKey != "" {
		return s.withReportToken(ctx, resp, requestKey)
	}
	claimed, err := s.store.TryClaim(ctx, recoveryID, requestKey, s.redelivery)
	if err != nil {
		return model.RestoreClaimResp{}, fmt.Errorf("claim restore job: %w", err)
	}
	if claimed {
		// Plan and repository are immutable, so the job read above is current.
		return s.withReportToken(ctx, resp, requestKey)
	}

	// Another request claimed it concurrently, or the window closed meanwhile.
	job, now, err = s.store.Get(ctx, recoveryID)
	if err != nil {
		return model.RestoreClaimResp{}, err
	}
	if job.ClaimRequestKey == "" {
		return model.RestoreClaimResp{}, ErrClaimExpired
	}
	if err := mayRedeliver(job, now, requestKey); err != nil {
		return model.RestoreClaimResp{}, err
	}
	return s.withReportToken(ctx, resp, requestKey)
}

// mayRedeliver reports whether requestKey may fetch an already claimed job's
// response again.
func mayRedeliver(job Job, now time.Time, requestKey string) error {
	if requestKey != job.ClaimRequestKey {
		return ErrClaimedByOther
	}
	if job.RedeliveryExpiresAt == nil || !now.Before(*job.RedeliveryExpiresAt) {
		return ErrRedeliveryExpired
	}
	return nil
}

// withReportToken issues a new report token and adds it to resp. Only its hash
// is stored, so a redelivered response cannot repeat the earlier token; it
// replaces it instead, as a re-claimed backup job gets a new lease token.
func (s *Service) withReportToken(ctx context.Context, resp model.RestoreClaimResp, requestKey string) (model.RestoreClaimResp, error) {
	reportToken, err := token.New()
	if err != nil {
		return model.RestoreClaimResp{}, err
	}
	if err := s.store.SetReportToken(ctx, resp.RecoveryID, requestKey, token.Hash(reportToken)); err != nil {
		return model.RestoreClaimResp{}, fmt.Errorf("issue report token: %w", err)
	}
	resp.ReportToken = reportToken
	return resp, nil
}

// authorized returns the job for recoveryID if reportToken is the one last
// issued for it.
func (s *Service) authorized(ctx context.Context, recoveryID, reportToken string) (Job, error) {
	job, _, err := s.store.Get(ctx, recoveryID)
	if err != nil {
		return Job{}, err
	}
	if len(job.ReportTokenHash) == 0 || !token.Matches(job.ReportTokenHash, reportToken) {
		return Job{}, ErrReportTokenRejected
	}
	return job, nil
}

// RecordProgress records a progress report. A report older than the last one
// recorded is accepted and ignored, since retries can arrive out of order.
func (s *Service) RecordProgress(ctx context.Context, recoveryID, reportToken string, p Progress) error {
	job, err := s.authorized(ctx, recoveryID, reportToken)
	if err != nil {
		return err
	}
	if !job.Status.active() {
		return ErrRestoreFinished
	}
	recorded, err := s.store.UpdateProgress(ctx, recoveryID, p)
	if err != nil {
		return err
	}
	if recorded {
		return nil
	}
	// Either the report was stale or the job ended in the meantime.
	job, _, err = s.store.Get(ctx, recoveryID)
	if err != nil {
		return err
	}
	if !job.Status.active() {
		return ErrRestoreFinished
	}
	return nil
}

// RecordResult records how the restore ended. Repeating the recorded result is
// accepted, since the agent cannot know whether its first report arrived; a
// different one is refused.
func (s *Service) RecordResult(ctx context.Context, recoveryID, reportToken string, r Result) (Status, error) {
	if _, err := s.authorized(ctx, recoveryID, reportToken); err != nil {
		return "", err
	}
	// A restore that failed before formatting left the disks as they were, so
	// it can be run again under this job — on the same rescue instance, once
	// the cause is fixed — rather than needing a new job and a new instance.
	if r.Outcome == OutcomeFailed && r.Phase == model.RestorePhasePreparing {
		reopened, err := s.store.Reopen(ctx, recoveryID, r)
		if err != nil {
			return "", err
		}
		if reopened {
			return StatusClaimed, nil
		}
		// The job has already ended; answer as for any late result.
	}
	final := StatusFailed
	if r.Outcome == OutcomeSucceeded {
		final = StatusAwaitingBoot
	}
	finished, err := s.store.Finish(ctx, recoveryID, final, r)
	if err != nil {
		return "", err
	}
	if finished {
		return final, nil
	}

	job, _, err := s.store.Get(ctx, recoveryID)
	if err != nil {
		return "", err
	}
	if job.Status.active() {
		// Finish matches every active status, so this means a store that
		// changed between the two calls in a way it should not.
		return "", fmt.Errorf("restore %s is still active after its result was refused", job.RestoreID)
	}
	if job.Outcome != r.Outcome || job.Message != r.Message {
		return "", ErrResultConflict
	}
	return job.Status, nil
}

// Get returns the restore job with the given restore ID or recovery ID. An
// operator holds either: the restore ID from "restore create", the recovery ID
// from the instance's init script and the rescue agent's log.
func (s *Service) Get(ctx context.Context, id string) (Job, error) {
	job, err := s.store.Lookup(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Job{}, fmt.Errorf("%w: no restore job has restore ID or recovery ID %q", ErrNotFound, id)
	}
	return job, err
}

func response(job Job, access model.RepositoryAccess) model.RestoreClaimResp {
	return model.RestoreClaimResp{
		RestoreID:  job.RestoreID,
		RecoveryID: job.RecoveryID,
		Plan:       job.Plan,
		Repository: access,
	}
}
