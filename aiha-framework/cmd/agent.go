package cmd

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/framework/agent"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var (
	tokenIssueTTL  time.Duration
	tokenIssueUses int
	tokenIssueNote string
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Manage agents and their enrolment",
}

var agentTokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Manage registration tokens",
}

var agentTokenIssueCmd = &cobra.Command{
	Use:   "issue",
	Short: "Issue a registration token",
	Long: `Issue a token that lets an agent enrol, and print the settings for the
agent's environment file: the framework address, how to verify its certificate
(following AIHA_FRAMEWORK_CERT_PIN) and the token. The token authorizes
enrolment and nothing else: it cannot receive work, and an agent's own token
cannot enrol anyone.

The token is printed once and is not recoverable: the framework keeps only its
hash. If the agent never receives its credentials, revoke the incomplete agent
and issue a new token rather than trying to reuse this one.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()

		// Checked before the token is issued, so that a missing setting does
		// not leave behind a token nobody received the settings for.
		publicURL, err := agentURL(cfg.PublicURL)
		if err != nil {
			return err
		}
		certSHA256, err := agentCertSHA256()
		if err != nil {
			return err
		}
		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		svc := agent.NewService(agent.NewPGStore(pool))
		issued, err := svc.IssueRegistrationToken(ctx, tokenIssueTTL, tokenIssueUses, tokenIssueNote)
		if err != nil {
			return err
		}

		// The token itself is never logged, only the record of issuing one.
		log.Info().Str("tokenId", issued.TokenID).
			Str("expiresAt", issued.ExpiresAt.Format(time.RFC3339)).
			Int("maxUses", issued.MaxUses).Msg("Registration token issued")

		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "# Registration token %s (usable until %s, %d use(s))\n",
			issued.TokenID, issued.ExpiresAt.Format(time.RFC3339), issued.MaxUses)
		fmt.Fprintf(out, "# Shown once. Put these in the agent's environment file; remove the token after enrolment.\n")
		printAgentEnv(out, publicURL, cfg.CertPin, certSHA256, issued.Token)
		return nil
	},
}

// printAgentEnv writes the agent's framework settings as environment-file
// lines. The fingerprint line is printed empty when pinning is off, because the
// agent rejects a fingerprint alongside AIHA_AGENT_FRAMEWORK_CERT_PIN=false.
func printAgentEnv(w io.Writer, url string, pin bool, certSHA256, token string) {
	fmt.Fprintf(w, "AIHA_AGENT_FRAMEWORK_URL='%s'\n", url)
	fmt.Fprintf(w, "AIHA_AGENT_FRAMEWORK_CERT_PIN='%t'\n", pin)
	fmt.Fprintf(w, "AIHA_AGENT_FRAMEWORK_CERT_SHA256='%s'\n", certSHA256)
	fmt.Fprintf(w, "AIHA_AGENT_REGISTRATION_TOKEN='%s'\n", token)
}

var agentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List enrolled agents",
	Long: `List enrolled agents with when they were last heard from.

TOKEN and KEY identify the enrolment that produced each agent. They are what
lets an operator find an agent that never received its credentials and so does
not know its own ID.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()

		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		agents, err := agent.NewService(agent.NewPGStore(pool)).ListAgents(ctx)
		if err != nil {
			return err
		}
		if len(agents) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No agents enrolled")
			return nil
		}

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "AGENT\tSYSTEM\tHOST\tSTATE\tLAST SEEN\tTOKEN\tKEY")
		for _, a := range agents {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				a.AgentID, a.SystemID, orDash(a.Hostname), agentState(a),
				lastSeen(a), orDash(a.RegistrationTokenID), orDash(a.RegistrationKey))
		}
		return w.Flush()
	},
}

var agentRevokeCmd = &cobra.Command{
	Use:   "revoke <agentId>",
	Short: "Revoke an agent's token",
	Long: `Revoke an agent's token. It stops working on the agent's next request.

The agent keeps its local credential file, so it cannot simply register again:
clear that file with "aiha-agent reset" on the host, then enrol it with a new
registration token.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		if err := agent.NewService(agent.NewPGStore(pool)).RevokeAgent(ctx, args[0]); err != nil {
			return err
		}
		log.Info().Str("agentId", args[0]).Msg("Agent token revoked")
		fmt.Fprintf(cmd.OutOrStdout(), "Agent %s revoked\n", args[0])
		return nil
	},
}

var agentTokenListCmd = &cobra.Command{
	Use:   "list",
	Short: "List registration tokens",
	Long: `List issued registration tokens and what is left of them. The tokens
themselves are not stored and cannot be listed.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()

		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		tokens, now, err := agent.NewService(agent.NewPGStore(pool)).ListRegistrationTokens(ctx)
		if err != nil {
			return err
		}
		if len(tokens) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No registration tokens issued")
			return nil
		}

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "TOKEN\tSTATE\tUSES\tEXPIRES\tNOTE")
		for _, t := range tokens {
			fmt.Fprintf(w, "%s\t%s\t%d/%d\t%s\t%s\n",
				t.TokenID, tokenState(t, now), t.Uses, t.MaxUses,
				t.ExpiresAt.Format("2006-01-02 15:04:05"), orDash(t.Note))
		}
		return w.Flush()
	},
}

var agentTokenRevokeCmd = &cobra.Command{
	Use:   "revoke <tokenId>",
	Short: "Revoke a registration token",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		if err := agent.NewService(agent.NewPGStore(pool)).RevokeRegistrationToken(ctx, args[0]); err != nil {
			return err
		}
		log.Info().Str("tokenId", args[0]).Msg("Registration token revoked")
		fmt.Fprintf(cmd.OutOrStdout(), "Registration token %s revoked\n", args[0])
		return nil
	},
}

// agentState describes an agent for an operator reading a list.
func agentState(a agent.AgentStatus) string {
	switch {
	case a.RevokedAt != nil:
		return "revoked"
	case a.Online:
		return "online"
	default:
		return "offline"
	}
}

// tokenState describes what is left of a registration token.
func tokenState(t agent.RegistrationToken, now time.Time) string {
	switch {
	case t.RevokedAt != nil:
		return "revoked"
	case t.Uses >= t.MaxUses:
		return "used up"
	case !now.Before(t.ExpiresAt):
		return "expired"
	default:
		return "usable"
	}
}

func lastSeen(a agent.AgentStatus) string {
	if a.LastSeenAt == nil {
		return "never"
	}
	return a.LastSeenAt.Format("2006-01-02 15:04:05")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func init() {
	agentTokenIssueCmd.Flags().DurationVar(&tokenIssueTTL, "ttl", 30*time.Minute, "how long the token may be used to enrol")
	agentTokenIssueCmd.Flags().IntVar(&tokenIssueUses, "uses", 1, "how many agents the token may enrol")
	agentTokenIssueCmd.Flags().StringVar(&tokenIssueNote, "note", "", "a note recorded with the token, e.g. what it was issued for")

	agentTokenCmd.AddCommand(agentTokenIssueCmd)
	agentTokenCmd.AddCommand(agentTokenListCmd)
	agentTokenCmd.AddCommand(agentTokenRevokeCmd)

	agentCmd.AddCommand(agentTokenCmd)
	agentCmd.AddCommand(agentListCmd)
	agentCmd.AddCommand(agentRevokeCmd)
	agentCmd.AddCommand(agentInventoryCmd)
	rootCmd.AddCommand(agentCmd)
}
