//go:build !rescue

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sync/atomic"

	"github.com/cloud-barista/ai-ha/pkg/agent/worker"
	apimodel "github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/repository"
)

// backupRunner carries out backup jobs handed out by the framework.
type backupRunner struct {
	// system is the identity the framework knows this agent by. Backups are
	// filed under it rather than under one re-derived locally: the framework
	// created the job for this id, and a set stored anywhere else would not
	// line up with the job that asked for it.
	system   model.SystemInfo
	stateDir string
}

// Run implements worker.Runner.
func (r backupRunner) Run(ctx context.Context, jobID string, raw json.RawMessage, access apimodel.RepositoryAccess, progress *atomic.Int64) (worker.Outcome, error) {
	var spec apimodel.BackupJobSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return worker.Outcome{}, fmt.Errorf("decode the backup job spec: %w", err)
	}
	// The job's repository, never the one configured on this host: the
	// framework records where it asked the set to go, and a set written
	// anywhere else could not be found to restore.
	uri, auth, err := s3Target(access)
	if err != nil {
		return worker.Outcome{}, err
	}

	set, err := runBackupSet(ctx, backupRequest{
		devices:     spec.Devices,
		compression: spec.Compression,
		stateDir:    r.stateDir,
		system:      r.system,
		// Carried from the inventory the framework built this job from, so
		// that a partition replaced since then is refused rather than captured
		// as if it were the one asked for.
		expectedFSUUIDs: spec.ExpectedFSUUIDs,
		progress:        progress,
		openRepository: func(ctx context.Context) (repository.Repository, error) {
			return repository.Open(ctx, uri, repository.WithS3Auth(auth), repository.WithLockDir(cfg.LockDir))
		},
	})

	// A set that partly failed is still reported with what it captured: a
	// partial capture that says so is worth more to an operator than nothing.
	outcome := worker.Outcome{}
	if set.ID != "" {
		outcome.Message = fmt.Sprintf("Backup set %s (%s, %d item(s))", set.ID, set.Status, len(set.Items))
		outcome.Detail = backupResult{
			BackupSetID: set.ID,
			Manifest:    model.ManifestName(set.ID),
			Status:      string(set.Status),
			Items:       len(set.Items),
		}
	}
	return outcome, err
}

// backupResult is what a finished backup job reports to the framework.
type backupResult struct {
	BackupSetID string `json:"backupSetId"`
	Manifest    string `json:"manifest"`
	Status      string `json:"status"`
	Items       int    `json:"items"`
}

// s3Target turns the repository a job was handed into what the S3 backend
// opens: an s3:// URI and its connection settings.
//
// The framework registers an endpoint as a URL ("https://host:port"); the S3
// client takes the host and whether to use TLS. AWS has no endpoint: the
// client's own default reaches it, always over TLS.
func s3Target(a apimodel.RepositoryAccess) (string, repository.S3Auth, error) {
	if a.Type == "" {
		return "", repository.S3Auth{}, errors.New("the framework sent no repository with this job")
	}
	if a.Type != "s3" {
		return "", repository.S3Auth{}, fmt.Errorf("repository type %q is not supported for framework jobs", a.Type)
	}
	if a.Bucket == "" {
		return "", repository.S3Auth{}, errors.New("the job's repository names no bucket")
	}
	auth := repository.S3Auth{
		AccessKey: a.AccessKey, SecretKey: a.SecretKey, SessionToken: a.SessionToken, Region: a.Region,
	}
	if a.Endpoint != "" {
		u, err := url.Parse(a.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", repository.S3Auth{}, errors.New("the job's repository endpoint is not an http(s) URL")
		}
		auth.Endpoint = u.Host
		auth.UseSSL = u.Scheme == "https"
	}
	return "s3://" + path.Join(a.Bucket, a.Prefix), auth, nil
}
