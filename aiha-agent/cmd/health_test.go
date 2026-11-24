//go:build !rescue

package cmd

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
)

// scriptedHealth answers each report with the next scripted response, and
// signals every call so that a test can wait for one without sleeping.
type scriptedHealth struct {
	mu     sync.Mutex
	answer func(n int) (model.AgentHealthResp, error)
	sent   []model.AgentHealthReq
	calls  chan int
}

func newScriptedHealth(answer func(n int) (model.AgentHealthResp, error)) *scriptedHealth {
	return &scriptedHealth{answer: answer, calls: make(chan int, 100)}
}

func (s *scriptedHealth) Health(_ context.Context, _ string, req model.AgentHealthReq) (model.AgentHealthResp, error) {
	s.mu.Lock()
	s.sent = append(s.sent, req)
	n := len(s.sent)
	s.mu.Unlock()
	s.calls <- n
	return s.answer(n)
}

func (s *scriptedHealth) reports() []model.AgentHealthReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.AgentHealthReq(nil), s.sent...)
}

// measureEcho reports which assignment it was asked to measure under.
func measureEcho(_ context.Context, a *model.HAAssignment) model.AgentHealthReq {
	req := model.AgentHealthReq{CollectedAt: time.Now()}
	if a != nil {
		req.GroupID, req.Role = a.GroupID, a.Role
	}
	return req
}

func assigned(groupID string) model.AgentHealthResp {
	return model.AgentHealthResp{
		Assigned: true, ReportIntervalSeconds: 30,
		Assignment: &model.HAAssignment{GroupID: groupID, Role: "primary",
			Service: model.HAServiceSpec{Unit: "ai.service", Port: 8000}},
	}
}

// waitCalls waits for the n-th call, failing after timeout.
func waitCalls(t *testing.T, s *scriptedHealth, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case got := <-s.calls:
			if got >= n {
				return
			}
		case <-deadline:
			t.Fatalf("waited %s for call %d; reports: %+v", timeout, n, s.reports())
		}
	}
}

func runHealthLoop(t *testing.T, s *scriptedHealth) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		reportHealth(ctx, s, measureEcho, "token")
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cancel
}

func TestReportHealthMeasuresAtOnceUnderANewAssignment(t *testing.T) {
	s := newScriptedHealth(func(int) (model.AgentHealthResp, error) { return assigned("g1"), nil })
	runHealthLoop(t, s)

	// The first report has nothing to measure; the second follows at once,
	// under the assignment the first answer carried.
	waitCalls(t, s, 2, 2*time.Second)
	got := s.reports()
	if got[0].GroupID != "" || got[1].GroupID != "g1" || got[1].Role != "primary" {
		t.Fatalf("reports = %+v", got)
	}
}

func TestReportHealthDoesNotSpinOnChangingAnswers(t *testing.T) {
	s := newScriptedHealth(func(n int) (model.AgentHealthResp, error) {
		return assigned(string(rune('a' + n))), nil
	})
	runHealthLoop(t, s)

	waitCalls(t, s, 2, 2*time.Second)
	// One immediate report per change, then the interval: the third report
	// must not follow at once even though the answer changed again.
	select {
	case n := <-s.calls:
		t.Fatalf("report %d sent without waiting the interval", n)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestReportHealthRetriesAFailedReport(t *testing.T) {
	s := newScriptedHealth(func(n int) (model.AgentHealthResp, error) {
		if n == 1 {
			return model.AgentHealthResp{}, errors.New("framework unavailable")
		}
		return model.AgentHealthResp{ReportIntervalSeconds: 30}, nil
	})
	runHealthLoop(t, s)

	// Retried after the first backoff, well before the normal interval.
	waitCalls(t, s, 2, 3*time.Second)
}

func TestHealthInterval(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		want    time.Duration
	}{
		{0, minHealthInterval},
		{-5, minHealthInterval},
		{30, 30 * time.Second},
		{86400, maxHealthInterval},
	} {
		if got := healthInterval(tc.seconds); got != tc.want {
			t.Errorf("healthInterval(%d) = %s, want %s", tc.seconds, got, tc.want)
		}
	}
}
