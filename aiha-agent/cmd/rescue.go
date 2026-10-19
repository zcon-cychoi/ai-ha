package cmd

import (
	"fmt"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/agent/rescue"
	"github.com/spf13/cobra"
)

// defaultRecoveryFile is where the instance init script leaves the recovery
// information: inside /autorun on the boot medium, which SystemRescue's
// copytoram brings into RAM. Reading it there means the disk being restored is
// never mounted.
const defaultRecoveryFile = "/run/archiso/bootmnt/autorun/aiha/recovery.env"

var (
	rescueRecoveryFile   string
	rescueOutDir         string
	rescueTimeout        time.Duration
	rescueRestoreTimeout time.Duration
)

var rescueCmd = &cobra.Command{
	Use:   "rescue",
	Short: "Claim this instance's restore job and restore it (run inside the rescue environment)",
	Long: `Claim this instance's restore job from the framework, stage the restore plan
and repository access under <out-dir>/<recoveryId>/, then restore the backup set
onto this machine's disks (see "restore" for what that does). The output
directory must be on tmpfs because the staged rclone configuration and report
token are secrets. Rerunning after a failure retries the claim with the same
request key and restores again from the start.

The recovery information comes from /autorun on the boot medium, where the
instance init script left it and copytoram brought it into RAM, so the disk
being restored is never mounted. SystemRescue runs this through /autorun/autorun2
after boot; run it by hand with the same flags when investigating.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		res, err := rescue.Run(cmd.Context(), rescue.Options{
			RecoveryFile: rescueRecoveryFile,
			OutDir:       rescueOutDir,
			Timeout:      rescueTimeout,
		})
		if err != nil {
			return err
		}
		fmt.Printf("Restore job claimed: restore %s (recovery %s)\n", res.RestoreID, res.RecoveryID)
		fmt.Printf("Repository:    %s\n", res.Repository)
		fmt.Printf("Restore plan:  %s\n", res.PlanFile)
		fmt.Printf("rclone config: %s (remote %q)\n", res.RcloneFile, rescue.RcloneRemote)

		return runRestore(cmd.Context(), res.Dir, rescueRecoveryFile, rescueRestoreTimeout)
	},
}

func init() {
	rescueCmd.Flags().StringVar(&rescueRecoveryFile, "recovery-file", defaultRecoveryFile, "recovery information left by the instance init script in /autorun")
	rescueCmd.Flags().StringVar(&rescueOutDir, "out-dir", "/run/aiha", "RAM-backed (tmpfs) directory for claim state and results")
	rescueCmd.Flags().DurationVar(&rescueTimeout, "timeout", 30*time.Minute, "give up on the claim after this long, retries included")
	rescueCmd.Flags().DurationVar(&rescueRestoreTimeout, "restore-timeout", defaultRestoreTimeout, "give up on the restore after this long")
	rootCmd.AddCommand(rescueCmd)
}
