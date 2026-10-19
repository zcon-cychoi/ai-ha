package cmd

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/framework/agent"
	"github.com/cloud-barista/ai-ha/pkg/framework/inventory"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/cloud-barista/ai-ha/pkg/framework/repository"
	"github.com/cloud-barista/ai-ha/pkg/framework/restore"
	"github.com/cloud-barista/ai-ha/pkg/framework/secret"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dbSetupTimeout bounds connecting to PostgreSQL and applying the schema.
const dbSetupTimeout = 15 * time.Second

// openDB connects to PostgreSQL and applies the schema. The DSN holds a
// password, so it is never logged.
func openDB(ctx context.Context) (*pgxpool.Pool, error) {
	if cfg.DBDSN == "" {
		return nil, errors.New("AIHA_FRAMEWORK_DB_DSN is required")
	}
	setupCtx, cancel := context.WithTimeout(ctx, dbSetupTimeout)
	defer cancel()

	pool, err := pgxpool.New(setupCtx, cfg.DBDSN)
	if err != nil {
		return nil, fmt.Errorf("configure database connection: %w", err)
	}
	if err := pool.Ping(setupCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("reach database: %w", err)
	}
	if err := repository.Migrate(setupCtx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	if err := restore.Migrate(setupCtx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	if err := agent.Migrate(setupCtx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	if err := job.Migrate(setupCtx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	if err := inventory.Migrate(setupCtx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// newInventoryService returns the inventory service over pool.
//
// It has nothing to configure: a system keeps one inventory, which is replaced
// rather than expired, so there is no window or retention to get wrong.
func newInventoryService(pool *pgxpool.Pool) *inventory.Service {
	return inventory.NewService(inventory.NewPGStore(pool))
}

// newJobService returns the job queue over pool.
//
// It refuses timings that would make healthy agents lose their leases. A lease
// no longer than the report interval expires between two on-time reports, so
// every job would fail as abandoned while its agent was working normally.
func newJobService(pool *pgxpool.Pool, repos *repository.Service) (*job.Service, error) {
	switch {
	case cfg.JobTTL <= 0, cfg.JobClaimedTimeout <= 0, cfg.JobLease <= 0,
		cfg.JobProgressInterval <= 0, cfg.JobSweepInterval <= 0:
		return nil, errors.New("AIHA_FRAMEWORK_JOB_* durations must be positive")
	case cfg.JobLease < 3*cfg.JobProgressInterval:
		return nil, fmt.Errorf(
			"AIHA_FRAMEWORK_JOB_LEASE (%s) must be at least three times AIHA_FRAMEWORK_JOB_PROGRESS_INTERVAL (%s), "+
				"or one late report costs a healthy agent its lease",
			cfg.JobLease, cfg.JobProgressInterval)
	}
	return job.NewService(job.NewPGStore(pool), repos, cfg.JobTTL, cfg.JobClaimedTimeout, job.Timings{
		ProgressInterval: cfg.JobProgressInterval,
		LeaseTTL:         cfg.JobLease,
	}), nil
}

// newRestoreService returns the restore service over pool, rejecting windows
// that would make every claim or redelivery fail.
func newRestoreService(pool *pgxpool.Pool, jobs *job.Service, repos *repository.Service) (*restore.Service, error) {
	if cfg.ClaimTTL <= 0 || cfg.ClaimRedelivery <= 0 {
		return nil, errors.New("AIHA_FRAMEWORK_CLAIM_TTL and AIHA_FRAMEWORK_CLAIM_REDELIVERY must be positive")
	}
	return restore.NewService(restore.NewPGStore(pool), jobs, repos, cfg.ClaimTTL, cfg.ClaimRedelivery), nil
}

// newRepositoryService returns the repository registry over pool.
//
// withKey loads the secret key, for the commands that encrypt or decrypt
// credentials: registering a repository, replacing its credentials, and the
// server, which decrypts them for every claim. A missing or malformed key
// fails those commands rather than leaving credentials unreadable later.
// Listing and inspecting need no key.
func newRepositoryService(pool *pgxpool.Pool, withKey bool) (*repository.Service, error) {
	if !withKey {
		return repository.NewService(repository.NewPGStore(pool), nil), nil
	}
	if cfg.SecretKeyFile == "" {
		return nil, errors.New("AIHA_FRAMEWORK_SECRET_KEY_FILE is required: it encrypts the repository credentials the framework keeps")
	}
	box, err := secret.LoadKeyFile(cfg.SecretKeyFile)
	if err != nil {
		return nil, err
	}
	return repository.NewService(repository.NewPGStore(pool), box), nil
}

// newServices returns the repository registry and the job queue sharing it,
// for commands that need both.
func newServices(pool *pgxpool.Pool, withKey bool) (*repository.Service, *job.Service, error) {
	repos, err := newRepositoryService(pool, withKey)
	if err != nil {
		return nil, nil, err
	}
	jobs, err := newJobService(pool, repos)
	if err != nil {
		return nil, nil, err
	}
	return repos, jobs, nil
}

// agentCertSHA256 returns the certificate fingerprint to hand agents: the pin
// when AIHA_FRAMEWORK_CERT_PIN is on, and "" when agents verify the
// certificate against their CA bundle instead.
func agentCertSHA256() (string, error) {
	if !cfg.CertPin {
		return "", nil
	}
	return certFingerprint(cfg.TLSCertFile)
}

// certFingerprint returns the SHA-256 of the DER bytes of the first (leaf)
// certificate in the PEM file at path. This is the value agents pin; it is not
// a hash of the PEM file itself.
func certFingerprint(path string) (string, error) {
	if path == "" {
		return "", errors.New("AIHA_FRAMEWORK_TLS_CERT is required: agents pin the server certificate")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read TLS certificate: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("%s holds no PEM certificate", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse TLS certificate: %w", err)
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:]), nil
}
