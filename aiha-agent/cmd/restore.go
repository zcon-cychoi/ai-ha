package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/agent/rescue"
	"github.com/cloud-barista/ai-ha/pkg/agent/restore"
	"github.com/spf13/cobra"
)

// defaultRestoreTimeout bounds a whole restore. Most of it is reading the
// backup files from the repository, so it scales with their size.
const defaultRestoreTimeout = 6 * time.Hour

var (
	restoreRecoveryFile string
	restoreOutDir       string
	restoreTimeout      time.Duration
)

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Restore the claimed backup set onto this machine (run inside the rescue environment)",
	Long: `Restore the backup set of a restore job already claimed by "rescue", from the
plan and repository access staged under <out-dir>/<recoveryId>/. "rescue" runs
this itself after claiming; run it by hand to try again after a failure without
a new claim.

THIS FORMATS THE PARTITIONS THE BACKUP SET WAS TAKEN FROM. The target is assumed
to be built from the same image as the source, so each partition is found under
the device path the source recorded. In order:

  1. preparing      read the set's manifest from the repository and check that
                    this machine can take it; nothing is changed if this fails
  2. formatting     recreate each filesystem with the source's label and UUID
  3. mounting       mountall --no-bind (each at /mnt/<device name>)
  4. extracting     rclone cat | decompress | tar -x for each backup file
  5. reconfiguring  point netplan at this machine's MAC address; check that the
                    UUIDs fstab, grub's defaults and the EFI grub stub name are
                    on this machine (a mismatch is a warning, not a failure);
                    leave the recovery marker for the restored agent
  6. bootloader     grub-install, update-grub and update-initramfs in arch-chroot

Progress and the result are reported to the framework. The machine is not
rebooted.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		rec, err := rescue.LoadRecovery(restoreRecoveryFile)
		if err != nil {
			return err
		}
		return runRestore(cmd.Context(), filepath.Join(restoreOutDir, rec.RecoveryID), restoreRecoveryFile, restoreTimeout)
	},
}

// runRestore restores the claim staged in dir, reporting to the framework
// named in recoveryFile.
func runRestore(ctx context.Context, dir, recoveryFile string, timeout time.Duration) error {
	rec, err := rescue.LoadRecovery(recoveryFile)
	if err != nil {
		return err
	}
	staged, err := rescue.LoadStaged(dir)
	if err != nil {
		return err
	}
	if staged.RecoveryID != rec.RecoveryID {
		return fmt.Errorf("%s holds recovery %s, not %s", dir, staged.RecoveryID, rec.RecoveryID)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := restore.Run(ctx, restore.Options{
		Staged:   staged,
		Reporter: rescue.NewReportClient(rec, staged.ReportToken),
	})
	if err != nil {
		return err
	}
	fmt.Printf("Backup set %s restored: %d partition(s), %s boot (%.1fs)\n",
		staged.Plan.SourceBackupSetID, res.Partitions, res.Firmware, res.Elapsed.Seconds())
	fmt.Printf("Restored root: %s (still mounted; the machine was not rebooted)\n", res.Root)
	for _, w := range res.Warnings {
		fmt.Printf("WARNING: %s\n", w)
	}
	return nil
}

func init() {
	restoreCmd.Flags().StringVar(&restoreRecoveryFile, "recovery-file", defaultRecoveryFile, "recovery information left by the instance init script in /autorun")
	restoreCmd.Flags().StringVar(&restoreOutDir, "out-dir", "/run/aiha", "RAM-backed (tmpfs) directory the claim was staged in")
	restoreCmd.Flags().DurationVar(&restoreTimeout, "timeout", defaultRestoreTimeout, "give up on the restore after this long")
	rootCmd.AddCommand(restoreCmd)
}
