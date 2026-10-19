//go:build !rescue

package cmd

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cloud-barista/ai-ha/config"
	"github.com/cloud-barista/ai-ha/pkg/agent/state"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show this agent's identity and enrolment",
	Long: `Show what this installation is: the system its backups are filed under, the
agent it has enrolled as, and the framework that issued that credential.

The identifiers are what the framework's own commands take, so this is where to
read the systemId for "aiha-framework agent inventory" or the agentId for
"aiha-framework agent revoke".

Nothing here is a secret. The agent's token is stored alongside these values and
is deliberately never printed.`,
	PreRunE: loadAgentConfig,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return printStatus(cmd.OutOrStdout(), agentCfg)
	},
}

// printStatus writes the agent's local state for an operator.
//
// It reports what it can and says what it cannot, rather than stopping at the
// first thing that is missing: a half-enrolled agent is exactly when someone
// runs this, and "no credential" is an answer, not a failure.
func printStatus(out io.Writer, cfg *config.AgentConfig) error {
	systemID, err := state.ReadSystemID(cfg.StateDir)
	switch {
	case errors.Is(err, state.ErrNoSystemID):
		fmt.Fprintln(out, "System:     none yet — assigned on enrolment or the first backup")
	case err != nil:
		return err
	default:
		fmt.Fprintf(out, "System:     %s\n", systemID)
	}

	cred, err := state.ReadCredential(cfg.StateDir)
	switch {
	case errors.Is(err, state.ErrNoCredential):
		fmt.Fprintf(out, "Agent:      not enrolled — run %q\n", binaryName+" register")
	case err != nil:
		return err
	default:
		fmt.Fprintf(out, "Agent:      %s\n", cred.AgentID)
		fmt.Fprintf(out, "Framework:  %s\n", cred.FrameworkURL)
		fmt.Fprintf(out, "Registered: %s\n", cred.RegisteredAt.Format(time.RFC3339))
	}

	fmt.Fprintf(out, "State dir:  %s\n", cfg.StateDir)

	if err == nil {
		// Both of these stop the agent from working, and neither is visible
		// from the framework's side. An operator looking at a silent agent
		// should find the reason here rather than in a log.
		if systemID != "" && cred.SystemID != "" && cred.SystemID != systemID {
			fmt.Fprintf(out,
				"\nThis agent enrolled for system %s, but the stored system id is %s.\n"+
					"Jobs are created for the first and backups filed under the second.\n",
				cred.SystemID, systemID)
		}
		if !cred.BelongsTo(cfg.FrameworkURL, cfg.FrameworkCertSHA256) {
			fmt.Fprintf(out,
				"\nThis credential was issued by %s, but the agent is configured to talk to %s.\n"+
					"It will not start until they agree, or until it is enrolled again with %q.\n",
				cred.FrameworkURL, orNone(cfg.FrameworkURL), binaryName+" reset")
		}
	}

	pending, err := state.ReadRecoveryPending(cfg.StateDir)
	if err != nil {
		return err
	}
	if pending != nil {
		fmt.Fprintf(out,
			"\nRecovery:   awaiting boot verification (restored %s)\n"+
				"This system booted from a backup and will not take work that belongs to the original.\n",
			pending.RestoredAt.Format(time.RFC3339))
	}
	return nil
}

// orNone renders an unset configuration value as something an operator can
// read, so that a blank line is not mistaken for a matching one.
func orNone(s string) string {
	if s == "" {
		return "nothing (AIHA_AGENT_FRAMEWORK_URL is not set)"
	}
	return s
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
