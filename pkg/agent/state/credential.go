package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// credentialFile holds what the agent authenticates to the framework with.
const credentialFile = "agent-token"

// ErrNoCredential reports that this agent has not enrolled yet.
var ErrNoCredential = errors.New("no agent credential")

// ErrCredentialExists reports that a credential is already stored. Enrolling
// again would strand the existing agent on the framework, so the caller is
// expected to clear it deliberately rather than overwrite it.
var ErrCredentialExists = errors.New("an agent credential is already stored")

// Credential is the agent's identity and token, with the framework it belongs
// to.
//
// The framework is recorded alongside the token for a reason: pointing the
// agent at a different framework must not make it present credentials issued
// by another one. On start the agent compares these against its configuration
// and stops if they disagree.
type Credential struct {
	AgentID  string `json:"agentId"`
	SystemID string `json:"systemId"`
	// Token authenticates every request. It is a secret: never log it.
	Token string `json:"token"`
	// FrameworkURL and FrameworkCertSHA256 are the framework this credential
	// was issued by.
	FrameworkURL        string    `json:"frameworkUrl"`
	FrameworkCertSHA256 string    `json:"frameworkCertSha256"`
	RegisteredAt        time.Time `json:"registeredAt"`
}

// BelongsTo reports whether the credential was issued by the framework the
// agent is configured to talk to.
func (c Credential) BelongsTo(frameworkURL, certSHA256 string) bool {
	return c.FrameworkURL == frameworkURL && c.FrameworkCertSHA256 == certSHA256
}

// ReadCredential returns the stored credential. A dir of "" means Dir. It
// returns ErrNoCredential when the agent has not enrolled.
func ReadCredential(dir string) (Credential, error) {
	path := credentialPath(dir)

	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Credential{}, ErrNoCredential
	}
	if err != nil {
		return Credential{}, fmt.Errorf("read agent credential %s: %w", path, err)
	}

	var cred Credential
	if err := json.Unmarshal(b, &cred); err != nil {
		return Credential{}, fmt.Errorf("parse agent credential %s: %w", path, err)
	}
	if cred.AgentID == "" || cred.Token == "" {
		return Credential{}, fmt.Errorf("agent credential %s is incomplete", path)
	}
	return cred, nil
}

// WriteCredential stores a newly issued credential.
//
// It refuses to replace one that is already there. An agent enrols once; a
// second enrolment leaves the first agent registered on the framework with a
// token nobody holds, which an operator then has to find and revoke. Clearing
// the old credential is therefore a separate, deliberate act — see
// ClearCredential.
func WriteCredential(dir string, cred Credential) error {
	if dir == "" {
		dir = Dir
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory %s: %w", dir, err)
	}

	body, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return fmt.Errorf("encode agent credential: %w", err)
	}

	// Written complete, then linked into place, so that a reader never sees a
	// half-written credential and a second writer loses rather than clobbers.
	tmp, err := writeTemp(dir, credentialFile, append(body, '\n'))
	if err != nil {
		return err
	}
	defer os.Remove(tmp)

	path := filepath.Join(dir, credentialFile)
	if err := publish(tmp, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrCredentialExists
		}
		return fmt.Errorf("store agent credential %s: %w", path, err)
	}
	return nil
}

// ClearCredential removes the stored credential so that the agent can enrol
// again. It leaves the system id in place: that identifier is what the backup
// history hangs on, and re-enrolling an agent must not sever it.
//
// It is not an error to clear a credential that is not there.
func ClearCredential(dir string) error {
	path := credentialPath(dir)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove agent credential %s: %w", path, err)
	}
	return nil
}

// EnsureWritable reports whether the state directory can be written.
//
// The agent checks this before it enrols. Enrolment spends a registration
// token that cannot be spent twice, so discovering afterwards that the
// credential cannot be stored costs the operator a token and leaves an agent
// registered that nobody can authenticate as.
func EnsureWritable(dir string) error {
	if dir == "" {
		dir = Dir
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory %s: %w", dir, err)
	}

	probe, err := writeTemp(dir, ".writable", nil)
	if err != nil {
		return fmt.Errorf("state directory %s is not writable: %w", dir, err)
	}
	return os.Remove(probe)
}

func credentialPath(dir string) string {
	if dir == "" {
		dir = Dir
	}
	return filepath.Join(dir, credentialFile)
}
