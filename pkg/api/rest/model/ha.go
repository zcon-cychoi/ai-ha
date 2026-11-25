package model

import "time"

// The types below are the planned contract of the redundancy API (TM-AI-HA-02,
// plan/ha-roadmap.md). Its routes answer 501 until the work behind them exists,
// so these shapes are what a client can plan against, not what it gets today.

// HAResourceSpec is the replicated volume of an HA group.
type HAResourceSpec struct {
	// Name identifies the replication resource. Empty has the framework issue
	// one; set, it adopts a resource that already exists.
	Name string `json:"name,omitempty" validate:"max=63" example:"aiha-r0"`
	// Device is what the filesystem sits on. The disk holding the data is
	// each node's own (HAGroupReq), because its by-id name differs between
	// nodes on most clouds.
	Device     string `json:"device"     validate:"required,max=4096" example:"/dev/drbd0"`
	MountPoint string `json:"mountPoint" validate:"required,max=4096" example:"/data"`
	Port       int    `json:"port"       validate:"required,min=1,max=65535" example:"7789"`
}

// HAServiceSpec is what is health-checked on each node and moved in a switch.
type HAServiceSpec struct {
	Unit       string `json:"unit"                 validate:"required,max=255" example:"ai-service.service"`
	Port       int    `json:"port"                 validate:"required,min=1,max=65535" example:"8000"`
	HealthPath string `json:"healthPath,omitempty" validate:"max=2048" example:"/healthz"`
	// ExpectedGPUs is how many accelerators each node must see. Zero skips
	// the check.
	ExpectedGPUs int `json:"expectedGpus,omitempty" validate:"min=0" example:"1"`
	// MaxHealthLatencyMs is how long the health API may take before the
	// group needs attention. It is the framework's judgement, so an agent
	// ignores it; it must stay below HAHealthAPITimeout, after which the call
	// counts as failed rather than slow.
	MaxHealthLatencyMs int `json:"maxHealthLatencyMs,omitempty" validate:"min=0" example:"1000"`
}

// HAHealthAPITimeout is how long an agent waits for a health API before it
// counts the call as failed. A latency limit at or above it could never be
// exceeded: the call would fail first.
const HAHealthAPITimeout = 5 * time.Second

// HAGroupReq registers an HA group: an active and a standby system, the volume
// replicated between them and the service that runs on it.
type HAGroupReq struct {
	Name            string `json:"name"            validate:"required,max=63" example:"inference-ha"`
	PrimarySystemID string `json:"primarySystemId" validate:"required" example:"1a2b3c4d5e6f7081"`
	StandbySystemID string `json:"standbySystemId" validate:"required" example:"2b3c4d5e6f708192"`
	// PrimaryDisk and StandbyDisk are each node's data disk, as the
	// /dev/disk/by-id path its inventory reports. A kernel name such as
	// /dev/nvme1n1 is refused: it can name another disk after a reboot.
	PrimaryDisk string         `json:"primaryDisk" validate:"required,max=4096" example:"/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol0123"`
	StandbyDisk string         `json:"standbyDisk" validate:"required,max=4096" example:"/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol0456"`
	Resource    HAResourceSpec `json:"resource"`
	Service     HAServiceSpec  `json:"service"`
	// ServiceAddress is where clients reach the service. It is recorded so
	// that a completed switch can say what to repoint.
	ServiceAddress string `json:"serviceAddress,omitempty" validate:"max=2048" example:"10.0.1.20:8000"`
}

// HAReplicationStatus is the replication state a node reports, in the words
// the replication tool uses. Only states are carried: never the resource
// configuration, which holds the peers' shared secret.
type HAReplicationStatus struct {
	// Role and PeerRole are this node's and its peer's: Primary, Secondary or
	// Unknown.
	Role     string `json:"role"               validate:"max=32" example:"Primary"`
	PeerRole string `json:"peerRole,omitempty" validate:"max=32" example:"Secondary"`
	// Connection is the link to the peer: Connected, Connecting, StandAlone
	// and so on. DRBD 8.4 folds a running resynchronization into it
	// (SyncSource, SyncTarget); DRBD 9 reports that separately, in Sync.
	Connection string `json:"connection"     validate:"max=32" example:"Connected"`
	Sync       string `json:"sync,omitempty" validate:"max=32" example:"Established"`
	// Disk and PeerDisk are this node's and its peer's data: UpToDate,
	// Inconsistent, Outdated and so on.
	Disk     string `json:"disk"               validate:"max=32" example:"UpToDate"`
	PeerDisk string `json:"peerDisk,omitempty" validate:"max=32" example:"UpToDate"`
	// OutOfSyncBytes is how much this node knows to differ from its peer.
	// Absent means it could not be read, which is not the same as nothing
	// being out of sync: the second is what a switch has to confirm.
	OutOfSyncBytes *int64 `json:"outOfSyncBytes,omitempty" example:"0"`
	// Error says why the status could not be read. The other fields are then
	// not meaningful.
	Error string `json:"error,omitempty" validate:"max=1024"`
}

// HAUnitCheck is the state of a systemd unit, as systemd names it.
type HAUnitCheck struct {
	// LoadState is "not-found" when the unit does not exist.
	LoadState   string `json:"loadState,omitempty"   validate:"max=32" example:"loaded"`
	ActiveState string `json:"activeState,omitempty" validate:"max=32" example:"active"`
	SubState    string `json:"subState,omitempty"    validate:"max=32" example:"running"`
	Error       string `json:"error,omitempty"       validate:"max=1024"`
}

// HAPortCheck is the outcome of opening a TCP connection.
type HAPortCheck struct {
	Address string `json:"address" validate:"max=300" example:"127.0.0.1:8000"`
	Open    bool   `json:"open"    example:"true"`
	Error   string `json:"error,omitempty" validate:"max=1024"`
}

// HAHTTPCheck is the outcome of calling a health API.
type HAHTTPCheck struct {
	URL string `json:"url" validate:"max=2400" example:"http://127.0.0.1:8000/healthz"`
	// StatusCode is zero when no response arrived; Error then says why.
	StatusCode int    `json:"statusCode,omitempty" example:"200"`
	LatencyMs  int64  `json:"latencyMs"            example:"35"`
	Error      string `json:"error,omitempty"      validate:"max=1024"`
}

// HAGPUCheck is how many accelerators the node's driver reports.
type HAGPUCheck struct {
	Count int    `json:"count" example:"1"`
	Error string `json:"error,omitempty" validate:"max=1024"`
}

// AgentHealthReq is one node's measurements of its service and replication.
//
// It carries values only. Judging them is the framework's job, because what a
// value should be depends on the node's role, and the role is the framework's
// record rather than the agent's.
//
// There is no systemId field: the node is the one the calling agent's token
// was issued for. GroupID and Role echo the assignment the measurements were
// taken under, so that measurements taken for a role the node no longer holds
// are not judged against the new one. A node without an assignment sends
// neither and receives its assignment in the response.
type AgentHealthReq struct {
	CollectedAt time.Time `json:"collectedAt" validate:"required" example:"2026-11-09T04:05:06Z"`
	GroupID     string    `json:"groupId,omitempty" validate:"max=64" example:"6a7b8c9d0e1f2a3b"`
	Role        string    `json:"role,omitempty"    validate:"omitempty,oneof=primary standby" example:"primary"`
	// Hostname is the node's current host name, which a restored system may
	// have changed since it enrolled.
	Hostname string `json:"hostname,omitempty" validate:"max=255" example:"ip-10-0-1-10"`

	// Service, Port and HealthAPI are measured on the node itself.
	Service   *HAUnitCheck `json:"service,omitempty"`
	Port      *HAPortCheck `json:"port,omitempty"`
	HealthAPI *HAHTTPCheck `json:"healthApi,omitempty"`
	// PeerPort and PeerHealthAPI are the standby's view of the primary's
	// service over the private network. The framework never connects to a
	// node, so this is how it learns the service can be reached from where a
	// switch would move it.
	PeerPort      *HAPortCheck `json:"peerPort,omitempty"`
	PeerHealthAPI *HAHTTPCheck `json:"peerHealthApi,omitempty"`

	GPU         *HAGPUCheck          `json:"gpu,omitempty"`
	Replication *HAReplicationStatus `json:"replication,omitempty"`
}

// HAAssignedResource is the replication resource a node checks.
type HAAssignedResource struct {
	Name       string `json:"name"       example:"r0"`
	Device     string `json:"device"     example:"/dev/drbd0"`
	MountPoint string `json:"mountPoint" example:"/data"`
}

// HAProbeTarget is where the standby checks the primary's service from.
type HAProbeTarget struct {
	Address    string `json:"address"              example:"10.0.1.10"`
	Port       int    `json:"port"                 example:"8000"`
	HealthPath string `json:"healthPath,omitempty" example:"/healthz"`
}

// HAAssignment is what a node is to check, and in which role.
type HAAssignment struct {
	GroupID string `json:"groupId" example:"6a7b8c9d0e1f2a3b"`
	// Role is primary or standby. It is the framework's record, which a
	// switch changes; the node's replica role is what the node reports.
	Role     string             `json:"role" example:"standby"`
	Service  HAServiceSpec      `json:"service"`
	Resource HAAssignedResource `json:"resource"`
	// Probe is set for the standby only.
	Probe *HAProbeTarget `json:"probe,omitempty"`
}

// AgentHealthResp answers a health report with the node's assignment, which
// is how a node learns what to check: the framework never connects to it.
type AgentHealthResp struct {
	// Assigned is false for a system in no HA group. It has nothing to
	// measure and keeps reporting so that it notices when it joins one.
	Assigned   bool          `json:"assigned"   example:"true"`
	Assignment *HAAssignment `json:"assignment,omitempty"`
	// ReportIntervalSeconds is how long to wait before the next report.
	ReportIntervalSeconds int `json:"reportIntervalSeconds" example:"30"`
}

// HANodeStatus is one node's latest health report.
type HANodeStatus struct {
	SystemID string `json:"systemId" example:"1a2b3c4d5e6f7081"`
	// Role is primary or standby.
	Role string `json:"role" example:"primary"`
	// Disk is the node's data disk.
	Disk        string               `json:"disk,omitempty" example:"/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol0123"`
	ReportedAt  *time.Time           `json:"reportedAt,omitempty"`
	ServiceUp   bool                 `json:"serviceUp"   example:"true"`
	PortOpen    bool                 `json:"portOpen"    example:"true"`
	HealthCode  int                  `json:"healthCode,omitempty"      example:"200"`
	LatencyMs   int64                `json:"healthLatencyMs,omitempty" example:"35"`
	GPUsSeen    int                  `json:"gpusSeen,omitempty"        example:"1"`
	Replication *HAReplicationStatus `json:"replication,omitempty"`
}

// HAGroupInfo is an HA group with its state.
type HAGroupInfo struct {
	GroupID string `json:"groupId" example:"6a7b8c9d0e1f2a3b"`
	Name    string `json:"name"    example:"inference-ha"`
	// State is registered, configuring, syncing, protected, degraded,
	// failover_pending, switching or error. It governs what may be done next.
	State string `json:"state" example:"protected"`
	// Verdict is ok, attention or error, derived anew at each evaluation, and
	// Reasons explains it. It is not State: a broken replication link is a
	// verdict of error on a group that is degraded.
	Verdict        string         `json:"verdict"           example:"ok"`
	Reasons        []string       `json:"reasons,omitempty"`
	Primary        HANodeStatus   `json:"primary"`
	Standby        HANodeStatus   `json:"standby"`
	Resource       HAResourceSpec `json:"resource"`
	Service        HAServiceSpec  `json:"service"`
	ServiceAddress string         `json:"serviceAddress,omitempty" example:"10.0.1.20:8000"`
	CreatedAt      time.Time      `json:"createdAt"`
}

// HAGroupListResp lists HA groups.
type HAGroupListResp struct {
	Items []HAGroupInfo `json:"items"`
}

// HAEvent is one change in an HA group's state. Seq only increases, so a
// reader resumes from the last one it saw.
type HAEvent struct {
	Seq     int64     `json:"seq"     example:"42"`
	GroupID string    `json:"groupId" example:"6a7b8c9d0e1f2a3b"`
	Kind    string    `json:"kind"    example:"state_changed"`
	From    string    `json:"from,omitempty"   example:"protected"`
	To      string    `json:"to,omitempty"     example:"degraded"`
	Reason  string    `json:"reason,omitempty" example:"Replication disconnected"`
	At      time.Time `json:"at"`
}

// HAEventListResp lists HA events in sequence order.
type HAEventListResp struct {
	Items []HAEvent `json:"items"`
}

// HAApproval records who authorized an action that cannot be undone. Every
// switch needs one: nothing is promoted on the framework's own judgement.
type HAApproval struct {
	ApprovedBy string `json:"approvedBy" validate:"required,max=255" example:"operator@example.com"`
	Reason     string `json:"reason,omitempty" validate:"max=1024" example:"Planned maintenance"`
	// IncidentID links the action to the incident it answers, if any.
	IncidentID string `json:"incidentId,omitempty" validate:"max=64" example:"7c8d9e0f1a2b3c4d"`
}

// HAFailoverReq approves a failover. ConfirmFenced states the old primary has
// really stopped writing, which only someone outside the framework can know.
// AcceptDataLoss acknowledges a gap in replication, when there is one.
type HAFailoverReq struct {
	HAApproval
	ConfirmFenced  bool `json:"confirmFenced"  validate:"required" example:"true"`
	AcceptDataLoss bool `json:"acceptDataLoss" example:"false"`
}

// HATransitionInfo is one switch or recovery of an HA group.
type HATransitionInfo struct {
	TransitionID string `json:"transitionId" example:"8d9e0f1a2b3c4d5e"`
	GroupID      string `json:"groupId"      example:"6a7b8c9d0e1f2a3b"`
	// Kind is switchover, failover, resume or rejoin.
	Kind string `json:"kind" example:"switchover"`
	// Status is pending, running, completed or failed, and Phase the step it
	// is on.
	Status           string     `json:"status"           example:"running"`
	Phase            string     `json:"phase,omitempty"  example:"demoting"`
	JobIDs           []string   `json:"jobIds,omitempty"`
	RequestedBy      string     `json:"requestedBy,omitempty" example:"admin"`
	ApprovedBy       string     `json:"approvedBy,omitempty"  example:"operator@example.com"`
	IncidentID       string     `json:"incidentId,omitempty"  example:"7c8d9e0f1a2b3c4d"`
	FencingConfirmed bool       `json:"fencingConfirmed" example:"false"`
	DataLossAccepted bool       `json:"dataLossAccepted" example:"false"`
	Message          string     `json:"message,omitempty"`
	StartedAt        time.Time  `json:"startedAt"`
	FinishedAt       *time.Time `json:"finishedAt,omitempty"`
}

// HATransitionListResp lists an HA group's transitions.
type HATransitionListResp struct {
	Items []HATransitionInfo `json:"items"`
}

// ReplicationAccess is what an HA job needs to authenticate the two nodes of
// a replication resource to each other. The framework keeps it encrypted and
// sends it only in the claim response; the agent writes it into the resource
// file and logs it nowhere.
type ReplicationAccess struct {
	SharedSecret string `json:"sharedSecret" example:"<shared-secret>"`
}

// ReplicationNode is one node of a replication resource, as its resource file
// names it.
type ReplicationNode struct {
	// Hostname is the node's host name, which the resource file's "on"
	// section must match. It is the one the node last reported, since a
	// restored system may have been renamed since it enrolled.
	Hostname string `json:"hostname" example:"ip-10-0-1-10"`
	Address  string `json:"address"  example:"10.0.1.10"`
	// NodeID is the node's number within the resource. It stays with the
	// system, not the role: the metadata on disk is laid out by it.
	NodeID int `json:"nodeId" example:"0"`
	// Disk is the node's data disk, a /dev/disk/by-id path.
	Disk string `json:"disk" example:"/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol0123"`
}

// ReplicationSetupSpec asks a node to configure its side of a replication
// resource (plan/ha-roadmap.md H3b). Every step is taken only if the node is
// not already there, so that a repeated request redoes nothing.
//
// It carries no secret. The peers' shared secret reaches the agent in the
// claim response only (ReplicationAccess).
type ReplicationSetupSpec struct {
	// FlowID and Step name the HA operation this job is a step of. A job
	// whose operation has moved on is refused before it starts.
	FlowID   string `json:"flowId" example:"4d5e6f7a8b9c0d1e"`
	Step     int    `json:"step"   example:"1"`
	Resource string `json:"resource" example:"aiha-6a7b8c9d"`
	// Device is the replicated device the filesystem goes on.
	Device     string `json:"device"     example:"/dev/drbd0"`
	MountPoint string `json:"mountPoint" example:"/data"`
	Port       int    `json:"port"       example:"7789"`
	// Protocol is the replication protocol: C, synchronous.
	Protocol string `json:"protocol" example:"C"`
	// ServiceUnit is the protected service. Setup stops it from starting on
	// its own and makes it require the replicated mount.
	ServiceUnit string          `json:"serviceUnit" example:"ai-service.service"`
	Self        ReplicationNode `json:"self"`
	Peer        ReplicationNode `json:"peer"`
	// InitialPrimary is set for the node that is promoted first, formats the
	// volume and starts the service. The other node only brings its side up.
	InitialPrimary bool `json:"initialPrimary" example:"false"`
	// PeerDiskBytes is the size of the peer's data disk, as the peer's own
	// setup measured it. Set for the initial primary, which refuses a disk of
	// another size; zero leaves the sizes unchecked.
	PeerDiskBytes int64 `json:"peerDiskBytes,omitempty" example:"8589934592"`
	// OverwriteDisk lets setup use a data disk that has no signature but
	// does not read as zeros at its start or end, writing over what is
	// there. A disk with a filesystem, partitions or mounts is refused all
	// the same.
	OverwriteDisk bool `json:"overwriteDisk,omitempty" example:"false"`
}

// ReplicationTeardownSpec asks a node to remove its side of a replication
// resource: stop the service, unmount, step down and take the resource down,
// then remove the files setup wrote. The data and the replication metadata
// stay on the disk.
type ReplicationTeardownSpec struct {
	FlowID      string `json:"flowId"      example:"4d5e6f7a8b9c0d1e"`
	Step        int    `json:"step"        example:"1"`
	Resource    string `json:"resource"    example:"aiha-6a7b8c9d"`
	MountPoint  string `json:"mountPoint"  example:"/data"`
	ServiceUnit string `json:"serviceUnit" example:"ai-service.service"`
}

// ReplicationStep is one step a replication job took or found already done.
type ReplicationStep struct {
	Name string `json:"name" example:"create-metadata"`
	// Done is true when the job did it, false when it was already so.
	Done bool `json:"done" example:"true"`
}

// ReplicationJobResult is what a finished replication job reports.
type ReplicationJobResult struct {
	Steps []ReplicationStep `json:"steps"`
	// DiskBytes is the size of the node's data disk. The standby's is passed
	// to the primary, which refuses a disk of another size.
	DiskBytes int64 `json:"diskBytes,omitempty" example:"8589934592"`
	// Role and Disk are the resource's state on this node when the job ended.
	Role string `json:"role,omitempty" example:"Secondary"`
	Disk string `json:"disk,omitempty" example:"Inconsistent"`
}
