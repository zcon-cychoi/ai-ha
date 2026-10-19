package model

import (
	"encoding/json"
	"time"
)

// AgentRegisterReq is sent by an agent to enrol with the framework.
type AgentRegisterReq struct {
	// RegistrationToken authorizes the enrolment. It is spent by a successful
	// registration and authorizes nothing else: it cannot receive work.
	RegistrationToken string `json:"registrationToken" validate:"required" example:"<registration-token>"`
	// RegistrationKey identifies this enrolment attempt. A retry reuses it so
	// that the framework recognises a repeat instead of enrolling a second
	// agent. 32 hexadecimal digits.
	RegistrationKey string `json:"registrationKey" validate:"required" example:"3f2a9c1e6b7d4a805e1f9c2d8b0a7e63"`
	// SystemID is the machine this agent protects. The agent proposes it; what
	// makes the enrolment legitimate is the registration token, never this.
	SystemID string `json:"systemId" validate:"required" example:"1a2b3c4d5e6f7081"`
	// Hostname and AgentVersion are recorded for display and diagnosis.
	Hostname     string `json:"hostname,omitempty"     example:"web01"`
	AgentVersion string `json:"agentVersion,omitempty" example:"aiha-agent 0.1.0"`
}

// AgentRegisterResp carries the identity and credential a successful
// enrolment issues. The token is returned only here: the framework keeps just
// its hash and cannot reissue it.
type AgentRegisterResp struct {
	AgentID  string `json:"agentId"  example:"9b1c2d3e4f5a6b7c"`
	SystemID string `json:"systemId" example:"1a2b3c4d5e6f7081"`
	// AgentToken authenticates every later request, as a bearer token.
	AgentToken string `json:"agentToken" example:"<agent-token>"`
}

// AgentJob is work handed to an agent by the long poll.
//
// Receiving it grants nothing. Two agents can be handed the same job; the
// claim decides between them, and work must not start until a claim succeeds.
type AgentJob struct {
	JobID string `json:"jobId" example:"5e6f70819a0b1c2d"`
	Kind  string `json:"kind"  example:"backup"`
	// Spec is what to do, passed through as the job was created.
	Spec json.RawMessage `json:"spec" swaggertype:"object"`
}

// AgentJobClaimReq claims a job.
type AgentJobClaimReq struct {
	// ClaimRequestKey scopes idempotency to this one request. A retry reuses
	// it; a stale request with a different key cannot join a newer attempt.
	// 32 hexadecimal digits.
	ClaimRequestKey string `json:"claimRequestKey" validate:"required" example:"8b1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f"`
}

// AgentJobClaimResp is the authority to run a job.
type AgentJobClaimResp struct {
	JobID     string `json:"jobId"     example:"5e6f70819a0b1c2d"`
	AttemptID string `json:"attemptId" example:"7a8b9c0d1e2f3a4b"`
	// LeaseToken proves this attempt is current. Every report carries it. It
	// is returned only here.
	LeaseToken string          `json:"leaseToken" example:"<lease-token>"`
	Spec       json.RawMessage `json:"spec"       swaggertype:"object"`
	// ProgressIntervalSeconds is how often to report, whether or not the work
	// has advanced. LeaseSeconds is how long a report buys. They are sent here
	// rather than compiled into the agent so that they can be tuned without
	// redeploying.
	ProgressIntervalSeconds int `json:"progressIntervalSeconds" example:"10"`
	LeaseSeconds            int `json:"leaseSeconds"            example:"90"`
	// Repository is where the job writes, with the credentials to do so. It is
	// sent only in this response and stored nowhere with the job.
	Repository RepositoryAccess `json:"repository"`
}

// AgentJobProgressReq reports on a running attempt and renews its lease.
type AgentJobProgressReq struct {
	AttemptID  string `json:"attemptId"  validate:"required" example:"7a8b9c0d1e2f3a4b"`
	LeaseToken string `json:"leaseToken" validate:"required" example:"<lease-token>"`
	// Seq orders reports within an attempt. An older one is ignored.
	Seq   int64  `json:"seq"   validate:"required,min=1" example:"3"`
	Phase string `json:"phase,omitempty" example:"archiving"`
	// ProcessedBytes is how much has been uploaded so far.
	ProcessedBytes int64 `json:"processedBytes" example:"1048576"`
	// Advanced says whether the work moved since the last report. A report
	// that only proves the agent is alive sets it false, which keeps an agent
	// that is up but stuck distinguishable from one that is gone.
	Advanced bool `json:"advanced" example:"true"`
}

// AgentJobProgressResp answers a progress report.
type AgentJobProgressResp struct {
	// CancelRequested tells the agent to stop. There is no inbound path to an
	// agent, so this is the only way a cancel can reach it.
	CancelRequested bool `json:"cancelRequested" example:"false"`
}

// AgentJobResultReq reports how an attempt ended.
type AgentJobResultReq struct {
	AttemptID  string `json:"attemptId"  validate:"required" example:"7a8b9c0d1e2f3a4b"`
	LeaseToken string `json:"leaseToken" validate:"required" example:"<lease-token>"`
	// Outcome is completed, failed or cancelled.
	Outcome string `json:"outcome" validate:"required,oneof=completed failed cancelled" example:"completed"`
	Message string `json:"message,omitempty" example:"Backup set 9c0d1e2f3a4b5c6d captured"`
	// Detail is what the attempt produced, recorded as reported.
	Detail json.RawMessage `json:"detail,omitempty" swaggertype:"object"`
}

// BackupJobSpec is what a backup job asks an agent to do. It is the contract
// between the framework, which writes it, and the agent, which reads it — so it
// lives here, where both already look, rather than in either side's internals.
//
// It carries no repository credentials. The agent uses the repository it is
// configured with, which keeps long-lived secrets out of the framework's
// database until short-lived, scoped credentials exist to put there instead.
type BackupJobSpec struct {
	// Devices are the partitions to capture together as one set.
	Devices []string `json:"devices" example:"/dev/nvme0n1p1"`
	// Compression names the codec, empty for the default.
	Compression string `json:"compression,omitempty" example:"zstd"`
	// ExpectedFSUUIDs maps a device to the filesystem UUID the framework
	// expects to find on it, taken from the inventory this job was built from.
	//
	// A system can be repartitioned between the moment a job is created and
	// the moment it runs, and a device path that has been reused names a
	// different filesystem without any error to show for it. The agent checks
	// this before it writes anything. An entry is absent when the framework
	// had nothing to compare against, as when an operator named the devices by
	// hand; the check is then skipped rather than guessed at.
	ExpectedFSUUIDs map[string]string `json:"expectedFsUuids,omitempty"`
	// RepositoryID is the registered repository the set is written to. Only
	// the ID is kept with the job; the agent receives the repository's access,
	// credentials included, in the claim response.
	RepositoryID string `json:"repositoryId" example:"3f2a9c1e6b7d4a80"`
}

// AgentInventoryReq is a system's current configuration, as its agent sees it.
//
// It lets a backup be asked for without naming partitions by hand, which is
// the only way an operator can name the wrong ones. It carries no file
// contents: an agent's full system facts include /etc/netplan, which may hold
// secrets, and those belong in a backup manifest rather than in the
// framework's database.
//
// There is no systemId field. The system is the one the calling agent's token
// was issued for; a body that could name another system would let a valid
// token describe someone else's machine.
type AgentInventoryReq struct {
	// CollectedAt is when the agent read the system, which is not when the
	// framework heard about it. Backup targets chosen from a stale inventory
	// can name a partition that no longer exists.
	CollectedAt time.Time `json:"collectedAt" validate:"required" example:"2026-09-18T04:05:06Z"`

	Firmware      string `json:"firmware,omitempty"      validate:"max=16"   example:"uefi"`
	Architecture  string `json:"architecture,omitempty"  validate:"max=64"   example:"x86_64"`
	KernelRelease string `json:"kernelRelease,omitempty" validate:"max=256"  example:"6.8.0-45-generic"`
	OSID          string `json:"osId,omitempty"          validate:"max=64"   example:"ubuntu"`
	OSVersionID   string `json:"osVersionId,omitempty"   validate:"max=64"   example:"24.04"`

	Partitions []InventoryPartition `json:"partitions" validate:"required,min=1,max=128,dive"`

	// Complete is false when the agent could not account for every filesystem
	// the system needs. Backup targets are not chosen automatically from an
	// inventory that says so.
	Complete bool `json:"complete" example:"true"`
	// Missing names the filesystems that were unaccounted for, e.g. "/boot/efi".
	Missing []string `json:"missing,omitempty" validate:"max=64,dive,max=4096"`
	// IncompleteReason explains Complete being false, in an operator's terms.
	IncompleteReason string `json:"incompleteReason,omitempty" validate:"max=1024"`
}

// InventoryPartition is one mounted filesystem of a system.
type InventoryPartition struct {
	// Device is the canonical device path. It is resolved from the mount's
	// device number rather than copied from the mount table, whose source name
	// can be an alias ("/dev/root" on cloud images) that cannot be backed up.
	// It is empty when the path could not be resolved, which is why it is not
	// required: a partition that is present but unreadable reads very
	// differently from one that is absent.
	Device     string `json:"device,omitempty"     validate:"max=4096" example:"/dev/nvme0n1p1"`
	MountPoint string `json:"mountPoint"           validate:"required,max=4096" example:"/"`
	FSType     string `json:"fsType,omitempty"     validate:"max=64"   example:"ext4"`
	FSUUID     string `json:"fsUuid,omitempty"     validate:"max=128"  example:"11111111-2222-3333-4444-555555555555"`
	PartUUID   string `json:"partUuid,omitempty"   validate:"max=128"`
	Label      string `json:"label,omitempty"      validate:"max=256"`
	DevNo      string `json:"devNo,omitempty"      validate:"max=32"   example:"259:1"`

	// FSSizeBytes and FSUsedBytes are the filesystem's size and usage, not the
	// device's. Null means the value could not be determined, which is not the
	// same as a filesystem that is empty.
	FSSizeBytes *int64 `json:"fsSizeBytes,omitempty" example:"8298131456"`
	FSUsedBytes *int64 `json:"fsUsedBytes,omitempty" example:"2147483648"`

	// Candidate says this partition is included when backup targets are chosen
	// automatically. Selectable says it may be named by hand. They differ: a
	// partition can be selectable without being a candidate, while one that is
	// not selectable cannot be captured at all and is refused even when asked
	// for by name.
	Candidate  bool `json:"candidate"`
	Selectable bool `json:"selectable"`
	// Reason explains a false in either flag, in an operator's terms.
	Reason string `json:"reason,omitempty" validate:"max=512"`
}
