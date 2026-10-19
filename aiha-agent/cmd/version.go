package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// version is the agent version; overridden at build time via -ldflags.
var version = "dev"

// versionDetails supply lines printed after the version. Build-specific files
// register what only their build can report, keeping their imports out of the
// other build.
var versionDetails []func() string

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version",
	Run: func(cmd *cobra.Command, _ []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", binaryName, version)
		for _, detail := range versionDetails {
			fmt.Fprintln(cmd.OutOrStdout(), detail())
		}
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
