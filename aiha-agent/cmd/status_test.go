//go:build !rescue

package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/config"
	"github.com/cloud-barista/ai-ha/pkg/agent/state"
)

// theToken is what must never be printed. It is distinctive so that a leak
// anywhere in the output is unambiguous.
const theToken = "s3cret-agent-token-do-not-print"

func enrolledCredential() state.Credential {
	return state.Credential{
		AgentID:             "3bff517cfccd5833",
		SystemID:            "ba021449c3f971e8",
		Token:               theToken,
		FrameworkURL:        "https://10.0.1.7:8443",
		FrameworkCertSHA256: "ab" + strings.Repeat("cd", 31),
		RegisteredAt:        time.Date(2026, 9, 18, 4, 21, 9, 0, time.UTC),
	}
}

// statusOf runs the command's body against a state directory and returns what
// an operator would see.
func statusOf(t *testing.T, dir string, cred *state.Credential) string {
	t.Helper()

	cfg := &config.AgentConfig{StateDir: dir}
	if cred != nil {
		cfg.FrameworkURL = cred.FrameworkURL
		cfg.FrameworkCertSHA256 = cred.FrameworkCertSHA256
	}

	var out bytes.Buffer
	if err := printStatus(&out, cfg); err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	return out.String()
}

// The point of the command is to hand an operator the identifiers that the
// framework's own commands take.
func TestStatusShowsTheIdentifiersAnOperatorNeeds(t *testing.T) {
	dir := t.TempDir()
	cred := enrolledCredential()
	if _, err := state.EnsureSystemID(dir); err != nil {
		t.Fatal(err)
	}
	systemID, err := state.ReadSystemID(dir)
	if err != nil {
		t.Fatal(err)
	}
	cred.SystemID = systemID
	if err := state.WriteCredential(dir, cred); err != nil {
		t.Fatal(err)
	}

	got := statusOf(t, dir, &cred)
	for _, want := range []string{systemID, cred.AgentID, cred.FrameworkURL, "2026-09-18T04:21:09Z", dir} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not mention %q:\n%s", want, got)
		}
	}
}

// The token sits in the same file as the identifiers. Printing it would put a
// long-lived credential into a terminal, a screenshot and a support ticket.
func TestStatusNeverPrintsTheToken(t *testing.T) {
	dir := t.TempDir()
	cred := enrolledCredential()
	if err := state.WriteCredential(dir, cred); err != nil {
		t.Fatal(err)
	}

	if got := statusOf(t, dir, &cred); strings.Contains(got, theToken) {
		t.Errorf("the agent token was printed:\n%s", got)
	}
}

// An agent that has not enrolled is exactly when someone runs this. It has to
// answer rather than fail.
func TestStatusOnAnAgentThatHasNotEnrolled(t *testing.T) {
	dir := t.TempDir()

	got := statusOf(t, dir, nil)
	if !strings.Contains(got, "not enrolled") {
		t.Errorf("output does not say the agent has not enrolled:\n%s", got)
	}
	if !strings.Contains(got, "none yet") {
		t.Errorf("output does not say there is no system id yet:\n%s", got)
	}
}

// Pointing the agent at a different framework stops it from starting, and the
// framework's side shows nothing at all. This is where the reason has to be.
func TestStatusReportsACredentialFromAnotherFramework(t *testing.T) {
	dir := t.TempDir()
	cred := enrolledCredential()
	if err := state.WriteCredential(dir, cred); err != nil {
		t.Fatal(err)
	}

	cfg := &config.AgentConfig{
		StateDir:            dir,
		FrameworkURL:        "https://10.0.9.9:8443",
		FrameworkCertSHA256: cred.FrameworkCertSHA256,
	}
	var out bytes.Buffer
	if err := printStatus(&out, cfg); err != nil {
		t.Fatalf("printStatus: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, cred.FrameworkURL) || !strings.Contains(got, "https://10.0.9.9:8443") {
		t.Errorf("output does not name both frameworks:\n%s", got)
	}
}

// A restored system carries the original's identity and must not act as it.
// An operator wondering why the agent is idle should find that here.
func TestStatusReportsAPendingRecovery(t *testing.T) {
	dir := t.TempDir()
	// state exposes no writer for this marker: the restore executor writes it
	// onto the restored disk, not the agent.
	marker := map[string]any{
		"recoveryId":      "7f3ae1c2",
		"restoredAt":      "2026-09-20T22:14:00Z",
		"bootVerifyToken": theToken,
	}
	b, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "recovery-pending.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	got := statusOf(t, dir, nil)
	if !strings.Contains(got, "2026-09-20T22:14:00Z") {
		t.Errorf("output does not report the pending recovery:\n%s", got)
	}
	if strings.Contains(got, theToken) {
		t.Errorf("the boot verification token was printed:\n%s", got)
	}
}
