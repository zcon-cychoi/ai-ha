//go:build !rescue

package cmd

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/agent/framework"
	"github.com/cloud-barista/ai-ha/pkg/agent/state"
	"github.com/cloud-barista/ai-ha/pkg/agent/worker"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

// Backoff for a framework that cannot be reached. A poll that ends empty is
// not a failure and never backs off; only an unreachable or overloaded
// framework does.
const (
	backoffBase = time.Second
	backoffMax  = 60 * time.Second
)

var runEnrolIfNeeded bool

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the agent, waiting for work from the framework",
	Long: `Run the agent as a long-lived process. It waits for work by long-polling the
framework, which is also what reports the agent alive. Jobs it receives are
claimed and run one at a time.

Reaching the framework is what marks this agent online. A poll that ends with
nothing to do is the ordinary idle outcome and is not retried or backed off;
only an unreachable framework is.

The process exits with a distinct status for conditions that will not improve
by running again — a revoked credential, a system awaiting boot verification,
a configuration error — so that a service manager can stop restarting it.`,
	// run executes backups, so unlike register and reset it needs the
	// repository configuration as well as the framework's.
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if err := loadAgentConfig(cmd, args); err != nil {
			return err
		}
		return loadConfig(cmd, args)
	},
	RunE: func(cmd *cobra.Command, _ []string) error {
		return run(cmd.Context())
	},
}

// run drives the agent until the context ends or a permanent condition stops
// it. Permanent conditions are reported through the process exit status.
func run(ctx context.Context) error {
	// A system restored from a backup holds the original's identity. Letting
	// it take work would have it act as a second copy of a system that may
	// still be running.
	if pending, err := state.ReadRecoveryPending(agentCfg.StateDir); err != nil {
		return exitWith(exitConfiguration, err)
	} else if pending != nil {
		// Never log the marker's boot-verify token.
		return exitWith(exitRecoveryPending, fmt.Errorf(
			"this system was restored (recovery %s) and its boot is not verified yet, "+
				"so it still holds the original system's identity: complete registration first",
			pending.RecoveryID))
	}

	pin, err := agentCfg.ValidateFramework()
	if err != nil {
		return exitWith(exitConfiguration, err)
	}
	wait, err := agentCfg.ResolvedPollWait()
	if err != nil {
		return exitWith(exitConfiguration, err)
	}
	inventoryEvery, err := agentCfg.ResolvedInventoryInterval()
	if err != nil {
		return exitWith(exitConfiguration, err)
	}

	cred, err := credentialFor(ctx)
	if err != nil {
		return err
	}

	client, err := framework.New(agentCfg.FrameworkURL, pin)
	if err != nil {
		return exitWith(exitConfiguration, err)
	}

	log.Info().Str("agentId", cred.AgentID).Str("systemId", cred.SystemID).
		Dur("pollWait", wait).Dur("inventoryInterval", inventoryEvery).Msg("Agent running")

	hostname, _ := os.Hostname()
	w := worker.New(client, cred.Token, backupRunner{
		// The id the framework enrolled this agent under, which is the one
		// its jobs are created for.
		system:   model.SystemInfo{ID: cred.SystemID, Hostname: hostname},
		stateDir: agentCfg.StateDir,
	}, agentCfg.StateDir)

	// Reporting runs beside the poll rather than inside it: reading the disks
	// takes long enough that doing it between polls would make an agent look
	// absent. Its context is cancelled when the poll returns for any reason,
	// so the reporter never outlives the agent's work.
	reportCtx, stopReporting := context.WithCancel(ctx)
	defer stopReporting()

	var reporting sync.WaitGroup
	reporting.Add(1)
	go func() {
		defer reporting.Done()
		reportInventory(reportCtx, client, readSystem, cred.Token, inventoryEvery)
	}()

	err = poll(ctx, client, w, cred.Token, wait)
	stopReporting()
	reporting.Wait()
	return err
}

// credentialFor returns the stored credential, enrolling first when asked to
// and when none is stored.
func credentialFor(ctx context.Context) (state.Credential, error) {
	cred, err := state.ReadCredential(agentCfg.StateDir)
	switch {
	case err == nil:
	case errors.Is(err, state.ErrNoCredential):
		if !runEnrolIfNeeded {
			return state.Credential{}, exitWith(exitReenrolNeeded, errors.New(
				"this agent is not enrolled: run \"aiha-agent register\", "+
					"or pass --enrol to enrol on startup"))
		}
		if cred, err = enrol(ctx); err != nil {
			return state.Credential{}, exitWith(exitReenrolNeeded, err)
		}
	default:
		return state.Credential{}, exitWith(exitConfiguration, err)
	}

	// A credential issued by one framework must not be presented to another.
	// Without this check, repointing the agent would send another framework's
	// token to a server that never issued it.
	if !cred.BelongsTo(agentCfg.FrameworkURL, agentCfg.FrameworkCertSHA256) {
		return state.Credential{}, exitWith(exitConfiguration, fmt.Errorf(
			"the stored credential was issued by %s, but this agent is configured for %s: "+
				"run \"aiha-agent reset\" and enrol with the framework you mean to use",
			cred.FrameworkURL, agentCfg.FrameworkURL))
	}
	return cred, nil
}

// poll waits for work until the context ends, running each job it receives.
func poll(ctx context.Context, client *framework.Client, w *worker.Worker, agentToken string, wait time.Duration) error {
	delay := backoffBase

	for {
		result, err := client.Poll(ctx, agentToken, wait)
		switch {
		case err == nil && result.HasWork:
			delay = backoffBase
			// One job at a time. Receiving it grants nothing; Execute claims
			// it and does no work unless the claim succeeds.
			if jobErr := w.Execute(ctx, result.Job); jobErr != nil {
				if permanent := permanentJobError(jobErr); permanent != nil {
					return permanent
				}
				log.Error().Err(jobErr).Str("jobId", result.Job.JobID).Msg("Job did not complete")
			}
			continue

		case err == nil:
			// The wait ended with nothing to do. Poll again at once: this is
			// the idle path, and delaying it would make the agent look absent.
			delay = backoffBase
			continue

		case ctx.Err() != nil:
			log.Info().Msg("Agent stopping")
			return nil

		case errors.Is(err, framework.ErrUnauthorized):
			return exitWith(exitReenrolNeeded, fmt.Errorf(
				"%w: revoke this agent on the framework if you have not already, "+
					"then run \"aiha-agent reset\" and enrol with a new registration token", err))

		case errors.Is(err, framework.ErrPinMismatch), errors.Is(err, framework.ErrRedirect):
			// Neither improves by waiting: both mean the agent is not talking
			// to the framework it was configured for.
			return exitWith(exitConfiguration, err)

		case framework.Retryable(err):
			log.Warn().Err(err).Dur("retryIn", delay).Msg("Could not reach the framework")
			if !sleepOrDone(ctx, jitter(delay)) {
				log.Info().Msg("Agent stopping")
				return nil
			}
			delay = min(2*delay, backoffMax)

		default:
			return exitWith(exitConfiguration, err)
		}
	}
}

// permanentJobError reports whether a job's failure means the agent itself can
// no longer work. A backup failing is the job's problem, and the agent moves on
// to the next one; a revoked credential is the agent's, and it has to stop.
func permanentJobError(err error) error {
	if errors.Is(err, framework.ErrUnauthorized) {
		return exitWith(exitReenrolNeeded, fmt.Errorf(
			"%w: revoke this agent on the framework if you have not already, "+
				"then run \"aiha-agent reset\" and enrol with a new registration token", err))
	}
	return nil
}

// jitter spreads retries so that a fleet restarting together does not return
// in lockstep. It is the same half-jitter the rescue client uses.
func jitter(d time.Duration) time.Duration {
	return d/2 + rand.N(d/2+1)
}

// sleepOrDone waits for d, reporting false if the context ended first.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// exitError carries a status code for a condition that will not improve by
// running again, so that a service manager can stop restarting the process.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// ExitCode implements the interface Execute uses to choose a status.
func (e *exitError) ExitCode() int { return e.code }

func exitWith(code int, err error) error {
	return &exitError{code: code, err: err}
}

func init() {
	runCmd.Flags().BoolVar(&runEnrolIfNeeded, "enrol", false,
		"enrol on startup when no credential is stored and a registration token is set")
	rootCmd.AddCommand(runCmd)
}
