//go:build !rescue

package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/cloud-barista/ai-ha/pkg/backup/sysinfo"
	"github.com/spf13/cobra"
)

var sysinfoCmd = &cobra.Command{
	Use:   "sysinfo",
	Short: "Print the system facts a restore would need",
	Long: `Collect and print the system facts that a backup records for a later restore:
the kernel, OS release, firmware mode, block devices, partition tables, mount
table, fstab, network configuration and LVM layout.

This is what goes into a backup's manifest. Run it to check what this system
reports before relying on a backup of it. Items that cannot be collected are
listed under "errors" and do not stop a backup.

The output includes the contents of /etc/netplan, which may hold secrets such
as a wireless passphrase. Treat it as you would those files.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		info, err := sysinfo.Collect(cmd.Context(), nil)
		if err != nil {
			return err
		}

		out, err := json.MarshalIndent(info, "", "  ")
		if err != nil {
			return fmt.Errorf("encode system information: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(out))

		if len(info.Errors) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "\n%d item(s) could not be collected; see \"errors\" above\n", len(info.Errors))
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(sysinfoCmd)
}
