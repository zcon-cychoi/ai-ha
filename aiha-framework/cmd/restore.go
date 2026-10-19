package cmd

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/framework/restore"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var restoreBackupSet string

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Manage restore jobs",
}

var restoreCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Register a restore job and print the values for the instance init script",
	Long: `Register a restore job for a backup set and print the variables to set in
sh/aiha-init.sh. How the rescue agent verifies the framework certificate
follows AIHA_FRAMEWORK_CERT_PIN. The claim token is shown only here; the database keeps its
hash.

The set must be one a framework backup job reported as completed: that job says
which system the set captured and which registered repository holds it, and
the rescue agent receives that repository's access when it claims the job. A
set taken by hand with "aiha-agent backup" is not known here and cannot be
restored this way. "Completed" is what the agent reported; whether the files
are still in the repository is found out by the rescue agent when it reads the
set's manifest.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()

		publicURL, err := agentURL(cfg.PublicURL)
		if err != nil {
			return err
		}
		fingerprint, err := agentCertSHA256()
		if err != nil {
			return err
		}
		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		svc, err := restoreServiceFor(pool)
		if err != nil {
			return err
		}
		created, err := svc.Create(ctx, restore.NewJob{BackupSetID: restoreBackupSet})
		if err != nil {
			return err
		}
		log.Info().Str("restoreId", created.RestoreID).Str("recoveryId", created.RecoveryID).
			Time("claimExpiresAt", created.ClaimExpiresAt).Msg("Restore job created")

		fmt.Printf("# sh/aiha-init.sh values for restore %s (first claim before %s)\n",
			created.RestoreID, created.ClaimExpiresAt.Format(time.RFC3339))
		fmt.Printf("AIHA_RECOVERY_ID='%s'\n", created.RecoveryID)
		fmt.Printf("AIHA_FRAMEWORK_URL='%s'\n", publicURL)
		fmt.Printf("AIHA_CLAIM_TOKEN='%s'\n", created.ClaimToken)
		fmt.Printf("AIHA_FRAMEWORK_CERT_PIN='%t'\n", cfg.CertPin)
		fmt.Printf("AIHA_FRAMEWORK_CERT_SHA256='%s'\n", fingerprint)
		return nil
	},
}

var restoreGetCmd = &cobra.Command{
	Use:   "get <restoreId|recoveryId>",
	Short: "Show a restore job and the progress its agent reported",
	Long: `Show a restore job: its status, the step the rescue agent is on and, while
backup files are restored, how much of them has been read from the repository.

Either ID finds the job: the restore ID "restore create" printed, or the
recovery ID set in the instance's init script and shown in the rescue log.

A status of awaiting_boot means the disk was restored; the restored system's
boot has not been confirmed yet.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		svc, err := restoreServiceFor(pool)
		if err != nil {
			return err
		}
		j, err := svc.Get(ctx, args[0])
		if err != nil {
			return err
		}

		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Restore:        %s\n", j.RestoreID)
		fmt.Fprintf(out, "Recovery:       %s\n", j.RecoveryID)
		fmt.Fprintf(out, "Status:         %s\n", j.Status)
		fmt.Fprintf(out, "Plan:           %s\n", string(j.Plan))
		fmt.Fprintf(out, "Created:        %s\n", j.CreatedAt.Format(time.RFC3339))
		if j.ClaimedAt != nil {
			fmt.Fprintf(out, "Claimed:        %s\n", j.ClaimedAt.Format(time.RFC3339))
		}

		p := j.Progress
		if p.Seq == 0 && j.Outcome == "" {
			fmt.Fprintln(out, "Progress:       none reported yet")
			return nil
		}
		if p.TotalSteps > 0 {
			fmt.Fprintf(out, "Phase:          %s (step %d of %d)\n", orDash(p.Phase), p.Step, p.TotalSteps)
		} else {
			fmt.Fprintf(out, "Phase:          %s\n", orDash(p.Phase))
		}
		if p.TotalBytes > 0 {
			fmt.Fprintf(out, "Restored:       %d of %d bytes (%.1f%%)\n",
				p.ProcessedBytes, p.TotalBytes, 100*float64(p.ProcessedBytes)/float64(p.TotalBytes))
		}
		if p.Item != "" {
			fmt.Fprintf(out, "Current file:   %s\n", p.Item)
		}
		if j.LastProgressAt != nil {
			fmt.Fprintf(out, "Last progress:  %s (report %d)\n", j.LastProgressAt.Format(time.RFC3339), p.Seq)
		}
		if j.Outcome != "" {
			fmt.Fprintf(out, "Outcome:        %s\n", j.Outcome)
		}
		if j.FinishedAt != nil {
			fmt.Fprintf(out, "Finished:       %s\n", j.FinishedAt.Format(time.RFC3339))
		}
		if j.Message != "" {
			fmt.Fprintf(out, "Message:        %s\n", j.Message)
		}
		return nil
	},
}

// agentURL validates the public URL agents use to reach the framework. It is
// printed inside single quotes for a shell script, so quoting characters and
// whitespace are rejected.
func agentURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("AIHA_FRAMEWORK_PUBLIC_URL is required: it is the address agents reach the framework at")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || strings.ContainsAny(raw, "'\"\\ \t\r\n") {
		return "", errors.New("AIHA_FRAMEWORK_PUBLIC_URL must be an https URL such as https://host:port")
	}
	return strings.TrimRight(raw, "/"), nil
}

// restoreServiceFor returns the restore service for the CLI. It needs no
// secret key: creating a job records the repository's ID, and only the server
// decrypts credentials, for each claim.
func restoreServiceFor(pool *pgxpool.Pool) (*restore.Service, error) {
	repos, jobs, err := newServices(pool, false)
	if err != nil {
		return nil, err
	}
	return newRestoreService(pool, jobs, repos)
}

func init() {
	restoreCreateCmd.Flags().StringVar(&restoreBackupSet, "backup-set", "", "backup set ID to restore (required); see \"aiha-framework job get <jobId>\"")
	_ = restoreCreateCmd.MarkFlagRequired("backup-set")
	restoreCmd.AddCommand(restoreCreateCmd)
	restoreCmd.AddCommand(restoreGetCmd)
	rootCmd.AddCommand(restoreCmd)
}
