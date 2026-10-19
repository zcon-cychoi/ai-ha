package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testCredential() Credential {
	return Credential{
		AgentID: "9b1c2d3e4f5a6b7c", SystemID: "sys-0123456789abcdef",
		Token:               "an-agent-token",
		FrameworkURL:        "https://framework.example:8443",
		FrameworkCertSHA256: "3911ba8d8fe5db35ae90469636dcd464c8b7aee357dc1bf9f9e6d8db3b77e81e",
		RegisteredAt:        time.Now().UTC().Truncate(time.Second),
	}
}

func TestCredentialRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := testCredential()

	if err := WriteCredential(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadCredential(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.AgentID != want.AgentID || got.Token != want.Token ||
		got.FrameworkURL != want.FrameworkURL || got.FrameworkCertSHA256 != want.FrameworkCertSHA256 {
		t.Fatalf("read back %+v, wrote %+v", got, want)
	}

	fi, err := os.Stat(filepath.Join(dir, credentialFile))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("credential mode = %v, want 0600", perm)
	}
}

func TestReadCredentialAbsent(t *testing.T) {
	if _, err := ReadCredential(t.TempDir()); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("err = %v, want ErrNoCredential", err)
	}
}

// Enrolling over an existing credential would leave the previous agent
// registered on the framework with a token nobody holds. Clearing it has to be
// deliberate.
func TestWriteCredentialRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	first := testCredential()
	if err := WriteCredential(dir, first); err != nil {
		t.Fatalf("first write: %v", err)
	}

	second := testCredential()
	second.AgentID = "other-agent"
	second.Token = "other-token"
	if err := WriteCredential(dir, second); !errors.Is(err, ErrCredentialExists) {
		t.Fatalf("err = %v, want ErrCredentialExists", err)
	}

	got, err := ReadCredential(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.AgentID != first.AgentID {
		t.Fatalf("the stored credential was replaced: %s", got.AgentID)
	}
}

// Clearing lets the agent enrol again; the system id must survive it, because
// the backup history hangs on that identifier.
func TestClearCredentialKeepsTheSystemID(t *testing.T) {
	dir := t.TempDir()

	systemID, err := EnsureSystemID(dir)
	if err != nil {
		t.Fatalf("ensure system id: %v", err)
	}
	if err := WriteCredential(dir, testCredential()); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := ClearCredential(dir); err != nil {
		t.Fatalf("clear: %v", err)
	}

	if _, err := ReadCredential(dir); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("credential survived the clear: %v", err)
	}
	after, err := ReadSystemID(dir)
	if err != nil {
		t.Fatalf("read system id after clear: %v", err)
	}
	if after != systemID {
		t.Fatalf("the system id changed across a clear: %q -> %q", systemID, after)
	}

	// And enrolling again must now succeed.
	if err := WriteCredential(dir, testCredential()); err != nil {
		t.Fatalf("write after clear: %v", err)
	}
}

func TestClearCredentialWhenAbsent(t *testing.T) {
	if err := ClearCredential(t.TempDir()); err != nil {
		t.Fatalf("clearing an absent credential failed: %v", err)
	}
}

// Pointing the agent at a different framework must not make it present a
// credential another framework issued.
func TestCredentialBelongsTo(t *testing.T) {
	cred := testCredential()

	if !cred.BelongsTo(cred.FrameworkURL, cred.FrameworkCertSHA256) {
		t.Error("a credential does not recognise its own framework")
	}
	if cred.BelongsTo("https://elsewhere.example:8443", cred.FrameworkCertSHA256) {
		t.Error("a credential accepted a different framework URL")
	}
	if cred.BelongsTo(cred.FrameworkURL, "0000000000000000000000000000000000000000000000000000000000000000") {
		t.Error("a credential accepted a different certificate pin")
	}
}

// Enrolment spends a token that cannot be spent twice, so an unwritable state
// directory has to be found before the request goes out, not after.
func TestEnsureWritable(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureWritable(dir); err != nil {
		t.Fatalf("a writable directory was reported unwritable: %v", err)
	}

	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this checks")
	}
	readOnly := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := EnsureWritable(readOnly); err == nil {
		t.Fatal("an unwritable directory was reported writable")
	}
}

func TestReadCredentialRejectsIncompleteFiles(t *testing.T) {
	for _, body := range []string{"{", "{}", `{"agentId":"x"}`, `{"token":"y"}`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, credentialFile), []byte(body), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := ReadCredential(dir); err == nil {
			t.Errorf("ReadCredential accepted %q", body)
		}
	}
}
