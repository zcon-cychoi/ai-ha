package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/backup/source"
)

func writeMarker(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, recoveryPendingFile), []byte(content), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	return dir
}

func TestReadRecoveryPendingAbsent(t *testing.T) {
	pending, err := ReadRecoveryPending(t.TempDir())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if pending != nil {
		t.Fatalf("a system with no marker reports a pending recovery: %+v", pending)
	}
}

func TestReadRecoveryPendingPresent(t *testing.T) {
	dir := writeMarker(t, `{"recoveryId":"645fae09c9ba9281","restoredAt":"2026-09-17T00:00:00Z"}`)

	pending, err := ReadRecoveryPending(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if pending == nil {
		t.Fatal("marker present but no pending recovery reported")
	}
	if pending.RecoveryID != "645fae09c9ba9281" {
		t.Fatalf("recoveryId %q", pending.RecoveryID)
	}
	if pending.RestoredAt.IsZero() {
		t.Fatal("restoredAt not decoded")
	}
}

// A marker we cannot parse still means a recovery happened. Reporting the
// error keeps the caller from treating a restored duplicate as a normal system.
func TestReadRecoveryPendingMalformedIsAnError(t *testing.T) {
	dir := writeMarker(t, "{not json")

	pending, err := ReadRecoveryPending(dir)
	if err == nil {
		t.Fatal("a malformed marker was accepted as no pending recovery")
	}
	if pending != nil {
		t.Fatalf("pending returned alongside an error: %+v", pending)
	}
}

// source.DefaultAgentStateDir spells this directory again so that the backup
// package needs no dependency on the agent. The two must agree: if they drift,
// an agent left on the default silently starts shipping its credentials inside
// every archive.
//
// A configured state directory is covered separately: the source is told where
// it is, so the exclusion follows it rather than relying on this constant.
func TestAgentStateDirMatchesTheExcludedDefault(t *testing.T) {
	if Dir != source.DefaultAgentStateDir {
		t.Fatalf("state.Dir = %q but source.DefaultAgentStateDir = %q: "+
			"the agent's state would no longer be excluded from backups",
			Dir, source.DefaultAgentStateDir)
	}
}

func TestWriteRecoveryPendingRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "var", "lib", "aiha")
	want := RecoveryPending{RecoveryID: "1a2b3c4d", RestoredAt: time.Date(2026, 10, 8, 1, 2, 3, 0, time.UTC)}
	for range 2 { // a rerun of the restore replaces the marker
		if err := WriteRecoveryPending(dir, want); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	got, err := ReadRecoveryPending(dir)
	if err != nil || got == nil {
		t.Fatalf("read: %v, %v", got, err)
	}
	if got.RecoveryID != want.RecoveryID || !got.RestoredAt.Equal(want.RestoredAt) {
		t.Fatalf("marker = %+v, want %+v", got, want)
	}
	info, err := os.Stat(filepath.Join(dir, recoveryPendingFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("marker missing or not 0600: %v", err)
	}
}
