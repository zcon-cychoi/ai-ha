package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/agent/framework"
	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
)

// fakeFramework stands in for the framework, recording what the worker sent.
type fakeFramework struct {
	mu sync.Mutex

	claimErr   error
	interval   int
	leaseSecs  int
	cancelAt   int64 // report seq from which cancelRequested is returned
	progressFn func(seq int64) error

	progress []model.AgentJobProgressReq
	results  []model.AgentJobResultReq
}

func (f *fakeFramework) Claim(context.Context, string, string, string) (model.AgentJobClaimResp, error) {
	if f.claimErr != nil {
		return model.AgentJobClaimResp{}, f.claimErr
	}
	return model.AgentJobClaimResp{
		JobID: "job-1", AttemptID: "attempt-1", LeaseToken: "lease",
		Spec:                    json.RawMessage(`{}`),
		ProgressIntervalSeconds: f.interval,
		LeaseSeconds:            f.leaseSecs,
	}, nil
}

func (f *fakeFramework) Progress(_ context.Context, _ string, _ string, req model.AgentJobProgressReq) (model.AgentJobProgressResp, error) {
	f.mu.Lock()
	f.progress = append(f.progress, req)
	fn, cancelAt := f.progressFn, f.cancelAt
	f.mu.Unlock()

	if fn != nil {
		if err := fn(req.Seq); err != nil {
			return model.AgentJobProgressResp{}, err
		}
	}
	return model.AgentJobProgressResp{CancelRequested: cancelAt > 0 && req.Seq >= cancelAt}, nil
}

func (f *fakeFramework) Result(_ context.Context, _ string, _ string, req model.AgentJobResultReq) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results = append(f.results, req)
	return nil
}

func (f *fakeFramework) reports() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.progress)
}

func (f *fakeFramework) resultList() []model.AgentJobResultReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.AgentJobResultReq(nil), f.results...)
}

// runnerFunc adapts a function to Runner.
type runnerFunc func(ctx context.Context, progress *atomic.Int64) (Outcome, error)

func (r runnerFunc) Run(ctx context.Context, _ string, _ json.RawMessage, _ model.RepositoryAccess, progress *atomic.Int64) (Outcome, error) {
	return r(ctx, progress)
}

func newTestWorker(t *testing.T, fw Framework, runner Runner) *Worker {
	t.Helper()
	return New(fw, "agent-token", runner, t.TempDir())
}

func testJob() model.AgentJob {
	return model.AgentJob{JobID: "job-1", Kind: "backup", Spec: json.RawMessage(`{}`)}
}

func TestExecuteReportsCompletion(t *testing.T) {
	fw := &fakeFramework{interval: 1, leaseSecs: 3}
	w := newTestWorker(t, fw, runnerFunc(func(_ context.Context, progress *atomic.Int64) (Outcome, error) {
		progress.Add(1024)
		return Outcome{Message: "captured", Detail: map[string]string{"setId": "s1"}}, nil
	}))

	if err := w.Execute(context.Background(), testJob()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	results := fw.resultList()
	if len(results) != 1 {
		t.Fatalf("%d results sent, want 1", len(results))
	}
	if results[0].Outcome != "completed" || results[0].Message != "captured" {
		t.Errorf("result = %+v", results[0])
	}
	if string(results[0].Detail) != `{"setId":"s1"}` {
		t.Errorf("detail = %s", results[0].Detail)
	}
}

// The framework returns an unstarted claim to the queue on the understanding
// that nothing was written. If the start is never acknowledged, the work must
// never begin — otherwise a second agent could be handed the same job while
// this one writes.
func TestNothingRunsUntilTheStartIsAcknowledged(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the start-report backoff")
	}
	fw := &fakeFramework{
		interval: 1, leaseSecs: 3,
		progressFn: func(int64) error { return fmt.Errorf("%w: down", framework.ErrUnavailable) },
	}
	var ran atomic.Bool
	w := newTestWorker(t, fw, runnerFunc(func(context.Context, *atomic.Int64) (Outcome, error) {
		ran.Store(true)
		return Outcome{}, nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := w.Execute(ctx, testJob()); err == nil {
		t.Fatal("a job whose start was never acknowledged reported success")
	}
	if ran.Load() {
		t.Fatal("work started although the framework never acknowledged the start")
	}
	if len(fw.resultList()) != 0 {
		t.Error("a result was sent for work that never started")
	}
}

// A lease lost mid-run means the framework has already moved on. The work
// must stop, and no result may be sent, since the framework would refuse it.
func TestLosingTheLeaseStopsTheWork(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on report intervals")
	}
	fw := &fakeFramework{
		interval: 1, leaseSecs: 5,
		progressFn: func(seq int64) error {
			if seq >= 2 { // the start is acknowledged; the next report is refused
				return fmt.Errorf("%w: superseded", framework.ErrLeaseLost)
			}
			return nil
		},
	}

	stopped := make(chan struct{})
	w := newTestWorker(t, fw, runnerFunc(func(ctx context.Context, _ *atomic.Int64) (Outcome, error) {
		<-ctx.Done() // runs until told to stop
		close(stopped)
		return Outcome{}, ctx.Err()
	}))

	err := w.Execute(context.Background(), testJob())
	if !errors.Is(err, ErrLostJob) {
		t.Fatalf("err = %v, want ErrLostJob", err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("the work was not stopped after the lease was lost")
	}
	if len(fw.resultList()) != 0 {
		t.Error("a result was sent for an attempt the framework no longer recognises")
	}
}

// An unreachable framework does not stop the work at once — the lease still
// holds — but the work must stop before the lease runs out, not after. Once it
// lapses the framework fails the job, and anything written later is written
// for nobody.
func TestUnreachableFrameworkStopsTheWorkBeforeTheLeaseRunsOut(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on the lease")
	}
	const leaseSecs = 4
	fw := &fakeFramework{
		interval: 1, leaseSecs: leaseSecs,
		progressFn: func(seq int64) error {
			if seq >= 2 {
				return fmt.Errorf("%w: connection refused", framework.ErrUnavailable)
			}
			return nil
		},
	}

	var stoppedAt atomic.Int64
	start := time.Now()
	w := newTestWorker(t, fw, runnerFunc(func(ctx context.Context, _ *atomic.Int64) (Outcome, error) {
		<-ctx.Done()
		stoppedAt.Store(int64(time.Since(start)))
		return Outcome{}, ctx.Err()
	}))

	if err := w.Execute(context.Background(), testJob()); !errors.Is(err, ErrLostJob) {
		t.Fatalf("err = %v, want ErrLostJob", err)
	}
	elapsed := time.Duration(stoppedAt.Load())
	if elapsed == 0 {
		t.Fatal("the work was never stopped")
	}
	if elapsed >= leaseSecs*time.Second {
		t.Fatalf("the work stopped after %s, not before the %ds lease ran out", elapsed, leaseSecs)
	}
}

// A slow backup is not a dead one. While the work makes no progress at all,
// the reporter must keep renewing the lease from its own goroutine.
func TestReportingContinuesWhileTheWorkIsStalled(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on report intervals")
	}
	fw := &fakeFramework{interval: 1, leaseSecs: 3}

	w := newTestWorker(t, fw, runnerFunc(func(ctx context.Context, _ *atomic.Int64) (Outcome, error) {
		// Stalled: no progress, but alive, for longer than the lease.
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
		return Outcome{Message: "slow but done"}, nil
	}))

	if err := w.Execute(context.Background(), testJob()); err != nil {
		t.Fatalf("a stalled but healthy job failed: %v", err)
	}
	// The run outlasted the 3s lease. Only continued reporting kept it valid.
	if n := fw.reports(); n < 4 {
		t.Fatalf("%d reports over a 5s stall with a 1s interval: the reporter stopped with the work", n)
	}
	if results := fw.resultList(); len(results) != 1 || results[0].Outcome != "completed" {
		t.Fatalf("results = %+v, want one completed", results)
	}

	// Those reports must say the work did not advance, or a stuck agent looks
	// like a working one.
	fw.mu.Lock()
	defer fw.mu.Unlock()
	for _, p := range fw.progress[1:] {
		if p.Advanced {
			t.Errorf("report %d claimed progress during a stall", p.Seq)
		}
	}
}

// A cancel travels on a progress response, the only path to an agent.
func TestCancelStopsTheWorkAndIsReported(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on report intervals")
	}
	fw := &fakeFramework{interval: 1, leaseSecs: 5, cancelAt: 2}

	w := newTestWorker(t, fw, runnerFunc(func(ctx context.Context, _ *atomic.Int64) (Outcome, error) {
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	}))

	if err := w.Execute(context.Background(), testJob()); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("execute: %v", err)
	}
	results := fw.resultList()
	if len(results) != 1 || results[0].Outcome != "cancelled" {
		t.Fatalf("results = %+v, want one cancelled", results)
	}
}

// Two agents handed the same job is normal; losing the claim is not an error.
func TestLosingTheClaimIsNotAnError(t *testing.T) {
	fw := &fakeFramework{
		interval: 1, leaseSecs: 3,
		claimErr: fmt.Errorf("%w: taken", framework.ErrNotClaimable),
	}
	var ran atomic.Bool
	w := newTestWorker(t, fw, runnerFunc(func(context.Context, *atomic.Int64) (Outcome, error) {
		ran.Store(true)
		return Outcome{}, nil
	}))

	if err := w.Execute(context.Background(), testJob()); err != nil {
		t.Fatalf("losing a claim was reported as an error: %v", err)
	}
	if ran.Load() {
		t.Fatal("work ran for a job this agent did not claim")
	}
}

// When the agent itself is shutting down, it reports nothing: the lease will
// lapse and the framework will record the outcome as unknown, which is true.
// Reporting "cancelled" or "failed" would claim more than the agent knows.
func TestAgentShutdownSendsNoResult(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on report intervals")
	}
	fw := &fakeFramework{interval: 1, leaseSecs: 5}

	ctx, cancel := context.WithCancel(context.Background())
	w := newTestWorker(t, fw, runnerFunc(func(runCtx context.Context, _ *atomic.Int64) (Outcome, error) {
		time.AfterFunc(1500*time.Millisecond, cancel) // the agent is told to stop
		<-runCtx.Done()
		return Outcome{}, runCtx.Err()
	}))

	if err := w.Execute(ctx, testJob()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if results := fw.resultList(); len(results) != 0 {
		t.Fatalf("a shutting-down agent sent %+v", results)
	}
}

// A timing pair that cannot keep a lease alive is refused before anything
// runs, rather than failing the job later and less clearly.
func TestUnusableTimingsAreRefused(t *testing.T) {
	fw := &fakeFramework{interval: 5, leaseSecs: 5}
	var ran atomic.Bool
	w := newTestWorker(t, fw, runnerFunc(func(context.Context, *atomic.Int64) (Outcome, error) {
		ran.Store(true)
		return Outcome{}, nil
	}))

	if err := w.Execute(context.Background(), testJob()); err == nil {
		t.Fatal("a lease no longer than the report interval was accepted")
	}
	if ran.Load() {
		t.Fatal("work ran under timings that could not keep its lease")
	}
}
