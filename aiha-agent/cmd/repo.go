//go:build !rescue

package cmd

import (
	"fmt"
	"text/tabwriter"

	"github.com/cloud-barista/ai-ha/pkg/agent/state"
	"github.com/cloud-barista/ai-ha/pkg/backup/engine"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/spf13/cobra"
)

var repoListSystemID string

var repoCmd = &cobra.Command{
	Use:   "repo",
	Short: "Inspect the backup repository",
}

var repoSetsCmd = &cobra.Command{
	Use:   "sets",
	Short: "List the backup sets of a system",
	Long: `List a system's backup sets, newest first. A set is one machine captured at
one point in time: the partitions that have to be restored together.`,
	PreRunE: loadConfig,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()

		systemID, err := resolveSystemID(repoListSystemID)
		if err != nil {
			return err
		}

		repo, err := openRepository(ctx)
		if err != nil {
			return err
		}
		defer repo.Close()

		sets, err := engine.New(repo).ListSets(ctx, systemID)
		if err != nil {
			return fmt.Errorf("list backup sets failed: %w", err)
		}
		if len(sets) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No backup sets found")
			return nil
		}

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "CREATED\tSET\tITEMS\tSIZE\tSTATUS\tMANIFEST")
		for _, s := range sets {
			var size int64
			for _, item := range s.Items {
				size += item.SizeBytes
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\n",
				s.CreatedAt.Format("2006-01-02 15:04:05"), s.ID, len(s.Items),
				size, s.Status, model.ManifestName(s.ID))
		}
		return w.Flush()
	},
}

var repoListCmd = &cobra.Command{
	Use:     "list",
	Short:   "List the individual backups of a system",
	PreRunE: loadConfig,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()

		systemID, err := resolveSystemID(repoListSystemID)
		if err != nil {
			return err
		}

		repo, err := openRepository(ctx)
		if err != nil {
			return err
		}
		defer repo.Close()

		records, err := engine.New(repo).List(ctx, systemID)
		if err != nil {
			return fmt.Errorf("list backups failed: %w", err)
		}
		if len(records) == 0 {
			fmt.Println("No backups found")
			return nil
		}

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "CREATED\tPARTITION\tFILE\tSIZE\tSTATUS")
		for _, r := range records {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n",
				r.CreatedAt.Format("2006-01-02 15:04:05"), r.Partition, r.FileName, r.SizeBytes, r.Status)
		}
		return w.Flush()
	},
}

func init() {
	const systemUsage = "systemId to list (default: this agent's own, as \"status\" shows it)"
	repoListCmd.Flags().StringVar(&repoListSystemID, "system", "", systemUsage)
	repoSetsCmd.Flags().StringVar(&repoListSystemID, "system", "", systemUsage)
	repoCmd.PersistentFlags().StringVar(&backupStateDir, "state-dir", state.Dir,
		"directory holding the agent's local state")
	repoCmd.AddCommand(repoListCmd)
	repoCmd.AddCommand(repoSetsCmd)
	addRepoFlag(repoCmd)
	rootCmd.AddCommand(repoCmd)
}
