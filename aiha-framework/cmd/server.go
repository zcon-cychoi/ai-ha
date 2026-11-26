package cmd

import (
	"context"
	"errors"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest"
	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/agent"
	"github.com/cloud-barista/ai-ha/pkg/framework/ha"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/cloud-barista/ai-ha/pkg/framework/token"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Run the AI-HA framework HTTPS server",
	Long: `Run the framework server over HTTPS. A TLS certificate and key are required:
rescue agents connect only to a server presenting the certificate whose
fingerprint was injected into their instance. The database schema is applied
before the server starts, so GET /aiha/readyz reports ready only after it.

The management API (systems, backups, restores, jobs, repositories, agents) is
served only when AIHA_FRAMEWORK_ADMIN_TOKEN_FILE is set, which also requires
AIHA_FRAMEWORK_PUBLIC_URL. Agents are served either way.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()

		if cfg.TLSKeyFile == "" {
			return errors.New("AIHA_FRAMEWORK_TLS_KEY is required")
		}
		fingerprint, err := certFingerprint(cfg.TLSCertFile)
		if err != nil {
			return err
		}

		// Checked before anything starts, so that a setting that would break
		// the management API stops the server rather than failing requests.
		adminAuth, access, err := managementAccess()
		if err != nil {
			return err
		}

		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		// The key is required here: every claim decrypts its repository's
		// credentials or its HA group's shared secret, and a server that
		// could not would fail each one.
		repos, jobs, flows, err := newServices(pool, true)
		if err != nil {
			return err
		}
		svc, err := newRestoreService(pool, jobs, repos)
		if err != nil {
			return err
		}
		haSvc, err := newHAService(pool)
		if err != nil {
			return err
		}
		go runSweeper(ctx, jobs, cfg.JobSweepInterval)
		go runHAEvaluator(ctx, haSvc, flows, cfg.HAReportInterval)

		if cfg.CertPin {
			log.Info().Str("certSha256", fingerprint).Msg("Serving with the pinned certificate")
		} else {
			log.Info().Str("certSha256", fingerprint).Msg("Serving; agents verify the certificate against their CA bundle")
		}
		return rest.Run(ctx, rest.Config{
			Port:            cfg.Port,
			TLSCertFile:     cfg.TLSCertFile,
			TLSKeyFile:      cfg.TLSKeyFile,
			Restore:         svc,
			Agent:           agent.NewService(agent.NewPGStore(pool)),
			Jobs:            jobs,
			Inventory:       newInventoryService(pool),
			HA:              haSvc,
			Repositories:    repos,
			AdminAuth:       adminAuth,
			FrameworkAccess: access,
			CORSOrigins:     cfg.CORSOrigins,
			EnableSwagger:   cfg.EnableSwagger,
		})
	},
}

func init() {
	rootCmd.AddCommand(serverCmd)
}

// managementAccess returns what the management API needs: how to authenticate
// its callers and what to tell them about reaching the framework. With no admin
// token configured it returns neither, which leaves that API closed.
//
// The public URL is required alongside the token: the API hands it out with
// every registration token and restore job, and one without it would issue
// credentials nobody could use.
func managementAccess() (rest.AdminAuthenticator, model.FrameworkAccess, error) {
	if cfg.AdminTokenFile == "" {
		log.Info().Msg("AIHA_FRAMEWORK_ADMIN_TOKEN_FILE is not set: the management API is closed")
		return nil, model.FrameworkAccess{}, nil
	}
	hash, err := token.LoadFile(cfg.AdminTokenFile)
	if err != nil {
		return nil, model.FrameworkAccess{}, err
	}
	publicURL, err := agentURL(cfg.PublicURL)
	if err != nil {
		return nil, model.FrameworkAccess{}, err
	}
	certSHA256, err := agentCertSHA256()
	if err != nil {
		return nil, model.FrameworkAccess{}, err
	}
	return rest.StaticAdminToken(hash),
		model.FrameworkAccess{URL: publicURL, CertPin: cfg.CertPin, CertSHA256: certSHA256}, nil
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
				Int("claimedExpired", counts.ClaimedExpired).
				Int("runningAbandoned", counts.RunningAbandoned).
				Int("cancelUnconfirmed", counts.CancelUnconfirmed).
				Msg("Swept timed-out jobs")
		}
	}
}

// runHAEvaluator moves the HA flows on and evaluates every HA group on its
// own, until ctx ends.
//
// A node that stops reporting sends nothing that could trigger an evaluation,
// so without this a dead primary would keep its last verdict forever. A flow
// moves on only here, too: a step's job completing is not enough, the node's
// next report has to agree, and neither event is the trigger on its own. The
// flows go first so that a group whose setup just completed is judged in the
// same round. A failed round is logged and retried on the next tick: the
// conditions it judges persist until judged.
func runHAEvaluator(ctx context.Context, svc *ha.Service, flows *ha.FlowService, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		roundCtx, cancel := context.WithTimeout(ctx, interval)
		if advanced, err := flows.AdvanceAll(roundCtx); err != nil {
			log.Error().Err(err).Int("advanced", advanced).Msg("HA operations could not all be advanced")
		}
		evaluated, err := svc.EvaluateAll(roundCtx)
		cancel()
		if err != nil {
			log.Error().Err(err).Int("evaluated", evaluated).Msg("HA evaluation failed")
		}
	}
}
