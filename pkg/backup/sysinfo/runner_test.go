package sysinfo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExecRunnerCapturesStdout(t *testing.T) {
	out, err := ExecRunner{}.Run(context.Background(), "echo", "hello")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hello" {
		t.Fatalf("stdout = %q", out)
	}
}

// A missing tool must be distinguishable, because the collector treats it as
// "this system has no such subsystem" rather than as a failure.
func TestExecRunnerReportsMissingCommand(t *testing.T) {
	_, err := ExecRunner{}.Run(context.Background(), "aiha-no-such-command-exists", "x")
	if !errors.Is(err, ErrCommandNotFound) {
		t.Fatalf("err = %v, want ErrCommandNotFound", err)
	}
}

// A tool that runs and fails is a different case, and its diagnostics belong
// in the error rather than in the output.
func TestExecRunnerFoldsStderrIntoTheError(t *testing.T) {
	out, err := ExecRunner{}.Run(context.Background(), "sh", "-c", "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("a failing command returned no error")
	}
	if errors.Is(err, ErrCommandNotFound) {
		t.Fatalf("a failing command was reported as missing: %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("stderr not reported in the error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("output returned alongside an error: %q", out)
	}
}

func TestExecRunnerTimesOut(t *testing.T) {
	start := time.Now()
	_, err := ExecRunner{Timeout: 100 * time.Millisecond}.
		Run(context.Background(), "sleep", "10")
	if err == nil {
		t.Fatal("a command that outran its timeout returned no error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout did not take effect: waited %s", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline error", err)
	}
}

// Collection commands report locale-sensitive formats; the locale is pinned so
// a parser never meets translated field names.
func TestExecRunnerPinsLocale(t *testing.T) {
	out, err := ExecRunner{}.Run(context.Background(), "sh", "-c", "echo $LC_ALL")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(string(out)) != "C" {
		t.Fatalf("LC_ALL = %q, want C", strings.TrimSpace(string(out)))
	}
}
