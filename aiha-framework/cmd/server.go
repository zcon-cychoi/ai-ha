package cmd

import (
	"context"
	"errors"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest"
	"github.com/cloud-barista/ai-ha/pkg/framework/agent"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Run the AI-HA framework HTTPS server",
	Long: `Run the framework server over HTTPS. A TLS certificate and key are required:
rescue agents connect only to a server presenting the certificate whose
fingerprint was injected into their instance. The database schema is applied
before the server starts, so GET /aiha/readyz reports ready only after it.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()

		if cfg.TLSKeyFile == "" {
			return errors.New("AIHA_FRAMEWORK_TLS_KEY is required")
		}
		fingerprint, err := certFingerprint(cfg.TLSCertFile)
		if err != nil {
			return err
		}

		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		// The key is required here: every claim decrypts its repository's
		// credentials, and a server that could not would fail each one.
		repos, jobs, err := newServices(pool, true)
		if err != nil {
			return err
		}
		svc, err := newRestoreService(pool, jobs, repos)
		if err != nil {
			return err
		}
		go runSweeper(ctx, jobs, cfg.JobSweepInterval)

		if cfg.CertPin {
			log.Info().Str("certSha256", fingerprint).Msg("Serving with the pinned certificate")
		} else {
			log.Info().Str("certSha256", fingerprint).Msg("Serving; agents verify the certificate against their CA bundle")
		}
		return rest.Run(ctx, rest.Config{
			Port:          cfg.Port,
			TLSCertFile:   cfg.TLSCertFile,
			TLSKeyFile:    cfg.TLSKeyFile,
			Restore:       svc,
			Agent:         agent.NewService(agent.NewPGStore(pool)),
			Jobs:          jobs,
			Inventory:     newInventoryService(pool),
			CORSOrigins:   cfg.CORSOrigins,
			EnableSwagger: cfg.EnableSwagger,
		})
	},
}

func init() {
	rootCmd.AddCommand(serverCmd)
}

// runSweeper moves on jobs nobody is reporting on, until ctx ends.
//
// It is what makes the timeouts real: without it a job whose agent vanished
// would read "running" forever. A failed sweep is logged and retried on the
// next tick, since the conditions it acts on persist until handled.
func runSweeper(ctx context.Context, jobs *job.Service, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		sweepCtx, cancel := context.WithTimeout(ctx, interval)
		counts, err := jobs.Sweep(sweepCtx)
		cancel()
		if err != nil {
			log.Error().Err(err).Msg("Job sweep failed")
			continue
		}
		if counts.Total() > 0 {
			log.Info().
				Int("queuedExpired", counts.QueuedExpired).
				Int("claimedReturned", counts.ClaimedReturned).
				Int("runningAbandoned", counts.RunningAbandoned).
				Int("cancelUnconfirmed", counts.CancelUnconfirmed).
				Msg("Swept timed-out jobs")
		}
	}
}
