package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// version is the framework version; overridden at build time via -ldflags.
var version = "dev"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version",
	Run: func(_ *cobra.Command, _ []string) {
		fmt.Printf("aiha-framework %s\n", version)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
