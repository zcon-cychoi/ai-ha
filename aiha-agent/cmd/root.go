// Package cmd implements the aiha-agent command-line interface.
//
// The package builds two binaries. Without build tags it is the full agent,
// which backs up partitions and talks to the repository. With "-tags rescue"
// the backup and repository commands are left out, producing the small binary
// the rescue environment downloads into RAM.
package cmd

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/cloud-barista/ai-ha/pkg/logging"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var flagLogLevel string

// rootCmd is the base command.
var rootCmd = &cobra.Command{
	Use:           binaryName,
	Short:         rootShort,
	Long:          rootLong,
	SilenceUsage:  true,
	SilenceErrors: true,
	// Only logging is set up here. Configuration is loaded by the commands
	// that need it, so that "rescue" never reads the repository credentials in
	// AIHA_BACKUP_*: an agent that only has to reach the framework has no
	// business holding them.
	PersistentPreRun: func(*cobra.Command, []string) {
		logging.Setup(flagLogLevel)
	},
}

// exitCoder is an error that names the status the process should end with.
// It lets a command distinguish a condition that will not improve by running
// again from an ordinary failure, so that a service manager can stop
// restarting the process.
type exitCoder interface {
	error
	ExitCode() int
}

// Execute runs the root command. It is the CLI entry point.
func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err := rootCmd.ExecuteContext(ctx)
	if err == nil {
		return
	}

	log.Error().Err(err).Msg("Command failed")

	var coded exitCoder
	if errors.As(err, &coded) {
		os.Exit(coded.ExitCode())
	}
	os.Exit(1)
}

func init() {
	rootCmd.PersistentFlags().StringVar(&flagLogLevel, "log-level", "", "log level: debug, info, warn, error")
}
