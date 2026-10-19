package restore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/agent/rescue"
	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/rs/zerolog/log"
)

// Reporter delivers progress and the result to the framework.
// *rescue.ReportClient implements it.
type Reporter interface {
	// Progress sends one report; a failed one is superseded by the next.
	Progress(ctx context.Context, req model.RestoreProgressReq) error
	// Result sends the result, retrying until it is recorded or refused.
	Result(ctx context.Context, req model.RestoreResultReq) error
}

// Phases of a restore, in order. They are reported as the step being carried
// out, so the framework can show "step 4 of 6".
const (
	PhasePreparing     = model.RestorePhasePreparing
	PhaseFormatting    = "formatting"
	PhaseMounting      = "mounting"
	PhaseExtracting    = "extracting"
	PhaseReconfiguring = "reconfiguring"
	PhaseBootloader    = "bootloader"
)

var phases = []string{PhasePreparing, PhaseFormatting, PhaseMounting, PhaseExtracting, PhaseReconfiguring, PhaseBootloader}

// defaultReportInterval is how often progress is reported while it changes
// only in bytes. A phase change is reported at once.
const defaultReportInterval = 10 * time.Second

// tracker holds the restore's current position for the reporter.
type tracker struct {
	mu    sync.Mutex
	phase string
	item  string
	total int64
	// read counts the bytes read from the repository across all files.
	read atomic.Int64
	// seq numbers the reports, so the framework can drop one that arrives
	// after a newer one.
	seq atomic.Int64
	// changed wakes the reporter when the phase or file changes.
	changed chan struct{}
}

// nextSeq returns the sequence number for the next report.
func (t *tracker) nextSeq() int64 { return t.seq.Add(1) }

func newTracker() *tracker {
	return &tracker{changed: make(chan struct{}, 1)}
}

// enter moves to phase and reports it at once.
func (t *tracker) enter(phase string) {
	t.mu.Lock()
	t.phase, t.item = phase, ""
	t.mu.Unlock()
	t.notify()
	log.Info().Str("phase", phase).Int("step", stepOf(phase)).Int("totalSteps", len(phases)).Msg("Restore phase started")
}

// setItem names the backup file being restored.
func (t *tracker) setItem(item string) {
	t.mu.Lock()
	t.item = item
	t.mu.Unlock()
	t.notify()
}

func (t *tracker) setTotal(total int64) {
	t.mu.Lock()
	t.total = total
	t.mu.Unlock()
}

func (t *tracker) currentPhase() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.phase
}

func (t *tracker) notify() {
	select {
	case t.changed <- struct{}{}:
	default:
	}
}

// snapshot is the report for the current position.
func (t *tracker) snapshot(seq int64) model.RestoreProgressReq {
	t.mu.Lock()
	defer t.mu.Unlock()
	return model.RestoreProgressReq{
		Seq:            seq,
		Phase:          t.phase,
		Step:           stepOf(t.phase),
		TotalSteps:     len(phases),
		Item:           t.item,
		ProcessedBytes: t.read.Load(),
		TotalBytes:     t.total,
	}
}

// stepOf is phase's 1-based position.
func stepOf(phase string) int {
	for i, p := range phases {
		if p == phase {
			return i + 1
		}
	}
	return 0
}

// reportLoop reports on its own goroutine until ctx ends, so that a stage
// blocked on a slow command still shows its agent alive. A failed report is
// logged and not retried: the next one carries the same information, and a
// restore must not stop over a report once it has started writing disks.
func reportLoop(ctx context.Context, r Reporter, t *tracker, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	refused := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.changed:
		case <-ticker.C:
		}
		if refused {
			continue
		}
		err := r.Progress(ctx, t.snapshot(t.nextSeq()))
		switch {
		case err == nil, ctx.Err() != nil:
		case errors.Is(err, rescue.ErrReportRefused):
			// The token was superseded or the job ended. Nothing will be
			// accepted from here on; the restore itself carries on.
			log.Error().Err(err).Msg("The framework refused a progress report; no further progress will be reported")
			refused = true
		default:
			log.Warn().Err(err).Msg("Could not report restore progress; will report again")
		}
	}
}
