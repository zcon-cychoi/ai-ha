package ha

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/agent"
)

var (
	// namePattern is a group name. It cannot look like a group ID, so that a
	// lookup by either never has two answers.
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	idPattern   = regexp.MustCompile(`^[0-9a-f]{16}$`)
	// resourcePattern is a replication resource name the tool accepts.
	resourcePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	// unitPattern is a systemd service unit name. It cannot start with a
	// hyphen: the agent passes it to systemctl as an argument.
	unitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.\\@-]{0,246}\.service$`)
)

const (
	// maxExpectedGPUs bounds the GPU expectation. A larger number is a typo.
	maxExpectedGPUs = 64
	// defaultEventLimit and maxEventLimit bound an event listing.
	defaultEventLimit = 50
	maxEventLimit     = 1000
)

// Systems is what the group registry needs from the agent registry: whether a
// system has been enrolled.
type Systems interface {
	GetSystem(ctx context.Context, systemID string) (agent.System, error)
}

// Timings are how often nodes report and when a report counts as missing.
type Timings struct {
	// ReportInterval is what nodes are told to wait between reports.
	ReportInterval time.Duration
	// StaleAfter is how old a node's last report may be before the node
	// counts as silent, and how long a broken link or a failing primary must
	// last before it counts as lasting. It must comfortably exceed
	// ReportInterval, or one late report flips a verdict.
	StaleAfter time.Duration
}

// Service registers HA groups and judges them from their nodes' reports.
type Service struct {
	store   Store
	systems Systems
	timings Timings
	newID   func() string
}

// NewService returns a Service over store.
func NewService(store Store, systems Systems, t Timings, newID func() string) *Service {
	return &Service{store: store, systems: systems, timings: t, newID: newID}
}

// Timings returns the reporting timings.
func (s *Service) Timings() Timings { return s.timings }

// Create registers a group. Registering configures nothing on the nodes: the
// group starts registered, and its nodes learn their roles from the answer to
// their next health report.
func (s *Service) Create(ctx context.Context, spec GroupSpec, requestedBy string) (GroupStatus, error) {
	spec = normalize(spec)
	if err := validateSpec(spec); err != nil {
		return GroupStatus{}, err
	}
	// Checked here rather than left to the first report: a mistyped system
	// would otherwise sit in the group as a node that never reports, reading
	// as an outage rather than as a typo.
	for _, n := range []NodeSpec{spec.Primary, spec.Standby} {
		if _, err := s.systems.GetSystem(ctx, n.SystemID); err != nil {
			if errors.Is(err, agent.ErrSystemNotFound) {
				return GroupStatus{}, fmt.Errorf("%w: %s", ErrUnknownSystem, n.SystemID)
			}
			return GroupStatus{}, fmt.Errorf("look up system %s: %w", n.SystemID, err)
		}
	}

	groupID := s.newID()
	if spec.Resource.Name == "" {
		// Issued from the group ID so that it is unique among the groups this
		// framework manages. A resource that already exists is adopted by
		// naming it instead.
		spec.Resource.Name = "aiha-" + groupID[:8]
	}
	return s.store.Create(ctx, NewGroup{GroupID: groupID, Spec: spec, CreatedBy: requestedBy})
}

// List returns every group with its nodes.
func (s *Service) List(ctx context.Context) ([]GroupStatus, error) {
	return s.store.List(ctx)
}

// Get returns the group whose ID or name is idOrName.
func (s *Service) Get(ctx context.Context, idOrName string) (GroupStatus, error) {
	if idOrName == "" {
		return GroupStatus{}, fmt.Errorf("%w: a group ID or name is required", ErrInvalidRequest)
	}
	return s.store.Get(ctx, idOrName)
}

// Events returns a group's events after afterSeq, in sequence order. A reader
// resumes from the last sequence number it saw.
func (s *Service) Events(ctx context.Context, idOrName string, afterSeq int64, limit int) (GroupStatus, []Event, error) {
	switch {
	case afterSeq < 0:
		return GroupStatus{}, nil, fmt.Errorf("%w: the sequence to resume after cannot be negative", ErrInvalidRequest)
	case limit < 0 || limit > maxEventLimit:
		return GroupStatus{}, nil, fmt.Errorf("%w: at most %d events may be asked for", ErrInvalidRequest, maxEventLimit)
	case limit == 0:
		limit = defaultEventLimit
	}
	gs, err := s.Get(ctx, idOrName)
	if err != nil {
		return GroupStatus{}, nil, err
	}
	events, err := s.store.Events(ctx, gs.GroupID, afterSeq, limit)
	if err != nil {
		return GroupStatus{}, nil, err
	}
	return gs, events, nil
}

// Report records a node's measurements and answers with its assignment.
//
// systemID comes from the caller's authenticated identity, never from the
// report: a body that could name another system would let any valid agent
// token report for someone else's node, and make a dead primary look alive.
func (s *Service) Report(ctx context.Context, systemID, agentID string, req model.AgentHealthReq) (model.AgentHealthResp, error) {
	if err := validateReport(systemID, agentID, req); err != nil {
		return model.AgentHealthResp{}, err
	}

	resp := model.AgentHealthResp{ReportIntervalSeconds: int(s.timings.ReportInterval / time.Second)}
	gs, _, err := s.store.Report(ctx, systemID, agentID, req, s.decide)
	switch {
	case errors.Is(err, ErrNotAssigned):
		return resp, nil
	case err != nil:
		return model.AgentHealthResp{}, err
	}

	resp.Assigned = true
	resp.Assignment = assignmentFor(gs, systemID)
	return resp, nil
}

// EvaluateAll evaluates every group with no new report. It is what notices a
// node that has stopped reporting: no report arrives to trigger it.
//
// A group that fails is logged by the caller through the returned error and
// does not stop the others from being evaluated.
func (s *Service) EvaluateAll(ctx context.Context) (int, error) {
	ids, err := s.store.GroupIDs(ctx)
	if err != nil {
		return 0, err
	}
	var errs []error
	evaluated := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return evaluated, ctx.Err()
		}
		if _, err := s.store.Evaluate(ctx, id, s.decide); err != nil {
			errs = append(errs, fmt.Errorf("evaluate group %s: %w", id, err))
			continue
		}
		evaluated++
	}
	return evaluated, errors.Join(errs...)
}

// decide is the evaluation the store applies under the group's lock.
func (s *Service) decide(gs GroupStatus) Change {
	return Assess(gs, s.timings.StaleAfter)
}

// assignmentFor tells one node of gs what it is to check.
func assignmentFor(gs GroupStatus, systemID string) *model.HAAssignment {
	self, peer := gs.Primary, gs.Standby
	if gs.Standby.SystemID == systemID {
		self, peer = gs.Standby, gs.Primary
	}
	a := &model.HAAssignment{
		GroupID: gs.GroupID,
		Role:    string(self.Role),
		Service: gs.Service,
		Resource: model.HAAssignedResource{
			Name:       gs.Resource.Name,
			Device:     gs.Resource.Device,
			MountPoint: gs.Resource.MountPoint,
		},
	}
	if self.Role == RoleStandby {
		// The standby is where the service would move to, so its view of the
		// primary is the one worth having.
		a.Probe = &model.HAProbeTarget{
			Address:    peer.Address,
			Port:       gs.Service.Port,
			HealthPath: gs.Service.HealthPath,
		}
	}
	return a
}

// normalize trims what an operator may have typed with surrounding space.
func normalize(spec GroupSpec) GroupSpec {
	spec.Name = strings.TrimSpace(spec.Name)
	spec.Primary.SystemID = strings.TrimSpace(spec.Primary.SystemID)
	spec.Standby.SystemID = strings.TrimSpace(spec.Standby.SystemID)
	spec.Primary.Address = strings.TrimSpace(spec.Primary.Address)
	spec.Standby.Address = strings.TrimSpace(spec.Standby.Address)
	spec.Primary.Disk = strings.TrimSpace(spec.Primary.Disk)
	spec.Standby.Disk = strings.TrimSpace(spec.Standby.Disk)
	spec.Resource.Name = strings.TrimSpace(spec.Resource.Name)
	spec.Service.Unit = strings.TrimSpace(spec.Service.Unit)
	spec.ServiceAddress = strings.TrimSpace(spec.ServiceAddress)
	return spec
}

// validateSpec checks a group definition before it is stored.
//
// What is checked is what would otherwise fail later and far from here: an
// address replication cannot use, a mount point that is not one, a latency
// limit that can never be exceeded.
func validateSpec(spec GroupSpec) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(format, args...))
	}
	switch {
	case !namePattern.MatchString(spec.Name):
		return invalid("a name is lowercase letters, digits and hyphens, at most 63, starting with a letter or digit")
	case idPattern.MatchString(spec.Name):
		return invalid("a name cannot look like a group ID")
	case spec.Primary.SystemID == "" || spec.Standby.SystemID == "":
		return invalid("a primary and a standby system are required")
	case spec.Primary.SystemID == spec.Standby.SystemID:
		return invalid("the primary and the standby must be different systems")
	}

	pAddr, err := netip.ParseAddr(spec.Primary.Address)
	if err != nil {
		return invalid("the primary's address must be an IP address")
	}
	sAddr, err := netip.ParseAddr(spec.Standby.Address)
	if err != nil {
		return invalid("the standby's address must be an IP address")
	}
	if pAddr == sAddr {
		return invalid("the primary and the standby must have different addresses")
	}
	for _, a := range []netip.Addr{pAddr, sAddr} {
		if a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() {
			return invalid("%s cannot be a node's address", a)
		}
	}

	switch {
	case (spec.Primary.Disk == "") != (spec.Standby.Disk == ""):
		return invalid("give both nodes' data disks, or neither for a resource configured by hand")
	case spec.Primary.Disk != "" && spec.Resource.Name != "":
		// A named resource is adopted as it is. Setting one up from given
		// disks under a name chosen by hand would let a typo collide with a
		// resource the node already runs.
		return invalid("data disks are for a resource the framework sets up; leave the resource name out")
	}
	for _, d := range []string{spec.Primary.Disk, spec.Standby.Disk} {
		if d != "" && !ByIDPath(d) {
			return invalid("a data disk must be a /dev/disk/by-id path, as the system's inventory lists it: " +
				"a kernel name such as /dev/sdb can name another disk after a reboot")
		}
	}

	r := spec.Resource
	switch {
	case r.Name != "" && !resourcePattern.MatchString(r.Name):
		return invalid("a resource name is letters, digits, hyphens and underscores, at most 63")
	case !strings.HasPrefix(r.Device, "/dev/") || path.Clean(r.Device) != r.Device:
		return invalid("the replicated device must be a clean path under /dev")
	case !strings.HasPrefix(r.MountPoint, "/") || path.Clean(r.MountPoint) != r.MountPoint || r.MountPoint == "/":
		return invalid("the mount point must be a clean absolute path other than /")
	case r.Port < 1 || r.Port > 65535:
		return invalid("the replication port must be 1-65535")
	}

	svc := spec.Service
	switch {
	case !unitPattern.MatchString(svc.Unit):
		return invalid("the service must be a systemd unit name ending in .service")
	case svc.Port < 1 || svc.Port > 65535:
		return invalid("the service port must be 1-65535")
	case svc.Port == r.Port:
		return invalid("the service and replication ports must differ")
	case svc.HealthPath != "" && (!strings.HasPrefix(svc.HealthPath, "/") || len(svc.HealthPath) > 2048 ||
		strings.ContainsAny(svc.HealthPath, " \t\r\n")):
		return invalid("the health path must start with / and contain no spaces")
	case svc.ExpectedGPUs < 0 || svc.ExpectedGPUs > maxExpectedGPUs:
		return invalid("the expected GPU count must be 0-%d", maxExpectedGPUs)
	case svc.MaxHealthLatencyMs < 0:
		return invalid("the health latency limit cannot be negative")
	case time.Duration(svc.MaxHealthLatencyMs)*time.Millisecond >= model.HAHealthAPITimeout:
		return invalid("the health latency limit must be below %s, after which a call counts as failed",
			model.HAHealthAPITimeout)
	case svc.MaxHealthLatencyMs > 0 && svc.HealthPath == "":
		return invalid("a health latency limit needs a health path to measure")
	case len(spec.ServiceAddress) > 2048:
		return invalid("the service address is too long")
	}
	return nil
}

// byIDPrefix is where udev links each disk by a name that survives reboots.
const byIDPrefix = "/dev/disk/by-id/"

// ByIDPath reports whether p is a clean path directly under /dev/disk/by-id.
// It does not look at the name past the prefix: the form differs by cloud,
// and the node checks the link when it sets the volume up.
func ByIDPath(p string) bool {
	name, ok := strings.CutPrefix(p, byIDPrefix)
	return ok && name != "" && !strings.Contains(name, "/") && path.Clean(p) == p
}

// validateReport checks a health report before it is recorded.
func validateReport(systemID, agentID string, req model.AgentHealthReq) error {
	switch {
	case systemID == "":
		return fmt.Errorf("%w: a system id is required", ErrInvalidRequest)
	case agentID == "":
		return fmt.Errorf("%w: an agent id is required", ErrInvalidRequest)
	case req.CollectedAt.IsZero():
		return fmt.Errorf("%w: a collection time is required", ErrInvalidRequest)
	case (req.GroupID == "") != (req.Role == ""):
		// Half an assignment cannot be matched against either the group or
		// the role, so the measurements could not be placed.
		return fmt.Errorf("%w: a group and a role are reported together or not at all", ErrInvalidRequest)
	}
	return nil
}
