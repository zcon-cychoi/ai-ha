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

	backupmodel "github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/agent"
	"github.com/cloud-barista/ai-ha/pkg/framework/ha"
	"github.com/cloud-barista/ai-ha/pkg/framework/inventory"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/cloud-barista/ai-ha/pkg/framework/repository"
	"github.com/cloud-barista/ai-ha/pkg/framework/restore"
	"github.com/cloud-barista/ai-ha/pkg/framework/secret"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dbSetupTimeout bounds connecting to PostgreSQL and applying the schema.
const dbSetupTimeout = 15 * time.Second

// migrations are the packages' schemas, in the order openDB applies them.
//
// Every command applies them, while the server may be serving agents. Each
// must therefore take no lock on a database that is already up to date: a
// schema that did would hold up claims and reports, and could deadlock with
// them (plan/protocol.md 3.5).
var migrations = []func(context.Context, *pgxpool.Pool) error{
	repository.Migrate,
	restore.Migrate,
	agent.Migrate,
	job.Migrate,
	inventory.Migrate,
	ha.Migrate,
}

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
	for _, migrate := range migrations {
		if err := migrate(setupCtx, pool); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return pool, nil
}

// newHAService returns the HA group registry over pool.
func newHAService(pool *pgxpool.Pool) (*ha.Service, error) {
	t, err := haTimings()
	if err != nil {
		return nil, err
	}
	return ha.NewService(ha.NewPGStore(pool), agent.NewService(agent.NewPGStore(pool)), t, backupmodel.NewID), nil
}

// newFlowService returns the HA flows over pool. box is nil for a command
// that neither starts a setup nor claims jobs: those are the only two that
// seal or open the peers' shared secret.
func newFlowService(pool *pgxpool.Pool, box *secret.Box) (*ha.FlowService, error) {
	t, err := haTimings()
	if err != nil {
		return nil, err
	}
	if cfg.HAJobTTL <= 0 {
		return nil, errors.New("AIHA_FRAMEWORK_HA_JOB_TTL must be positive")
	}
	// A nil *secret.Box inside the interface would not compare equal to nil,
	// and the flows would try to use it.
	var sealer ha.Sealer
	if box != nil {
		sealer = box
	}
	return ha.NewFlowService(ha.NewPGStore(pool), sealer, t, cfg.HAJobTTL, backupmodel.NewID), nil
}

// haTimings returns the HA reporting timings.
//
// It refuses timings that would make a healthy node look silent. A report
// counts as missing once it is older than AIHA_FRAMEWORK_HA_STALE_AFTER; were
// that no more than one interval, a single report arriving late would flip a
// verdict and record an event for nothing.
func haTimings() (ha.Timings, error) {
	switch {
	case cfg.HAReportInterval < time.Second || cfg.HAStaleAfter <= 0:
		return ha.Timings{}, errors.New("AIHA_FRAMEWORK_HA_REPORT_INTERVAL must be at least 1s and AIHA_FRAMEWORK_HA_STALE_AFTER positive")
	case cfg.HAStaleAfter < 2*cfg.HAReportInterval:
		return ha.Timings{}, fmt.Errorf(
			"AIHA_FRAMEWORK_HA_STALE_AFTER (%s) must be at least twice AIHA_FRAMEWORK_HA_REPORT_INTERVAL (%s), "+
				"or one late report makes a healthy node look silent",
			cfg.HAStaleAfter, cfg.HAReportInterval)
	}
	return ha.Timings{ReportInterval: cfg.HAReportInterval, StaleAfter: cfg.HAStaleAfter}, nil
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
func newJobService(pool *pgxpool.Pool, repos *repository.Service, flows job.Flows) (*job.Service, error) {
	switch {
	case cfg.JobTTL <= 0, cfg.JobClaimedTimeout <= 0, cfg.JobLease <= 0,
		cfg.JobProgressInterval <= 0, cfg.JobSweepInterval <= 0:
		return nil, errors.New("AIHA_FRAMEWORK_JOB_* durations must be positive")
	case cfg.HAJobTTL <= 0:
		return nil, errors.New("AIHA_FRAMEWORK_HA_JOB_TTL must be positive")
	case cfg.JobLease < 3*cfg.JobProgressInterval:
		return nil, fmt.Errorf(
			"AIHA_FRAMEWORK_JOB_LEASE (%s) must be at least three times AIHA_FRAMEWORK_JOB_PROGRESS_INTERVAL (%s), "+
				"or one late report costs a healthy agent its lease",
			cfg.JobLease, cfg.JobProgressInterval)
	}
	return job.NewService(job.NewPGStore(pool), repos, flows,
		job.TTLs{Backup: cfg.JobTTL, HA: cfg.HAJobTTL}, cfg.JobClaimedTimeout, job.Timings{
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
	box, err := loadSecretBox("the repository credentials")
	if err != nil {
		return nil, err
	}
	return repository.NewService(repository.NewPGStore(pool), box), nil
}

// loadSecretBox loads the framework's secret key. what names the secrets it
// is needed for, for the message when it is not configured.
func loadSecretBox(what string) (*secret.Box, error) {
	if cfg.SecretKeyFile == "" {
		return nil, fmt.Errorf("AIHA_FRAMEWORK_SECRET_KEY_FILE is required: it encrypts %s the framework keeps", what)
	}
	return secret.LoadKeyFile(cfg.SecretKeyFile)
}

// newServices returns the repository registry, the HA flows and the job queue
// sharing them, for commands that need the queue. withKey loads the secret
// key for both: the server decrypts repository credentials and the peers'
// shared secret at every claim.
func newServices(pool *pgxpool.Pool, withKey bool) (*repository.Service, *job.Service, *ha.FlowService, error) {
	var box *secret.Box
	if withKey {
		var err error
		if box, err = loadSecretBox("the repository credentials and the HA groups' shared secrets"); err != nil {
			return nil, nil, nil, err
		}
	}
	repos := repository.NewService(repository.NewPGStore(pool), box)
	flows, err := newFlowService(pool, box)
	if err != nil {
		return nil, nil, nil, err
	}
	jobs, err := newJobService(pool, repos, flows)
	if err != nil {
		return nil, nil, nil, err
	}
	return repos, jobs, flows, nil
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
