// Package cmd implements the aiha-framework command-line interface.
package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/cloud-barista/ai-ha/config"
	"github.com/cloud-barista/ai-ha/pkg/logging"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var (
	flagLogLevel string
	cfg          *config.FrameworkConfig
)

// rootCmd is the base command.
var rootCmd = &cobra.Command{
	Use:   "aiha-framework",
	Short: "AI-HA framework server",
	Long: `aiha-framework is the central AI-HA server. It registers restore jobs and
hands each rescue agent its restore plan and repository access once the agent
presents the job's claim token. Configuration comes from AIHA_FRAMEWORK_*
environment variables; see conf/template-setup.env.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		loaded, err := config.LoadFramework()
		if err != nil {
			return err
		}
		cfg = loaded
		if flagLogLevel != "" {
			cfg.LogLevel = flagLogLevel
		}
		logging.Setup(cfg.LogLevel)
		return nil
	},
}

// Execute runs the root command. It is the CLI entry point.
func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		log.Error().Err(err).Msg("Command failed")
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&flagLogLevel, "log-level", "", "log level: debug, info, warn, error")
}
