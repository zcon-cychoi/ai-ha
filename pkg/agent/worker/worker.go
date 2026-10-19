// Package worker carries out a job handed to the agent by the framework:
// claiming it, reporting on it while it runs, and reporting how it ended.
//
// Three rules shape it, and each one exists because the obvious alternative
// fails in a way that is hard to see:
//
//   - Reporting runs on its own goroutine, apart from the work. A backup that
//     stalls reading a slow disk must still renew its lease, or a healthy
//     agent loses its job for being slow.
//   - An agent that cannot renew its lease stops its own work. The framework
//     will fail the job once the lease expires; carrying on would leave the
//     agent writing to the repository on behalf of a job that is over.
//   - Nothing is written until the framework has acknowledged that the work
//     started. The framework returns a claimed-but-unstarted job to the queue
//     precisely because nothing has been written yet; starting to write before
//     that acknowledgement would let a second agent overlap the first.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/agent/framework"
	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/filelock"
	"github.com/rs/zerolog/log"
)

// Phases reported to the framework.
const (
	phaseStarting = "starting"
	phaseRunning  = "running"
)

// startReportAttempts bounds how long the agent tries to have its start
// acknowledged before giving up without writing anything.
const startReportAttempts = 5

// resultRetryWindow bounds how long the agent keeps trying to deliver a result.
// A result is safe to repeat, so the agent retries; but not forever.
const resultRetryWindow = 5 * time.Minute

// workLockTimeout bounds how long a job waits for other work on this host to
// finish before it gives up on its claim.
const workLockTimeout = 10 * time.Second

// Runner does the work a job asks for.
type Runner interface {
	// Run performs the work described by spec against repo, the repository
	// the framework handed out with the job. It must stop promptly when ctx is
	// cancelled and add to progress as it goes, since progress is read
	// concurrently by the reporter. repo holds credentials: never log it.
	Run(ctx context.Context, jobID string, spec json.RawMessage, repo model.RepositoryAccess, progress *atomic.Int64) (Outcome, error)
}

// Outcome is what a runner produced.
type Outcome struct {
	// Message summarises the result for an operator.
	Message string
	// Detail is recorded by the framework as reported.
	Detail any
}

// Framework is the part of the framework client the worker uses.
type Framework interface {
	Claim(ctx context.Context, agentToken, jobID, claimRequestKey string) (model.AgentJobClaimResp, error)
	Progress(ctx context.Context, agentToken, jobID string, req model.AgentJobProgressReq) (model.AgentJobProgressResp, error)
	Result(ctx context.Context, agentToken, jobID string, req model.AgentJobResultReq) error
}

// Worker carries out jobs one at a time.
type Worker struct {
	framework  Framework
	agentToken string
	runner     Runner
	// lockPath serialises work on this host, so that a job from the framework
	// and a backup started by hand cannot run over each other.
	lockPath string
}

// New returns a worker. stateDir is where the agent keeps its local state; the
// work lock lives there.
func New(fw Framework, agentToken string, runner Runner, stateDir string) *Worker {
	return &Worker{
		framework:  fw,
		agentToken: agentToken,
		runner:     runner,
		lockPath:   filepath.Join(stateDir, "work.lock"),
	}
}

// ErrLostJob reports that the framework took the job away mid-run: its lease
// expired or was superseded. The work was stopped and no result was sent,
// because the framework would refuse it.
var ErrLostJob = errors.New("the framework no longer recognises this job's attempt")

// Execute claims and runs one job handed out by the long poll.
//
// It returns nil when the job was claimed by someone else, since that is the
// normal outcome of two agents being handed the same work.
func (w *Worker) Execute(ctx context.Context, job model.AgentJob) error {
	logger := log.With().Str("jobId", job.JobID).Logger()

	// Taken before claiming, so that an agent busy with other work does not
	// claim a job it cannot start and leave it to time out.
	lock, err := filelock.Acquire(ctx, w.lockPath, workLockTimeout)
	if err != nil {
		if errors.Is(err, filelock.ErrLocked) {
			logger.Warn().Msg("Other work is running on this host; leaving the job for later")
			return nil
		}
		return fmt.Errorf("acquire the work lock: %w", err)
	}
	defer lock.Release()

	key, err := newClaimKey()
	if err != nil {
		return err
	}

	claimed, err := w.claim(ctx, job.JobID, key)
	switch {
	case errors.Is(err, framework.ErrNotClaimable):
		logger.Info().Msg("Another claim holds this job")
		return nil
	case err != nil:
		return fmt.Errorf("claim job %s: %w", job.JobID, err)
	}

	logger = logger.With().Str("attemptId", claimed.AttemptID).Logger()
	logger.Info().Msg("Job claimed")

	return w.run(ctx, logger, claimed)
}

// claim takes the job, retrying a lost response with the same key.
//
// The framework recognises a repeated key and hands back the same attempt with
// a usable lease, so retrying cannot claim the job twice.
func (w *Worker) claim(ctx context.Context, jobID, key string) (model.AgentJobClaimResp, error) {
	delay := time.Second
	for attempt := 1; ; attempt++ {
		claimed, err := w.framework.Claim(ctx, w.agentToken, jobID, key)
		if err == nil || !framework.Retryable(err) || attempt >= startReportAttempts {
			return claimed, err
		}
		if !sleep(ctx, delay) {
			return model.AgentJobClaimResp{}, ctx.Err()
		}
		delay *= 2
	}
}

// run carries out a claimed job.
func (w *Worker) run(ctx context.Context, logger zeroLogger, claimed model.AgentJobClaimResp) error {
	interval := time.Duration(claimed.ProgressIntervalSeconds) * time.Second
	lease := time.Duration(claimed.LeaseSeconds) * time.Second
	if interval <= 0 || lease <= interval {
		// The framework is expected to hand out usable timings. Running with
		// ones that cannot keep a lease alive would only fail the job later
		// and less clearly.
		return fmt.Errorf("the framework handed out unusable timings (report every %s, lease %s)", interval, lease)
	}

	r := &reporter{
		framework: w.framework,
		token:     w.agentToken,
		jobID:     claimed.JobID,
		attemptID: claimed.AttemptID,
		lease:     claimed.LeaseToken,
		interval:  interval,
		leaseTTL:  lease,
		progress:  new(atomic.Int64),
		logger:    logger,
	}

	// Nothing may be written until the framework has acknowledged the start.
	// It returns an unstarted claim to the queue on the understanding that
	// nothing was written; writing first would break that understanding and
	// let a second agent overlap this one.
	if err := r.reportStart(ctx); err != nil {
		if errors.Is(err, framework.ErrLeaseLost) {
			logger.Warn().Err(err).Msg("The framework took the job back before it started; nothing was written")
			return nil
		}
		// The start was never acknowledged, so nothing was written and the
		// framework will return the claim to the queue on its own.
		return fmt.Errorf("the start of job %s was never acknowledged; not starting: %w", claimed.JobID, err)
	}

	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()

	var reportWG sync.WaitGroup
	reportWG.Add(1)
	go func() {
		defer reportWG.Done()
		r.loop(workCtx, cancelWork)
	}()

	outcome, runErr := w.runner.Run(workCtx, claimed.JobID, claimed.Spec, claimed.Repository, r.progress)

	cancelWork()
	reportWG.Wait()

	switch {
	case r.lost.Load():
		// The framework has already moved on. A result would be refused, and
		// the work was stopped as soon as the lease was lost.
		logger.Error().Msg("Stopped: the lease on this job was lost")
		return ErrLostJob

	case ctx.Err() != nil && !r.cancelled.Load():
		// The agent itself is shutting down, not the job being cancelled.
		// Report nothing: the lease will expire and the framework will record
		// the outcome as unknown, which is the truth.
		logger.Warn().Msg("Agent stopping mid-job; leaving the outcome to the framework")
		return nil
	}

	result := model.AgentJobResultReq{
		AttemptID:  claimed.AttemptID,
		LeaseToken: claimed.LeaseToken,
	}
	switch {
	case r.cancelled.Load():
		result.Outcome = "cancelled"
		result.Message = "Cancelled at the framework's request"
	case runErr != nil:
		result.Outcome = "failed"
		result.Message = runErr.Error()
	default:
		result.Outcome = "completed"
		result.Message = outcome.Message
	}
	if outcome.Detail != nil {
		detail, err := json.Marshal(outcome.Detail)
		if err != nil {
			return fmt.Errorf("encode the job result: %w", err)
		}
		result.Detail = detail
	}

	if err := w.deliverResult(ctx, claimed.JobID, result); err != nil {
		logger.Error().Err(err).Str("outcome", result.Outcome).Msg("Could not report the job result")
		return err
	}
	logger.Info().Str("outcome", result.Outcome).Msg("Job finished")
	return runErr
}

// deliverResult reports the result, retrying while the framework is
// unavailable. A result is safe to repeat: the framework accepts the same one
// again and refuses only a different one.
func (w *Worker) deliverResult(ctx context.Context, jobID string, result model.AgentJobResultReq) error {
	// Delivered even if the agent is stopping: the work is done and the
	// framework should hear how it went.
	deliverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resultRetryWindow)
	defer cancel()

	delay := time.Second
	for {
		err := w.framework.Result(deliverCtx, w.agentToken, jobID, result)
		if err == nil || !framework.Retryable(err) {
			return err
		}
		if !sleep(deliverCtx, jitter(delay)) {
			return fmt.Errorf("gave up delivering the result: %w", err)
		}
		delay = min(2*delay, 30*time.Second)
	}
}

// newClaimKey returns a fresh key scoping one claim request.
func newClaimKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate a claim key: %w", err)
	}
	return hex.EncodeToString(b), nil
}
