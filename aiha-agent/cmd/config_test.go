//go:build !rescue

package cmd

import (
	"strings"
	"testing"

	"github.com/cloud-barista/ai-ha/pkg/agent/state"
)

// The agent already knows which system it is. Making an operator type that
// identifier is how a repository ends up being listed for the wrong system.
func TestResolveSystemIDFallsBackToThisAgentsOwn(t *testing.T) {
	dir := t.TempDir()
	want, err := state.EnsureSystemID(dir)
	if err != nil {
		t.Fatal(err)
	}

	restore := backupStateDir
	backupStateDir = dir
	t.Cleanup(func() { backupStateDir = restore })

	got, err := resolveSystemID("")
	if err != nil {
		t.Fatalf("resolveSystemID: %v", err)
	}
	if got != want {
		t.Errorf("resolveSystemID = %q, want this agent's own %q", got, want)
	}

	// A named system still wins: the repository holds more than one, and an
	// operator inspecting another machine's backups must be able to say so.
	if got, err := resolveSystemID("someone-else"); err != nil || got != "someone-else" {
		t.Errorf("resolveSystemID(%q) = %q, %v", "someone-else", got, err)
	}
}

// Reading must not create. A command that only inspects would otherwise write
// an identifier into the state directory as a side effect of being run, and
// the system's backup history would hang on something nobody chose.
func TestResolveSystemIDDoesNotInventAnIdentifier(t *testing.T) {
	dir := t.TempDir()

	restore := backupStateDir
	backupStateDir = dir
	t.Cleanup(func() { backupStateDir = restore })

	_, err := resolveSystemID("")
	if err == nil {
		t.Fatal("resolveSystemID invented an identifier for a system that has none")
	}
	if !strings.Contains(err.Error(), "--system") {
		t.Errorf("err = %v, want it to say how to name a system", err)
	}
	if _, err := state.ReadSystemID(dir); !strings.Contains(err.Error(), "no system id") {
		t.Errorf("an identifier was written to the state directory: %v", err)
	}
}
