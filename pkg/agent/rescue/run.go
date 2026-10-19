package rescue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/filelock"
	"github.com/rs/zerolog/log"
)

// Options configures Run.
type Options struct {
	// RecoveryFile is the recovery.env the rescue hook copied into RAM.
	RecoveryFile string
	// OutDir is the RAM-backed directory holding per-recovery state and results.
	OutDir string
	// Timeout bounds the whole claim, retries included.
	Timeout time.Duration

	// Test seams; zero values select the production behavior.
	ramBacked   func(string) (bool, error)
	backoffBase time.Duration
	backoffMax  time.Duration
}

// Result describes a staged claim. It holds no credentials.
type Result struct {
	RestoreID  string
	RecoveryID string
	// Dir is the per-recovery directory the claim is staged in; LoadStaged
	// reads it back.
	Dir        string
	PlanFile   string
	RcloneFile string
	// Repository names where the backup is, without credentials.
	Repository string
}

// Run claims this instance's restore job and stages the result under
// OutDir/<recoveryId>. Every check that can fail runs before the claim is sent,
// because the claim uses up the one-time token and cannot be undone.
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.Timeout <= 0 {
		return Result{}, errors.New("timeout must be positive")
	}
	ramBacked := opts.ramBacked
	if ramBacked == nil {
		ramBacked = isRAMBacked
	}

	rec, err := LoadRecovery(opts.RecoveryFile)
	if err != nil {
		return Result{}, err
	}

	// Created here so that a run by hand needs no preparation. Making an empty
	// directory is harmless even on a disk: the check below refuses one before
	// anything secret is written into it.
	if err := os.MkdirAll(opts.OutDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("create %s: %w", opts.OutDir, err)
	}
	ram, err := ramBacked(opts.OutDir)
	if err != nil {
		return Result{}, err
	}
	if !ram {
		return Result{}, fmt.Errorf("%s is not on tmpfs: repository credentials must never be written to a disk", opts.OutDir)
	}

	dir := filepath.Join(opts.OutDir, rec.RecoveryID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Result{}, fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return Result{}, fmt.Errorf("restrict %s: %w", dir, err)
	}

	lock, err := filelock.Acquire(ctx, filepath.Join(dir, "agent.lock"), 0)
	if errors.Is(err, filelock.ErrLocked) {
		return Result{}, fmt.Errorf("another agent is already claiming recovery %s", rec.RecoveryID)
	}
	if err != nil {
		return Result{}, err
	}
	defer func() {
		if err := lock.Release(); err != nil {
			log.Warn().Err(err).Str("recoveryId", rec.RecoveryID).Msg("Failed to release the agent lock")
		}
	}()

	key, err := loadOrCreateRequestKey(dir)
	if err != nil {
		return Result{}, err
	}

	claimCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	log.Info().Str("recoveryId", rec.RecoveryID).Str("framework", rec.FrameworkURL).Msg("Claiming restore job")
	resp, err := newClaimClient(rec, opts.backoffBase, opts.backoffMax).claim(claimCtx, rec, key)
	if err != nil {
		return Result{}, err
	}
	if resp.RecoveryID != rec.RecoveryID {
		return Result{}, fmt.Errorf("framework answered for recovery %q instead of %q", resp.RecoveryID, rec.RecoveryID)
	}

	if err := writeResults(dir, resp); err != nil {
		return Result{}, err
	}
	log.Info().Str("recoveryId", rec.RecoveryID).Str("restoreId", resp.RestoreID).Msg("Restore job claimed and staged")

	r := resp.Repository
	return Result{
		RestoreID:  resp.RestoreID,
		RecoveryID: resp.RecoveryID,
		Dir:        dir,
		PlanFile:   filepath.Join(dir, planFile),
		RcloneFile: filepath.Join(dir, rcloneFile),
		Repository: fmt.Sprintf("%s %s bucket=%s prefix=%s", r.Provider, r.Endpoint, r.Bucket, r.Prefix),
	}, nil
}
