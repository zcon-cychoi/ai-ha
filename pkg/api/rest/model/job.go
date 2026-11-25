package model

import (
	"encoding/json"
	"time"
)

// Job kinds. They are the contract between the framework, which creates a job,
// and the agent, which picks the runner for it by kind.
const (
	// JobKindBackup captures partitions into a backup set.
	JobKindBackup = "backup"

	// The HA kinds (TM-AI-HA-02 table 4-6, and the rejoin that table lacks).
	// Only an HA flow creates them, one per step and node.
	JobKindReplicationSetup    = "replication-setup"
	JobKindReplicationTeardown = "replication-teardown"
	JobKindHADemote            = "ha-demote"
	JobKindHAPromote           = "ha-promote"
	JobKindHARejoin            = "ha-rejoin"
)

// JobInfo is a job as a caller follows it.
type JobInfo struct {
	JobID    string `json:"jobId"    example:"5e6f70819a0b1c2d"`
	SystemID string `json:"systemId" example:"1a2b3c4d5e6f7081"`
	Kind     string `json:"kind"     example:"backup"`
	// Status is queued, claimed, running, cancelling, completed, failed or
	// cancelled.
	Status string `json:"status" example:"running"`
	// CancelRequested says a cancel was asked for. A running job only stops
	// once its agent confirms it.
	CancelRequested bool `json:"cancelRequested" example:"false"`
	// Spec is what the agent was asked to do. It holds no credentials.
	Spec        json.RawMessage `json:"spec"                  swaggertype:"object"`
	Message     string          `json:"message,omitempty"     example:"Backup set 9c0d1e2f3a4b5c6d (completed, 3 item(s))"`
	RequestedBy string          `json:"requestedBy,omitempty" example:"admin"`
	CreatedAt   time.Time       `json:"createdAt"`
	ExpiresAt   time.Time       `json:"expiresAt"`
	StartedAt   *time.Time      `json:"startedAt,omitempty"`
	FinishedAt  *time.Time      `json:"finishedAt,omitempty"`
	// Attempt is the execution in progress or the last one. It is absent until
	// an agent claims the job, and in lists.
	Attempt *JobAttemptInfo `json:"attempt,omitempty"`
}

// JobAttemptInfo is one execution of a job, reported as progress rather than a
// percentage: a backup streams and compresses, so its final size is not known
// until it ends.
type JobAttemptInfo struct {
	AttemptID string `json:"attemptId" example:"7a8b9c0d1e2f3a4b"`
	AgentID   string `json:"agentId"   example:"9b1c2d3e4f5a6b7c"`
	Phase     string `json:"phase,omitempty" example:"archiving"`
	// ProcessedBytes is how much has been handled so far. TotalUnknown says
	// there is no total to set it against: show throughput and elapsed time
	// rather than a percentage.
	ProcessedBytes int64 `json:"processedBytes" example:"1048576"`
	TotalUnknown   bool  `json:"totalUnknown"   example:"true"`
	// LastSeenAt moves with every report and LastProgressAt only when the work
	// advanced, so an agent that is alive but stuck can be told from one that
	// is gone. LeaseExpiresAt is when the attempt loses its authority.
	LastSeenAt     time.Time  `json:"lastSeenAt"`
	LastProgressAt *time.Time `json:"lastProgressAt,omitempty"`
	LeaseExpiresAt time.Time  `json:"leaseExpiresAt"`
	// Outcome is completed, failed, cancelled or unknown; unknown means the
	// agent stopped reporting, which is not a confirmed failure.
	Outcome string `json:"outcome,omitempty" example:"completed"`
	Message string `json:"message,omitempty"`
	// Result is what the agent reported producing, recorded as reported.
	Result     json.RawMessage `json:"result,omitempty" swaggertype:"object"`
	FinishedAt *time.Time      `json:"finishedAt,omitempty"`
}

// JobListResp lists jobs.
type JobListResp struct {
	Items []JobInfo `json:"items"`
}
