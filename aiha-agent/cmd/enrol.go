//go:build !rescue

package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cloud-barista/ai-ha/config"
	"github.com/cloud-barista/ai-ha/pkg/agent/framework"
	"github.com/cloud-barista/ai-ha/pkg/agent/state"
	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/filelock"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

// Exit codes for conditions that will not improve by running again. The
// systemd unit excludes them from automatic restart: retrying a credential
// that has been revoked only fills the log.
const (
	exitReenrolNeeded   = 10
	exitRecoveryPending = 11
	exitConfiguration   = 12
)

// enrolLockTimeout bounds how long "register" and "run" wait for each other.
// They contend only at startup, so this is short.
const enrolLockTimeout = 10 * time.Second

// registrationKeyPattern matches what the framework accepts.
var registrationKeyPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// registrationKeyFile records the key for an enrolment attempt. It is written
// before the first request, so that a retry is recognised as a retry.
const registrationKeyFile = "registration.key"

var agentCfg *config.AgentConfig

// loadAgentConfig reads AIHA_AGENT_*. It is the PreRunE of the commands that
// talk to the framework; they deliberately do not read repository credentials.
func loadAgentConfig(*cobra.Command, []string) error {
	loaded, err := config.LoadAgent()
	if err != nil {
		return err
	}
	agentCfg = loaded
	return nil
}

var registerCmd = &cobra.Command{
	Use:   "register",
	Short: "Enrol this agent with the framework",
	Long: `Spend a registration token to receive this agent's identity and its long-lived
token, and store them under the state directory.

The registration token comes from AIHA_AGENT_REGISTRATION_TOKEN or the file
AIHA_AGENT_REGISTRATION_TOKEN_FILE names. It is not a flag: a flag is visible
to every process on the host.

Enrolling is not repeatable. A successful registration spends the token and the
agent token it returns cannot be reissued, so an agent that already holds a
credential is left alone. To replace one, clear it with "aiha-agent reset" and
enrol with a new registration token.`,
	PreRunE: loadAgentConfig,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()

		cred, err := enrol(ctx)
		if errors.Is(err, errAlreadyEnrolled) {
			existing, readErr := state.ReadCredential(agentCfg.StateDir)
			if readErr != nil {
				return readErr
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Already enrolled as agent %s for system %s\n", existing.AgentID, existing.SystemID)
			return nil
		}
		if err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Enrolled as agent %s for system %s\n", cred.AgentID, cred.SystemID)
		fmt.Fprintf(cmd.OutOrStdout(),
			"Remove the registration token from the environment: it is spent.\n")
		return nil
	},
}

var resetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Clear this agent's framework credential so it can enrol again",
	Long: `Remove the stored agent credential so that the agent can enrol with a new
registration token.

The system ID is kept. It is what this system's backup history is filed under,
and re-enrolling an agent must not sever that history.

Revoke the old agent on the framework first ("aiha-framework agent revoke"),
or it stays registered with a token nobody holds.`,
	PreRunE: loadAgentConfig,
	RunE: func(cmd *cobra.Command, _ []string) error {
		existing, err := state.ReadCredential(agentCfg.StateDir)
		switch {
		case errors.Is(err, state.ErrNoCredential):
			fmt.Fprintln(cmd.OutOrStdout(), "No agent credential stored")
			return nil
		case err != nil:
			// A credential that cannot be parsed still has to be removable.
			log.Warn().Err(err).Msg("The stored credential could not be read; clearing it anyway")
		}

		if err := state.ClearCredential(agentCfg.StateDir); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(agentCfg.StateDir, registrationKeyFile)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove the registration key: %w", err)
		}

		if existing.AgentID != "" {
			log.Info().Str("agentId", existing.AgentID).Msg("Agent credential cleared")
			fmt.Fprintf(cmd.OutOrStdout(),
				"Cleared the credential for agent %s. Revoke it on the framework if you have not already.\n",
				existing.AgentID)
			return nil
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Agent credential cleared")
		return nil
	},
}

// errAlreadyEnrolled reports that a credential is already stored.
var errAlreadyEnrolled = errors.New("already enrolled")

// enrol performs the enrolment and stores the credential.
//
// Everything that can fail is checked before the request goes out, because the
// request spends a token that cannot be spent twice: discovering afterwards
// that the credential cannot be written costs the operator a token and leaves
// an agent registered that nobody can authenticate as.
func enrol(ctx context.Context) (state.Credential, error) {
	pin, err := agentCfg.ValidateFramework()
	if err != nil {
		return state.Credential{}, err
	}
	if err := state.EnsureWritable(agentCfg.StateDir); err != nil {
		return state.Credential{}, err
	}

	lock, err := filelock.Acquire(ctx, filepath.Join(agentCfg.StateDir, "enrol.lock"), enrolLockTimeout)
	if err != nil {
		if errors.Is(err, filelock.ErrLocked) {
			return state.Credential{}, errors.New("another agent process is enrolling")
		}
		return state.Credential{}, err
	}
	defer lock.Release()

	if _, err := state.ReadCredential(agentCfg.StateDir); err == nil {
		return state.Credential{}, errAlreadyEnrolled
	} else if !errors.Is(err, state.ErrNoCredential) {
		return state.Credential{}, err
	}

	if agentCfg.RegistrationToken == "" {
		return state.Credential{}, errors.New(
			"a registration token is required: set AIHA_AGENT_REGISTRATION_TOKEN or AIHA_AGENT_REGISTRATION_TOKEN_FILE")
	}

	systemID, err := state.EnsureSystemID(agentCfg.StateDir)
	if err != nil {
		return state.Credential{}, err
	}
	// Recorded before the request, so that a retry of this attempt carries the
	// same key and is recognised rather than enrolling a second agent.
	key, err := loadOrCreateRegistrationKey(agentCfg.StateDir)
	if err != nil {
		return state.Credential{}, err
	}

	client, err := framework.New(agentCfg.FrameworkURL, pin)
	if err != nil {
		return state.Credential{}, err
	}

	hostname, _ := os.Hostname()
	resp, err := client.Register(ctx, model.AgentRegisterReq{
		RegistrationToken: agentCfg.RegistrationToken,
		RegistrationKey:   key,
		SystemID:          systemID,
		Hostname:          hostname,
		AgentVersion:      binaryName + " " + version,
	})
	if err != nil {
		return state.Credential{}, err
	}

	cred := state.Credential{
		AgentID:             resp.AgentID,
		SystemID:            resp.SystemID,
		Token:               resp.AgentToken,
		FrameworkURL:        agentCfg.FrameworkURL,
		FrameworkCertSHA256: agentCfg.FrameworkCertSHA256,
		RegisteredAt:        time.Now().UTC(),
	}
	if err := state.WriteCredential(agentCfg.StateDir, cred); err != nil {
		// The enrolment succeeded on the framework but the credential is lost.
		// Say so plainly: the operator has to revoke the agent there.
		return state.Credential{}, fmt.Errorf(
			"enrolled as agent %s but could not store the credential, so it is lost: "+
				"revoke that agent on the framework and enrol again: %w", resp.AgentID, err)
	}

	log.Info().Str("agentId", cred.AgentID).Str("systemId", cred.SystemID).
		Msg("Enrolled with the framework")
	return cred, nil
}

// loadOrCreateRegistrationKey returns this enrolment attempt's key, creating
// and storing one the first time.
func loadOrCreateRegistrationKey(dir string) (string, error) {
	path := filepath.Join(dir, registrationKeyFile)

	if b, err := os.ReadFile(path); err == nil {
		key := strings.TrimSpace(string(b))
		if !registrationKeyPattern.MatchString(key) {
			return "", fmt.Errorf("the stored registration key in %s is not usable", path)
		}
		return key, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read the registration key %s: %w", path, err)
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate a registration key: %w", err)
	}
	key := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("store the registration key %s: %w", path, err)
	}
	return key, nil
}

func init() {
	rootCmd.AddCommand(registerCmd)
	rootCmd.AddCommand(resetCmd)
}
