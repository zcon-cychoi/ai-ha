//go:build !rescue

package cmd

import (
	"fmt"

	"github.com/cloud-barista/ai-ha/pkg/agent/state"
	"github.com/cloud-barista/ai-ha/pkg/backup/engine"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/spf13/cobra"
)

var (
	extractSystemID    string
	extractFile        string
	extractDest        string
	extractCompression string
)

// extractCmd was "aiha-backup restore". It is named for what it does: unpack
// an archive into a directory. Restoring a system is a different operation,
// carried out from the rescue environment, and the name is left free for it.
var extractCmd = &cobra.Command{
	Use:   "extract",
	Short: "Extract a backup archive into a target directory",
	Long: `Extract a backup archive from the repository into a directory. This unpacks
the archive and nothing more: it does not create partitions, rewrite fstab or
install a bootloader. Restoring a bootable system is done from the rescue
environment.`,
	PreRunE: loadConfig,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()

		systemID, err := resolveSystemID(extractSystemID)
		if err != nil {
			return err
		}

		repo, err := openRepository(ctx)
		if err != nil {
			return err
		}
		defer repo.Close()

		req := model.RestoreRequest{
			SystemID:    systemID,
			FileName:    extractFile,
			Compression: model.CompressionType(extractCompression),
			Dest:        extractDest,
		}
		if err := engine.New(repo).Restore(ctx, req); err != nil {
			return fmt.Errorf("extract failed: %w", err)
		}

		fmt.Printf("Extract completed: %s -> %s\n", extractFile, extractDest)
		return nil
	},
}

func init() {
	extractCmd.Flags().StringVar(&extractSystemID, "system", "", "systemId the backup belongs to (default: this agent's own)")
	extractCmd.Flags().StringVar(&extractFile, "file", "", "backup file name to extract (required)")
	extractCmd.Flags().StringVar(&extractDest, "dest", "", "directory to extract the backup into (required)")
	extractCmd.Flags().StringVar(&extractCompression, "compression", "", "compression codec of the backup: zstd (default), none")
	extractCmd.Flags().StringVar(&backupStateDir, "state-dir", state.Dir, "directory holding the agent's local state")
	_ = extractCmd.MarkFlagRequired("file")
	_ = extractCmd.MarkFlagRequired("dest")
	addRepoFlag(extractCmd)
	rootCmd.AddCommand(extractCmd)
}
