// Package ha keeps the framework's record of redundancy (HA) groups and judges
// their health from what the two nodes' agents report (TM-AI-HA-02 chapter 5,
// plan/ha-roadmap.md H1).
//
// The framework never connects to a node. Each node's agent reports what it
// measured, and the answer tells it what to measure: its role, the service
// definition and, for the standby, where to probe the primary from. Judging
// the measurements is done here, because what a value should be depends on the
// role, and the role is the framework's record rather than the agent's.
//
// A group has two separate kinds of status:
//
//   - State governs what may be done next. It moves only by a flow, an
//     operator, or a judgement that has lasted, and every move is an UPDATE
//     conditioned on the state expected, so that two movers cannot overwrite
//     each other.
//   - Verdict (ok, attention, error) and its reasons are derived anew at every
//     evaluation.
//
// Nothing here promotes, demotes or configures anything. Those are flows of
// later stages; this package only records and judges.
package ha

import (
	"context"
	"errors"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
)

// Role is a node's place in its group, as the framework records it. It is not
// the replica role the node reports: the two agree on a healthy group, and the
// difference is one of the things judged.
type Role string

const (
	RolePrimary Role = "primary"
	RoleStandby Role = "standby"
)

// State is a group's state (plan/ha-roadmap.md section 6).
type State string

const (
	StateRegistered      State = "registered"
	StateConfiguring     State = "configuring"
	StateSyncing         State = "syncing"
	StateProtected       State = "protected"
	StateDegraded        State = "degraded"
	StateFailoverPending State = "failover_pending"
	StateSwitching       State = "switching"
	StateError           State = "error"
)

// Verdict is the outcome of one evaluation (TM-AI-HA-02 table 5-3).
type Verdict string

const (
	// VerdictUnknown is a group that has not been evaluated yet.
	VerdictUnknown   Verdict = "unknown"
	VerdictOK        Verdict = "ok"
	VerdictAttention Verdict = "attention"
	VerdictError     Verdict = "error"
)

// severity orders verdicts, the worse one higher.
func (v Verdict) severity() int {
	switch v {
	case VerdictError:
		return 2
	case VerdictAttention:
		return 1
	default:
		return 0
	}
}

// Event kinds.
const (
	EventGroupCreated   = "group_created"
	EventStateChanged   = "state_changed"
	EventVerdictChanged = "verdict_changed"
	// EventFlowStarted and EventFlowEnded bracket an HA operation. To names
	// the operation when it starts and its outcome when it ends.
	EventFlowStarted = "flow_started"
	EventFlowEnded   = "flow_ended"
)

var (
	// ErrInvalidRequest reports a request that cannot be accepted as given.
	ErrInvalidRequest = errors.New("invalid HA request")
	// ErrGroupNotFound reports a group no ID or name matches.
	ErrGroupNotFound = errors.New("HA group not found")
	// ErrNameTaken reports a group name already in use.
	ErrNameTaken = errors.New("an HA group with this name already exists")
	// ErrSystemInGroup reports a system that already belongs to a group. A
	// node holds one role, so it can be in one group only.
	ErrSystemInGroup = errors.New("the system already belongs to an HA group")
	// ErrUnknownSystem reports a system no agent has enrolled for.
	ErrUnknownSystem = errors.New("no agent has enrolled for this system")
	// ErrNotAssigned reports a system that belongs to no group. It is not a
	// fault: most systems are not part of one.
	ErrNotAssigned = errors.New("the system belongs to no HA group")
)

// ResourceSpec is the replication resource of a group, without its shared
// secret, which is never kept with the group.
type ResourceSpec struct {
	Name       string `json:"name"`
	Device     string `json:"device"`
	MountPoint string `json:"mountPoint"`
	// Port is the replication port on both nodes.
	Port int `json:"port"`
}

// ServiceSpec is the service a group protects.
type ServiceSpec = model.HAServiceSpec

// NodeSpec is one system of a new group.
type NodeSpec struct {
	SystemID string
	// Address is the node's private IP address.
	Address string
	// Disk is the node's data disk, a /dev/disk/by-id path. It is empty for
	// a group that adopts a resource configured by hand, which then cannot be
	// set up or torn down by the framework.
	Disk string
}

// GroupSpec registers a group. Registering configures nothing on the nodes.
type GroupSpec struct {
	Name     string
	Primary  NodeSpec
	Standby  NodeSpec
	Resource ResourceSpec
	Service  ServiceSpec
	// ServiceAddress is where clients reach the service. It is recorded, not
	// managed: repointing it is the operator's.
	ServiceAddress string
}

// Reason explains a verdict.
type Reason struct {
	// Code identifies the condition. It is stable across evaluations while the
	// condition lasts, unlike Message, which carries figures.
	Code     string  `json:"code"`
	Severity Verdict `json:"severity"`
	// Node is the node the condition is about, empty for the pair.
	Node    Role   `json:"node,omitempty"`
	Message string `json:"message"`
}

// Group is a group as the framework holds it.
type Group struct {
	GroupID        string
	Name           string
	State          State
	Verdict        Verdict
	Reasons        []Reason
	EvaluatedAt    *time.Time
	Resource       ResourceSpec
	Service        ServiceSpec
	ServiceAddress string

	LastSyncConfirmedAt *time.Time
	LinkDownSince       *time.Time
	PrimaryFailingSince *time.Time

	EventSeq  int64
	CreatedBy string
	CreatedAt time.Time
}

// Node is one system of a group, with its latest report under its current role.
type Node struct {
	SystemID string
	Role     Role
	Address  string
	Disk     string
	// NodeID is the node's number within the replication resource. It stays
	// with the system when roles switch.
	NodeID  int
	Fenced  bool
	AgentID string
	// Report is nil until the node reports under this role.
	Report     *model.AgentHealthReq
	ReceivedAt *time.Time
}

// GroupStatus is a group with both its nodes, read together.
type GroupStatus struct {
	Group
	Primary Node
	Standby Node
	// Now is the store's clock when the group was read, so that ages are
	// judged against one clock rather than the caller's.
	Now time.Time
}

// Event is one change in a group, numbered within the group.
type Event struct {
	// EventID names the row; Seq is the order to read and resume by.
	EventID int64
	GroupID string
	Seq     int64
	Kind    string
	From    string
	To      string
	Reasons []Reason
	At      time.Time
}

// NewGroup is a group to insert.
type NewGroup struct {
	GroupID   string
	Spec      GroupSpec
	CreatedBy string
}

// NewEvent is an event to record. Its sequence number is the store's to give.
type NewEvent struct {
	Kind    string
	From    string
	To      string
	Reasons []Reason
}

// Change is the outcome of one evaluation, applied to the group as a whole.
type Change struct {
	// State is the state to move to. Equal to the current state, nothing
	// moves.
	State               State
	Verdict             Verdict
	Reasons             []Reason
	LastSyncConfirmedAt *time.Time
	LinkDownSince       *time.Time
	PrimaryFailingSince *time.Time
	Events              []NewEvent
}

// Decide computes the change an evaluation makes to a group. The store calls
// it with the group locked, so that the decision and its effects are one step
// that no other evaluation or report can interleave with.
type Decide func(GroupStatus) Change

// Store persists HA groups.
type Store interface {
	// Create inserts a group, its two nodes and its first event together. It
	// returns ErrNameTaken or ErrSystemInGroup on a conflict.
	Create(ctx context.Context, g NewGroup) (GroupStatus, error)

	// List returns every group with its nodes, oldest first.
	List(ctx context.Context) ([]GroupStatus, error)

	// Get returns the group whose ID or name is idOrName.
	Get(ctx context.Context, idOrName string) (GroupStatus, error)

	// GroupIDs returns the ID of every group.
	GroupIDs(ctx context.Context) ([]string, error)

	// Events returns a group's events after seq, in sequence order.
	Events(ctx context.Context, groupID string, afterSeq int64, limit int) ([]Event, error)

	// Report records a node's measurements and evaluates its group, as one
	// step under the group's lock.
	//
	// The measurements are recorded only when they were taken under the
	// node's current assignment — the group and role they name are the node's
	// — and the group is evaluated only then. recorded says which happened.
	// It returns ErrNotAssigned for a system in no group.
	Report(ctx context.Context, systemID, agentID string, r model.AgentHealthReq, decide Decide) (gs GroupStatus, recorded bool, err error)

	// Evaluate evaluates a group with no new report, which is how a node that
	// has stopped reporting is noticed.
	Evaluate(ctx context.Context, groupID string, decide Decide) (GroupStatus, error)
}
