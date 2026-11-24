package ha

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
)

// An HA flow is an operation on a group that the framework carries out as a
// sequence of jobs (plan/ha-roadmap.md, "작업 흐름 공통 규칙"):
//
//   - A group runs one flow at a time.
//   - A step and its jobs are written in one transaction, and a step's job is
//     keyed by the flow, the step and the system, so that a restart never
//     issues a step twice.
//   - The next step is issued only after the step's job completed and the
//     node's own reports agree with it. Jobs on different nodes are not
//     ordered by the queue, so "the job said so" is not enough.
//   - A job that fails, or that completes without the node bearing it out,
//     stops the flow and puts the group in error. Nothing is retried: an
//     operator looks, acknowledges and asks again.
//   - A job whose flow has moved on is refused before it starts (job.Flows).

// FlowKind is what a flow does.
type FlowKind string

const (
	// FlowSetup configures replication: the standby first, then the primary,
	// which is promoted, formatted and starts the service.
	FlowSetup FlowKind = "setup"
	// FlowTeardown removes it: the primary first, which stops the service,
	// then the standby. The data and the metadata stay on the disks.
	FlowTeardown FlowKind = "teardown"
)

// FlowStatus is where a flow is.
type FlowStatus string

const (
	FlowRunning   FlowStatus = "running"
	FlowCompleted FlowStatus = "completed"
	FlowFailed    FlowStatus = "failed"
)

// replicationProtocol is DRBD's synchronous protocol (TM-AI-HA-02 table 6-2).
const replicationProtocol = "C"

// secretBytes is the shared secret's length before hex encoding. The tool
// accepts at most 64 characters; 48 leave room and match the H0 runbook.
const secretBytes = 24

var (
	// ErrFlowInProgress reports a group that already runs a flow.
	ErrFlowInProgress = errors.New("the HA group is already in an HA operation")
	// ErrWrongState reports a group whose state does not allow what was asked.
	ErrWrongState = errors.New("the HA group's state does not allow this")
	// ErrSystemBusy reports a node with jobs that have not ended. A flow does
	// not start beside them: a running backup would hold up a step, and its
	// archive would describe a volume that was being reconfigured under it.
	ErrSystemBusy = errors.New("a node of the HA group has unfinished jobs")
	// ErrNodeSilent reports a node that is not reporting, so that the
	// framework cannot know its host name or that its agent would take a job.
	ErrNodeSilent = errors.New("a node of the HA group is not reporting")
	// ErrRolesDisagree reports nodes whose own reports contradict the roles
	// the framework records, which an acknowledgement must not paper over.
	ErrRolesDisagree = errors.New("the nodes' reports do not match their recorded roles")
	// ErrNoSecretKey reports a command that cannot seal or open the peers'
	// shared secret because it was not given the framework's key.
	ErrNoSecretKey = errors.New("the framework's secret key is not loaded")
	// errNoFlow reports a job that belongs to no flow.
	errNoFlow = errors.New("the job belongs to no HA operation")
)

// Flow is an HA operation on a group.
type Flow struct {
	FlowID      string
	GroupID     string
	Kind        FlowKind
	Status      FlowStatus
	Step        int
	Message     string
	RequestedBy string
	StartedAt   time.Time
	UpdatedAt   time.Time
	FinishedAt  *time.Time
	// Jobs are the jobs of every step so far, in step order, with their
	// status as the queue has it.
	Jobs []FlowJob
}

// FlowJob is one job of a flow's step.
type FlowJob struct {
	Step     int
	SystemID string
	JobID    string
	Status   job.Status
	Message  string
}

// Sealer seals and opens the peers' shared secret. *secret.Box is one.
type Sealer interface {
	Seal(plain, aad []byte) ([]byte, error)
	Open(sealed, aad []byte) ([]byte, error)
}

// secretAAD binds a sealed secret to its group, and keeps it from opening as
// anything else the framework seals.
func secretAAD(groupID string) []byte {
	return []byte("ha-group:" + groupID)
}

// StartContext is what a flow is planned from: the group, locked, with what
// else is going on around it.
type StartContext struct {
	Group GroupStatus
	// Running is the group's flow in progress, if any.
	Running *Flow
	// Active are the unfinished jobs of the group's systems.
	Active []job.Job
	// SealedSecret is the group's shared secret, sealed, if it has one.
	SealedSecret []byte
}

// FlowStart is a flow to begin, with the jobs of its first step.
type FlowStart struct {
	FlowID      string
	Kind        FlowKind
	RequestedBy string
	// To is the group's state while the flow runs.
	To State
	// SealedSecret, when set, is stored as the group's shared secret.
	SealedSecret []byte
	Jobs         []PlannedJob
}

// PlannedJob is a job a flow step issues.
type PlannedJob struct {
	JobID    string
	SystemID string
	Kind     job.Kind
	Spec     json.RawMessage
}

// StepJob is a job of the step a flow is on, as the queue has it.
type StepJob struct {
	FlowJob
	Job     job.Job
	Attempt *job.Attempt
}

// AdvanceContext is what a flow's next move is decided from.
type AdvanceContext struct {
	Group GroupStatus
	Flow  Flow
	// Step are the jobs of the step the flow is on.
	Step []StepJob
}

// MoveKind is what a flow does next.
type MoveKind int

const (
	// MoveWait leaves the flow where it is.
	MoveWait MoveKind = iota
	// MoveNext issues the next step's jobs.
	MoveNext
	// MoveComplete ends the flow and moves the group to To.
	MoveComplete
	// MoveFail ends the flow and puts the group in error.
	MoveFail
)

// FlowMove is the outcome of one look at a flow.
type FlowMove struct {
	Kind    MoveKind
	Message string
	// Jobs are the next step's, for MoveNext.
	Jobs []PlannedJob
	// To is the group's state once the flow completes.
	To State
}

// AckContext is what an acknowledgement is decided from.
type AckContext struct {
	Group  GroupStatus
	Latest *Flow
	Active []job.Job
}

// FlowStore is the persistence the flows need. *PGStore implements it.
type FlowStore interface {
	StartFlow(ctx context.Context, idOrName string, plan func(StartContext) (FlowStart, error), ttl time.Duration) (GroupStatus, Flow, error)
	RunningFlowIDs(ctx context.Context) ([]string, error)
	AdvanceFlow(ctx context.Context, flowID string, decide func(AdvanceContext) (FlowMove, error), ttl time.Duration) error
	FlowOfJob(ctx context.Context, jobID string) (FlowRef, error)
	SystemInFlow(ctx context.Context, systemID string) (bool, error)
	Acknowledge(ctx context.Context, idOrName string, decide func(AckContext) (State, error), by string) (GroupStatus, error)
	LatestFlow(ctx context.Context, groupID string) (*Flow, error)
}

// FlowRef is the flow a job belongs to.
type FlowRef struct {
	FlowID       string
	GroupID      string
	Kind         FlowKind
	Status       FlowStatus
	Step         int
	JobStep      int
	SealedSecret []byte
}

// FlowService runs HA flows. It is also what the job queue asks about HA
// jobs (job.Flows).
type FlowService struct {
	store FlowStore
	// box is nil for commands that neither set up nor claim: they never seal
	// or open the secret.
	box     Sealer
	timings Timings
	jobTTL  time.Duration
	newID   func() string
	// secret makes a new shared secret.
	secret func() (string, error)
}

// NewFlowService returns a FlowService over store. box may be nil for a
// command that only reads; jobTTL is how long an HA job waits for its agent.
func NewFlowService(store FlowStore, box Sealer, t Timings, jobTTL time.Duration, newID func() string) *FlowService {
	return &FlowService{store: store, box: box, timings: t, jobTTL: jobTTL, newID: newID, secret: newSecret}
}

// newSecret draws a shared secret.
func newSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("draw a shared secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Compile-time check that FlowService answers the job queue's questions.
var _ job.Flows = (*FlowService)(nil)

// Setup starts replication setup on a registered group: the standby's side
// first. The group is configuring until both sides are up, then syncing.
func (f *FlowService) Setup(ctx context.Context, idOrName, requestedBy string) (GroupStatus, Flow, error) {
	flowID := f.newID()
	return f.store.StartFlow(ctx, idOrName, func(c StartContext) (FlowStart, error) {
		gs := c.Group
		if err := f.startable(c, FlowSetup, StateRegistered); err != nil {
			return FlowStart{}, err
		}
		if gs.Primary.Disk == "" || gs.Standby.Disk == "" {
			return FlowStart{}, fmt.Errorf("%w: the group was registered without data disks, for a resource "+
				"configured by hand; register one with --primary-disk and --standby-disk to have it set up", ErrInvalidRequest)
		}
		sealed, err := f.sealedSecret(gs.GroupID, c.SealedSecret)
		if err != nil {
			return FlowStart{}, err
		}
		spec, err := setupSpec(gs, flowID, 1, gs.Standby, gs.Primary, false, 0)
		if err != nil {
			return FlowStart{}, err
		}
		start := FlowStart{
			FlowID: flowID, Kind: FlowSetup, RequestedBy: requestedBy, To: StateConfiguring,
			Jobs: []PlannedJob{{JobID: f.newID(), SystemID: gs.Standby.SystemID, Kind: job.KindReplicationSetup, Spec: spec}},
		}
		if c.SealedSecret == nil {
			start.SealedSecret = sealed
		}
		return start, nil
	}, f.jobTTL)
}

// Teardown starts removing replication from a group: the primary's side
// first, so that the service stops before its standby goes. The group is
// configuring until both sides are down, then registered.
func (f *FlowService) Teardown(ctx context.Context, idOrName, requestedBy string) (GroupStatus, Flow, error) {
	flowID := f.newID()
	return f.store.StartFlow(ctx, idOrName, func(c StartContext) (FlowStart, error) {
		gs := c.Group
		if err := f.startable(c, FlowTeardown, StateRegistered, StateSyncing, StateProtected, StateDegraded); err != nil {
			return FlowStart{}, err
		}
		spec, err := teardownSpec(gs, flowID, 1)
		if err != nil {
			return FlowStart{}, err
		}
		return FlowStart{
			FlowID: flowID, Kind: FlowTeardown, RequestedBy: requestedBy, To: StateConfiguring,
			Jobs: []PlannedJob{{JobID: f.newID(), SystemID: gs.Primary.SystemID, Kind: job.KindReplicationTeardown, Spec: spec}},
		}, nil
	}, f.jobTTL)
}

// startable checks what every flow needs before it starts.
func (f *FlowService) startable(c StartContext, kind FlowKind, from ...State) error {
	gs := c.Group
	switch {
	case c.Running != nil:
		return fmt.Errorf("%w: %s %s is still running", ErrFlowInProgress, c.Running.Kind, c.Running.FlowID)
	case !slices.Contains(from, gs.State):
		return fmt.Errorf("%w: the group is %s, and %s starts from %s",
			ErrWrongState, gs.State, kind, joinStates(from))
	case len(c.Active) > 0:
		ids := make([]string, 0, len(c.Active))
		for _, j := range c.Active {
			ids = append(ids, fmt.Sprintf("%s (%s, %s)", j.JobID, j.Kind, j.Status))
		}
		return fmt.Errorf("%w: wait for %s", ErrSystemBusy, strings.Join(ids, ", "))
	}
	// A node that is not reporting would leave its job queued until it
	// expired; asking now is the quicker way to learn that. A node's host
	// name also comes from its report, and the resource file needs it.
	for _, n := range []Node{gs.Primary, gs.Standby} {
		if !fresh(n, gs.Now, f.timings.StaleAfter) || n.Report.Hostname == "" {
			return fmt.Errorf("%w: the %s (%s) has not reported in the last %s", ErrNodeSilent, n.Role, n.SystemID,
				f.timings.StaleAfter)
		}
	}
	return nil
}

// sealedSecret returns the group's shared secret, sealed: the one it has,
// once it is known to open under this key, or a new one.
func (f *FlowService) sealedSecret(groupID string, sealed []byte) ([]byte, error) {
	if f.box == nil {
		return nil, ErrNoSecretKey
	}
	if sealed != nil {
		// Checked now rather than at claim time, when the only sign of a key
		// that changed would be every job of the flow failing.
		if _, err := f.box.Open(sealed, secretAAD(groupID)); err != nil {
			return nil, fmt.Errorf("open the group's shared secret: %w", err)
		}
		return sealed, nil
	}
	plain, err := f.secret()
	if err != nil {
		return nil, err
	}
	out, err := f.box.Seal([]byte(plain), secretAAD(groupID))
	if err != nil {
		return nil, fmt.Errorf("seal the group's shared secret: %w", err)
	}
	return out, nil
}

// Acknowledge clears a group's error once the nodes allow it: no HA job left
// to run, and no node claiming a role the framework does not record for it.
//
// A group whose last flow failed goes back to registered, from where setup or
// teardown can be asked for again: both redo only what is not already so. Any
// other error, such as two primaries, goes to degraded, and the evaluator
// judges the group from there.
func (f *FlowService) Acknowledge(ctx context.Context, idOrName, by string) (GroupStatus, error) {
	return f.store.Acknowledge(ctx, idOrName, func(c AckContext) (State, error) {
		gs := c.Group
		if gs.State != StateError {
			return "", fmt.Errorf("%w: the group is %s, and only an error is acknowledged", ErrWrongState, gs.State)
		}
		var waiting []string
		for _, j := range c.Active {
			if j.Kind.HA() {
				waiting = append(waiting, fmt.Sprintf("%s (%s, %s)", j.JobID, j.Kind, j.Status))
			}
		}
		if len(waiting) > 0 {
			return "", fmt.Errorf("%w: wait for %s", ErrSystemBusy, strings.Join(waiting, ", "))
		}
		if s := gs.Standby; fresh(s, gs.Now, f.timings.StaleAfter) && s.Report.Replication != nil &&
			s.Report.Replication.Error == "" && s.Report.Replication.Role == "Primary" {
			return "", fmt.Errorf("%w: the standby (%s) reports itself Primary; "+
				"demote it by hand, or switch roles, before acknowledging", ErrRolesDisagree, s.SystemID)
		}
		if c.Latest != nil && c.Latest.Status == FlowFailed {
			return StateRegistered, nil
		}
		return StateDegraded, nil
	}, by)
}

// LatestFlow returns the group's most recent flow, nil if it has had none.
func (f *FlowService) LatestFlow(ctx context.Context, groupID string) (*Flow, error) {
	return f.store.LatestFlow(ctx, groupID)
}

// AdvanceAll moves every running flow on as far as its jobs and its nodes'
// reports allow. A flow that fails to advance is reported through the error
// and does not hold up the others.
func (f *FlowService) AdvanceAll(ctx context.Context) (int, error) {
	ids, err := f.store.RunningFlowIDs(ctx)
	if err != nil {
		return 0, err
	}
	var errs []error
	advanced := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return advanced, ctx.Err()
		}
		if err := f.store.AdvanceFlow(ctx, id, f.decide, f.jobTTL); err != nil {
			errs = append(errs, fmt.Errorf("advance HA operation %s: %w", id, err))
			continue
		}
		advanced++
	}
	return advanced, errors.Join(errs...)
}

// decide is one look at a running flow.
func (f *FlowService) decide(c AdvanceContext) (FlowMove, error) {
	return DecideFlow(c, f.timings.StaleAfter, f.newID)
}

// DecideFlow is the next move of a running flow. It is a pure function of its
// arguments apart from newID, so that the rules can be tested as a table.
//
// A step's job has to complete and then the node has to bear it out in a
// report received after the job ended. A node that does not within
// staleAfter stops the flow: the job said one thing and the node, as far as
// anyone can tell, another.
func DecideFlow(c AdvanceContext, staleAfter time.Duration, newID func() string) (FlowMove, error) {
	if len(c.Step) != 1 {
		return FlowMove{}, fmt.Errorf("step %d of %s has %d jobs; each step has one", c.Flow.Step, c.Flow.FlowID, len(c.Step))
	}
	sj := c.Step[0]
	node, ok := nodeOf(c.Group, sj.SystemID)
	if !ok {
		return FlowMove{}, fmt.Errorf("system %s of step %d is not a node of the group", sj.SystemID, sj.Step)
	}
	what := fmt.Sprintf("%s step %d on the %s", c.Flow.Kind, sj.Step, node.Role)

	switch sj.Job.Status {
	case job.StatusCompleted:
	case job.StatusFailed, job.StatusCancelled:
		msg := sj.Job.Message
		if msg == "" {
			msg = "no reason given"
		}
		return FlowMove{Kind: MoveFail, Message: fmt.Sprintf("The %s failed: %s", what, msg)}, nil
	default:
		return FlowMove{Kind: MoveWait}, nil
	}

	if sj.Job.FinishedAt == nil {
		return FlowMove{}, fmt.Errorf("job %s completed without a finish time", sj.Job.JobID)
	}
	finished := *sj.Job.FinishedAt
	expect, borne := bearsOut(c.Flow.Kind, sj.Step, node, finished)
	if !borne {
		if c.Group.Now.Sub(finished) <= staleAfter {
			return FlowMove{Kind: MoveWait}, nil
		}
		return FlowMove{Kind: MoveFail, Message: fmt.Sprintf(
			"The %s reported success, but the node's reports in the %s since do not show %s",
			what, staleAfter, expect)}, nil
	}

	switch {
	case c.Flow.Kind == FlowSetup && sj.Step == 1:
		spec, err := primarySetupSpec(sj)
		if err != nil {
			return FlowMove{}, err
		}
		return FlowMove{Kind: MoveNext, Message: "Standby set up; setting up the primary", Jobs: []PlannedJob{{
			JobID: newID(), SystemID: c.Group.Primary.SystemID, Kind: job.KindReplicationSetup, Spec: spec,
		}}}, nil
	case c.Flow.Kind == FlowSetup:
		return FlowMove{Kind: MoveComplete, To: StateSyncing,
			Message: "Both sides set up; the standby is catching up"}, nil
	case c.Flow.Kind == FlowTeardown && sj.Step == 1:
		spec, err := teardownSpec(c.Group, c.Flow.FlowID, 2)
		if err != nil {
			return FlowMove{}, err
		}
		return FlowMove{Kind: MoveNext, Message: "Primary taken down; taking the standby down", Jobs: []PlannedJob{{
			JobID: newID(), SystemID: c.Group.Standby.SystemID, Kind: job.KindReplicationTeardown, Spec: spec,
		}}}, nil
	default:
		return FlowMove{Kind: MoveComplete, To: StateRegistered,
			Message: "Replication removed; the data and its metadata are still on the disks"}, nil
	}
}

// bearsOut reports whether the node's report, received after its job ended,
// shows what the step was to achieve, and describes that for a message.
//
// The report need not have been measured after the job ended. What it shows
// did not hold before the job ran — the resource up in this role, or gone —
// so a report showing it is evidence either way.
func bearsOut(kind FlowKind, step int, n Node, finished time.Time) (string, bool) {
	var expect string
	switch {
	case kind == FlowSetup && step == 1:
		expect = "the resource up as Secondary"
	case kind == FlowSetup:
		expect = "the resource up as Primary"
	default:
		expect = "the resource gone"
	}
	if n.Report == nil || n.ReceivedAt == nil || !n.ReceivedAt.After(finished) {
		return expect, false
	}
	r := n.Report.Replication
	readable := r != nil && r.Error == ""
	switch {
	case kind == FlowSetup && step == 1:
		return expect, readable && r.Role == "Secondary"
	case kind == FlowSetup:
		return expect, readable && r.Role == "Primary"
	default:
		return expect, !readable
	}
}

// nodeOf returns the node of gs that systemID is.
func nodeOf(gs GroupStatus, systemID string) (Node, bool) {
	switch systemID {
	case gs.Primary.SystemID:
		return gs.Primary, true
	case gs.Standby.SystemID:
		return gs.Standby, true
	}
	return Node{}, false
}

// replicationNode describes n for a resource file.
func replicationNode(n Node) model.ReplicationNode {
	host := ""
	if n.Report != nil {
		host = n.Report.Hostname
	}
	return model.ReplicationNode{Hostname: host, Address: n.Address, NodeID: n.NodeID, Disk: n.Disk}
}

// setupSpec is the job that sets self's side up.
func setupSpec(gs GroupStatus, flowID string, step int, self, peer Node, initialPrimary bool, peerDiskBytes int64) (json.RawMessage, error) {
	spec := model.ReplicationSetupSpec{
		FlowID:         flowID,
		Step:           step,
		Resource:       gs.Resource.Name,
		Device:         gs.Resource.Device,
		MountPoint:     gs.Resource.MountPoint,
		Port:           gs.Resource.Port,
		Protocol:       replicationProtocol,
		ServiceUnit:    gs.Service.Unit,
		Self:           replicationNode(self),
		Peer:           replicationNode(peer),
		InitialPrimary: initialPrimary,
		PeerDiskBytes:  peerDiskBytes,
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("encode the setup job: %w", err)
	}
	return b, nil
}

// primarySetupSpec is the primary's setup job, made from the standby's. The
// two describe the same resource, so the primary's is the standby's turned
// around: built from the group again, a host name reported in between would
// give the two nodes different resource files.
func primarySetupSpec(standby StepJob) (json.RawMessage, error) {
	var spec model.ReplicationSetupSpec
	if err := json.Unmarshal(standby.Job.Spec, &spec); err != nil {
		return nil, fmt.Errorf("read the standby's setup job %s: %w", standby.Job.JobID, err)
	}
	var result model.ReplicationJobResult
	if standby.Attempt != nil && len(standby.Attempt.Result) > 0 {
		if err := json.Unmarshal(standby.Attempt.Result, &result); err != nil {
			return nil, fmt.Errorf("read the result of the standby's setup job %s: %w", standby.Job.JobID, err)
		}
	}
	spec.Step = 2
	spec.Self, spec.Peer = spec.Peer, spec.Self
	spec.InitialPrimary = true
	spec.PeerDiskBytes = result.DiskBytes
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("encode the setup job: %w", err)
	}
	return b, nil
}

// teardownSpec is the job that takes a node's side down.
func teardownSpec(gs GroupStatus, flowID string, step int) (json.RawMessage, error) {
	b, err := json.Marshal(model.ReplicationTeardownSpec{
		FlowID:      flowID,
		Step:        step,
		Resource:    gs.Resource.Name,
		MountPoint:  gs.Resource.MountPoint,
		ServiceUnit: gs.Service.Unit,
	})
	if err != nil {
		return nil, fmt.Errorf("encode the teardown job: %w", err)
	}
	return b, nil
}

// joinStates lists states for a message.
func joinStates(states []State) string {
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = string(s)
	}
	return strings.Join(out, ", ")
}

// InProgress implements job.Flows.
func (f *FlowService) InProgress(ctx context.Context, systemID string) (bool, error) {
	return f.store.SystemInFlow(ctx, systemID)
}

// Admit implements job.Flows. A setup job is given the peers' shared secret,
// which is opened here and nowhere else; it is never written into a job.
func (f *FlowService) Admit(ctx context.Context, j job.Job) (*model.ReplicationAccess, error) {
	ref, err := f.current(ctx, j)
	if err != nil {
		return nil, err
	}
	if j.Kind != job.KindReplicationSetup {
		return nil, nil
	}
	if f.box == nil {
		return nil, ErrNoSecretKey
	}
	if ref.SealedSecret == nil {
		return nil, fmt.Errorf("group %s has no shared secret for setup job %s", ref.GroupID, j.JobID)
	}
	plain, err := f.box.Open(ref.SealedSecret, secretAAD(ref.GroupID))
	if err != nil {
		return nil, fmt.Errorf("open the shared secret of group %s: %w", ref.GroupID, err)
	}
	return &model.ReplicationAccess{SharedSecret: string(plain)}, nil
}

// Current implements job.Flows.
func (f *FlowService) Current(ctx context.Context, j job.Job) error {
	_, err := f.current(ctx, j)
	return err
}

// current returns the flow j belongs to when j is still that flow's step, and
// job.ErrFlowMoved when it is not.
func (f *FlowService) current(ctx context.Context, j job.Job) (FlowRef, error) {
	ref, err := f.store.FlowOfJob(ctx, j.JobID)
	switch {
	case errors.Is(err, errNoFlow):
		return FlowRef{}, fmt.Errorf("%w: %w", job.ErrFlowMoved, err)
	case err != nil:
		return FlowRef{}, err
	case ref.Status != FlowRunning:
		return FlowRef{}, fmt.Errorf("%w: %s %s is %s", job.ErrFlowMoved, ref.Kind, ref.FlowID, ref.Status)
	case ref.Step != ref.JobStep:
		return FlowRef{}, fmt.Errorf("%w: %s %s is on step %d, and this job is step %d",
			job.ErrFlowMoved, ref.Kind, ref.FlowID, ref.Step, ref.JobStep)
	}
	return ref, nil
}
