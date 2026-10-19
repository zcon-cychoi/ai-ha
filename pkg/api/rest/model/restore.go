package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// RestoreClaimReq is sent by a rescue agent to claim a restore job.
type RestoreClaimReq struct {
	// ClaimToken is the one-time secret injected through the instance's init
	// script.
	ClaimToken string `json:"claimToken" validate:"required" example:"<claim-token>"`
	// RequestKey identifies this agent's claim. Retries reuse it so that a
	// response lost in transit can be fetched again.
	RequestKey string `json:"requestKey" validate:"required" example:"3f2a9c1e6b7d4a80"`
}

// RestoreClaimResp carries the restore plan and the repository access granted
// by a successful claim.
type RestoreClaimResp struct {
	RestoreID  string `json:"restoreId"  example:"9b1c2d3e4f5a6b7c"`
	RecoveryID string `json:"recoveryId" example:"1a2b3c4d5e6f7081"`
	// Plan is passed through as registered; its schema is RestorePlan.
	Plan       json.RawMessage  `json:"plan" swaggertype:"object"`
	Repository RepositoryAccess `json:"repository"`
	// ReportToken authenticates this agent's progress and result reports for
	// the restore. A new one is issued on every claim response, redeliveries
	// included, and only the latest is accepted. It is a secret.
	ReportToken string `json:"reportToken" example:"<report-token>"`
}

// RestorePlan names the backup set a restore job recovers. The file list is
// not part of it: the agent reads it from the set's manifest in the
// repository, which is the source of truth for what a set contains.
type RestorePlan struct {
	// SourceSystemID is the system the set was captured from. The repository
	// files a system's backups under this identifier.
	SourceSystemID string `json:"sourceSystemId" example:"web01-3f2a9c1e"`
	// SourceBackupSetID is the backup set to restore.
	SourceBackupSetID string `json:"sourceBackupSetId" example:"9c0d1e2f3a4b5c6d"`
}

// RestoreProgressReq reports where a running restore is. Reports carry no
// lease: a restore is never handed to another agent.
type RestoreProgressReq struct {
	// Seq orders reports within a restore. An older one is ignored.
	Seq int64 `json:"seq" validate:"required,min=1" example:"3"`
	// Phase is the step being carried out, e.g. "extracting".
	Phase string `json:"phase" validate:"required" example:"extracting"`
	// Step and TotalSteps place Phase in the sequence, e.g. 4 of 6.
	Step       int `json:"step"       validate:"min=0" example:"4"`
	TotalSteps int `json:"totalSteps" validate:"min=0" example:"6"`
	// Item is the backup file being restored, while extracting.
	Item string `json:"item,omitempty" example:"web01-nvme0n1p1-1a2b3c4d5e6f7081.aha"`
	// ProcessedBytes is how much of the backup files has been read from the
	// repository; TotalBytes is their combined size.
	ProcessedBytes int64 `json:"processedBytes" validate:"min=0" example:"1048576"`
	TotalBytes     int64 `json:"totalBytes"     validate:"min=0" example:"4294967296"`
}

// RestorePhasePreparing is the restore's first phase, which reads and checks
// everything and changes nothing on disk. A restore that fails in it may be
// run again under the same job.
const RestorePhasePreparing = "preparing"

// RestoreResultReq reports how a restore ended.
type RestoreResultReq struct {
	// Outcome is succeeded or failed.
	Outcome string `json:"outcome" validate:"required,oneof=succeeded failed" example:"succeeded"`
	// Phase is the step the restore ended in.
	Phase   string `json:"phase,omitempty"   example:"bootloader"`
	Message string `json:"message,omitempty" example:"Restored 3 partition(s) (412.5s)"`
}

// RepositoryAccess is the connection information for the backup repository
// holding the backup set to restore.
type RepositoryAccess struct {
	Type         string `json:"type"                   example:"s3"`
	Provider     string `json:"provider"               example:"Minio"`
	Endpoint     string `json:"endpoint,omitempty"     example:"https://storage.example.com:9000"`
	Region       string `json:"region,omitempty"       example:"us-east-1"`
	Bucket       string `json:"bucket"                 example:"aiha-backup"`
	Prefix       string `json:"prefix,omitempty"       example:"systems/web01"`
	AccessKey    string `json:"accessKey"              example:"<access-key>"`
	SecretKey    string `json:"secretKey"              example:"<secret-key>"`
	SessionToken string `json:"sessionToken,omitempty" example:"<session-token>"`
}

// planIDPattern is what a plan identifier must look like. Both become a path
// component in the repository, so nothing like "/" or ".." may pass; starting
// with a letter or digit rules out "." and "..".
var planIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)

// Validate checks that the plan names a system and a set by identifiers that
// are safe to use in a repository path.
func (p RestorePlan) Validate() error {
	if !planIDPattern.MatchString(p.SourceSystemID) {
		return errors.New("sourceSystemId is missing or not a valid identifier")
	}
	if !planIDPattern.MatchString(p.SourceBackupSetID) {
		return errors.New("sourceBackupSetId is missing or not a valid identifier")
	}
	return nil
}

// DecodeRestorePlan parses a plan as registered, rejecting unknown fields so
// that a plan written for another schema is refused rather than half read.
func DecodeRestorePlan(raw json.RawMessage) (RestorePlan, error) {
	var p RestorePlan
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return RestorePlan{}, fmt.Errorf("decode restore plan: %w", err)
	}
	if err := p.Validate(); err != nil {
		return RestorePlan{}, err
	}
	return p, nil
}
