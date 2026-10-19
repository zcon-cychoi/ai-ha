package worker

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/agent/framework"
	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/rs/zerolog"
)

type zeroLogger = zerolog.Logger

// reporter renews an attempt's lease and relays cancellation, on its own
// goroutine, so that a stalled backup still reports that its agent is alive.
type reporter struct {
	framework Framework
	token     string
	jobID     string
	attemptID string
	lease     string
	interval  time.Duration
	leaseTTL  time.Duration
	progress  *atomic.Int64
	logger    zerolog.Logger

	seq          int64
	lastReported int64

	// lost is set when the lease can no longer be counted on. The work is
	// stopped as soon as it is.
	lost atomic.Bool
	// cancelled is set when the framework asked for the job to stop.
	cancelled atomic.Bool
}

// reportStart sends the first report and waits for it to be acknowledged.
//
// Until it is, the framework holds the job as claimed but not started, and
// will hand it to another agent if this one goes quiet. Writing anything
// before then would let the two overlap.
func (r *reporter) reportStart(ctx context.Context) error {
	delay := time.Second
	for attempt := 1; ; attempt++ {
		_, err := r.send(ctx, phaseStarting, false)
		switch {
		case err == nil:
			return nil
		case !framework.Retryable(err) || attempt >= startReportAttempts:
			return err
		}
		r.logger.Warn().Err(err).Msg("Could not report the start of the job; retrying")
		if !sleep(ctx, jitter(delay)) {
			return ctx.Err()
		}
		delay *= 2
	}
}

// loop reports every interval until ctx ends, stopping the work through
// cancelWork when the job is cancelled or the lease is lost.
func (r *reporter) loop(ctx context.Context, cancelWork context.CancelFunc) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	// lastRenewed is when the lease was last known to be extended. The lease
	// is only safe to rely on until lastRenewed + leaseTTL.
	lastRenewed := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Reported whether or not the work moved: a report is what renews the
		// lease, and a slow backup is not a dead one.
		current := r.progress.Load()
		advanced := current != r.lastReported

		resp, err := r.send(ctx, phaseRunning, advanced)
		switch {
		case err == nil:
			lastRenewed = time.Now()
			r.lastReported = current
			if resp.CancelRequested && !r.cancelled.Load() {
				r.logger.Info().Msg("The framework asked for this job to stop")
				r.cancelled.Store(true)
				cancelWork()
			}

		case ctx.Err() != nil:
			return

		case errors.Is(err, framework.ErrLeaseLost), errors.Is(err, framework.ErrNotOwned),
			errors.Is(err, framework.ErrUnauthorized):
			// The framework has decided this attempt is over. There is nothing
			// to wait for, and every write from here on belongs to no job.
			r.logger.Error().Err(err).Msg("The framework no longer recognises this attempt; stopping")
			r.lost.Store(true)
			cancelWork()
			return

		default:
			// The framework could not be reached. The lease still holds for a
			// while, but work must stop before it runs out rather than after:
			// once it lapses the framework fails the job, and anything written
			// later is written for nobody.
			remaining := r.leaseTTL - time.Since(lastRenewed)
			if remaining <= r.interval {
				r.logger.Error().Err(err).Dur("leaseRemaining", remaining).
					Msg("Could not renew the lease in time; stopping the work")
				r.lost.Store(true)
				cancelWork()
				return
			}
			r.logger.Warn().Err(err).Dur("leaseRemaining", remaining).
				Msg("Could not report progress; will retry")
		}
	}
}

// send issues one progress report.
func (r *reporter) send(ctx context.Context, phase string, advanced bool) (model.AgentJobProgressResp, error) {
	r.seq++
	resp, err := r.framework.Progress(ctx, r.token, r.jobID, model.AgentJobProgressReq{
		AttemptID:      r.attemptID,
		LeaseToken:     r.lease,
		Seq:            r.seq,
		Phase:          phase,
		ProcessedBytes: r.progress.Load(),
		Advanced:       advanced,
	})
	if err != nil {
		return model.AgentJobProgressResp{}, fmt.Errorf("report progress: %w", err)
	}
	return resp, nil
}

// sleep waits for d, reporting false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// jitter spreads retries so that agents recovering together do not return in
// lockstep.
func jitter(d time.Duration) time.Duration {
	return d/2 + rand.N(d/2+1)
}
