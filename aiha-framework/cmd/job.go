package cmd

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var (
	jobCreateSystem      string
	jobCreateDevices     []string
	jobCreateCompression string
	jobCreateRepository  string
	jobListSystem        string
	jobListLimit         int
)

var jobCmd = &cobra.Command{
	Use:   "job",
	Short: "Manage backup jobs",
}

var jobBackupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Ask a system's agent to take a backup",
	Long: `Queue a backup job for a system. Its agent picks it up on its next long poll,
claims it and reports as it goes.

The set is written to the registered repository named by --repository (see
"aiha-framework repository list"). The job keeps only the repository's ID; its
agent receives the repository's address and credentials when it claims the job,
decrypted for that response alone. The repository configured on the agent's
host is not used.

With no --device, the partitions are taken from what the system's agent
reported (see "aiha-framework agent inventory"), and the ones chosen are
printed before the job is queued. Pass --device once per partition to name them
yourself; they are checked against the same report, so a device that is not
there is refused now rather than when an agent picks the job up.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()

		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		_, jobs, err := newServices(pool, false)
		if err != nil {
			return err
		}

		devices, expected, err := backupTargets(ctx, cmd, newInventoryService(pool))
		if err != nil {
			return err
		}

		created, err := jobs.CreateBackup(ctx, jobCreateSystem, job.BackupSpec{
			Devices:         devices,
			Compression:     jobCreateCompression,
			ExpectedFSUUIDs: expected,
			RepositoryID:    jobCreateRepository,
		}, "cli")
		if err != nil {
			return err
		}

		log.Info().Str("jobId", created.JobID).Str("systemId", created.SystemID).
			Msg("Backup job queued")
		fmt.Fprintf(cmd.OutOrStdout(), "Queued backup job %s for system %s (expires %s)\n",
			created.JobID, created.SystemID, created.ExpiresAt.Format(time.RFC3339))
		return nil
	},
}

var jobListCmd = &cobra.Command{
	Use:   "list",
	Short: "List jobs, newest first",
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()

		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		_, jobs, err := newServices(pool, false)
		if err != nil {
			return err
		}
		list, err := jobs.List(ctx, jobListSystem, jobListLimit)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No jobs")
			return nil
		}

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "JOB\tSYSTEM\tKIND\tSTATUS\tCREATED\tMESSAGE")
		for _, j := range list {
			status := string(j.Status)
			if j.CancelRequested && !j.Status.Terminal() {
				status += " (cancel requested)"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				j.JobID, j.SystemID, j.Kind, status,
				j.CreatedAt.Format("2006-01-02 15:04:05"), orDash(j.Message))
		}
		return w.Flush()
	},
}

var jobGetCmd = &cobra.Command{
	Use:   "get <jobId>",
	Short: "Show a job and its current attempt",
	Long: `Show a job and the attempt carrying it out.

LAST SEEN and LAST PROGRESS are reported separately on purpose. An agent that is
seen but not progressing is alive and stuck; one that is not seen has lost
contact. They call for different responses.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		_, jobs, err := newServices(pool, false)
		if err != nil {
			return err
		}
		j, attempt, err := jobs.Get(ctx, args[0])
		if err != nil {
			return err
		}

		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Job:        %s\n", j.JobID)
		fmt.Fprintf(out, "System:     %s\n", j.SystemID)
		fmt.Fprintf(out, "Kind:       %s\n", j.Kind)
		fmt.Fprintf(out, "Status:     %s\n", j.Status)
		if j.CancelRequested {
			fmt.Fprintf(out, "Cancel:     requested\n")
		}
		fmt.Fprintf(out, "Spec:       %s\n", string(j.Spec))
		fmt.Fprintf(out, "Created:    %s\n", j.CreatedAt.Format(time.RFC3339))
		if j.Message != "" {
			fmt.Fprintf(out, "Message:    %s\n", j.Message)
		}

		if attempt == nil {
			fmt.Fprintln(out, "Attempt:    none yet")
			return nil
		}
		fmt.Fprintf(out, "\nAttempt:        %s (agent %s)\n", attempt.AttemptID, attempt.AgentID)
		fmt.Fprintf(out, "Phase:          %s\n", orDash(attempt.Phase))
		fmt.Fprintf(out, "Uploaded:       %d bytes (report %d)\n", attempt.ProcessedBytes, attempt.LastSeq)
		fmt.Fprintf(out, "Last seen:      %s\n", attempt.LastSeenAt.Format(time.RFC3339))
		if attempt.LastProgressAt != nil {
			fmt.Fprintf(out, "Last progress:  %s\n", attempt.LastProgressAt.Format(time.RFC3339))
		} else {
			fmt.Fprintln(out, "Last progress:  none yet")
		}
		fmt.Fprintf(out, "Lease expires:  %s\n", attempt.LeaseExpiresAt.Format(time.RFC3339))
		if attempt.Outcome != "" {
			fmt.Fprintf(out, "Outcome:        %s\n", attempt.Outcome)
		}
		return nil
	},
}

var jobCancelCmd = &cobra.Command{
	Use:   "cancel <jobId>",
	Short: "Ask a job to stop",
	Long: `Ask a job to stop.

A job still in the queue is cancelled at once. A job an agent is running is
only marked: the agent learns of it on its next report and has to confirm it
stopped. Until it does, the job stays "cancelling" — a cancel request is not a
stop.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		_, jobs, err := newServices(pool, false)
		if err != nil {
			return err
		}
		j, err := jobs.RequestCancel(ctx, args[0])
		if err != nil {
			return err
		}

		log.Info().Str("jobId", j.JobID).Str("status", string(j.Status)).Msg("Job cancel requested")
		switch j.Status {
		case job.StatusCancelled:
			fmt.Fprintf(cmd.OutOrStdout(), "Job %s cancelled\n", j.JobID)
		case job.StatusCancelling:
			fmt.Fprintf(cmd.OutOrStdout(),
				"Job %s marked for cancellation; waiting for its agent to confirm it stopped\n", j.JobID)
		default:
			fmt.Fprintf(cmd.OutOrStdout(), "Job %s is already %s\n", j.JobID, j.Status)
		}
		return nil
	},
}

func init() {
	jobBackupCmd.Flags().StringVar(&jobCreateSystem, "system", "", "system ID to back up (required); see \"aiha-framework agent list\"")
	jobBackupCmd.Flags().StringArrayVar(&jobCreateDevices, "device", nil, "partition to back up (repeat for each; omit to use what the system's agent reported)")
	jobBackupCmd.Flags().StringVar(&jobCreateCompression, "compression", "", "compression codec: zstd (default), none")
	jobBackupCmd.Flags().StringVar(&jobCreateRepository, "repository", "", "repository ID or name to write the set to (required); see \"aiha-framework repository list\"")
	_ = jobBackupCmd.MarkFlagRequired("system")
	_ = jobBackupCmd.MarkFlagRequired("repository")

	jobListCmd.Flags().StringVar(&jobListSystem, "system", "", "only this system's jobs")
	jobListCmd.Flags().IntVar(&jobListLimit, "limit", 50, "how many jobs to show")

	jobCmd.AddCommand(jobBackupCmd)
	jobCmd.AddCommand(jobListCmd)
	jobCmd.AddCommand(jobGetCmd)
	jobCmd.AddCommand(jobCancelCmd)
	rootCmd.AddCommand(jobCmd)
}
