package inventory

import (
	"context"
	"fmt"
	"time"
)

// maxPartitions bounds one report. A system in the year-one scenario has a
// handful of partitions; a report with a hundred is a bug or an abuse, and
// either way the framework should not store it. It is kept well inside the
// server's body limit, so that a report is refused with a reason rather than
// cut off as too large.
const maxPartitions = 128

// clockSkew is how far into the future a collection time may sit before it is
// refused. Agents and the framework keep their own clocks, and a small
// disagreement is normal; a large one means the timestamp cannot be used to
// judge staleness, which is the only thing it is for.
const clockSkew = 5 * time.Minute

// maxAge is how far into the past a collection time may sit. A report older
// than this was not taken on this run of the agent, and storing it would date
// the inventory to something that never happened.
const maxAge = 24 * time.Hour

// staleAfter is when an inventory stops being treated as a current description
// of the system. It does not refuse anything on its own: an operator is warned
// and the agent re-checks the devices before it writes.
const staleAfter = 24 * time.Hour

// Service records inventories and answers what a backup of a system should
// capture.
type Service struct {
	store Store
}

// NewService returns a Service backed by store.
func NewService(store Store) *Service {
	return &Service{store: store}
}

// StaleAfter is how old an inventory may be before a caller should say so.
func (s *Service) StaleAfter() time.Duration { return staleAfter }

// Report records what an agent says its system is made of.
//
// systemID comes from the caller's authenticated identity, never from the
// report: a body that could name another system would let any valid agent
// token describe someone else's machine.
func (s *Service) Report(ctx context.Context, systemID, agentID string, inv Inventory) error {
	if err := validate(systemID, agentID, inv, time.Now().UTC()); err != nil {
		return err
	}
	return s.store.Put(ctx, systemID, agentID, inv)
}

// Get returns a system's inventory, or ErrNoInventory.
func (s *Service) Get(ctx context.Context, systemID string) (Stored, error) {
	if systemID == "" {
		return Stored{}, fmt.Errorf("%w: a system id is required", ErrInvalidRequest)
	}
	return s.store.Get(ctx, systemID)
}

// BackupDevices returns the partitions a backup of this system should capture,
// with the filesystem UUID expected on each, so that the agent can tell before
// it writes whether the system is still the one the job was built for.
//
// It refuses an inventory the agent marked incomplete. A set chosen from one
// would leave out a filesystem the system needs, and nothing downstream could
// notice: the backup succeeds, and only the restore fails.
func (s *Service) BackupDevices(ctx context.Context, systemID string) ([]string, map[string]string, Stored, error) {
	stored, err := s.Get(ctx, systemID)
	if err != nil {
		return nil, nil, Stored{}, err
	}
	if !stored.Complete {
		return nil, nil, stored, fmt.Errorf("%w: %s", ErrIncomplete, incompleteReason(stored))
	}

	var devices []string
	expected := make(map[string]string)
	for _, p := range stored.Inventory.Partitions {
		if !p.Candidate || p.Device == "" {
			continue
		}
		devices = append(devices, p.Device)
		if p.FSUUID != "" {
			expected[p.Device] = p.FSUUID
		}
	}
	if len(devices) == 0 {
		// Complete and empty should not co-occur, since completeness requires a
		// root filesystem. Refusing rather than trusting the flag keeps a
		// disagreement between the two from producing an empty backup.
		return nil, nil, stored, fmt.Errorf(
			"%w: the inventory names no partition that can be backed up", ErrIncomplete)
	}
	return devices, expected, stored, nil
}

// ValidateDevices refuses devices this system's inventory does not list, and
// devices it lists as ones the agent cannot capture.
//
// Checking only that a device is listed would not be enough: the inventory
// records unusable partitions on purpose, so that an operator can see why they
// were left out. Accepting one because it appears in that list would turn an
// explanation into permission.
func (s *Service) ValidateDevices(ctx context.Context, systemID string, devices []string) (map[string]string, Stored, error) {
	stored, err := s.Get(ctx, systemID)
	if err != nil {
		return nil, Stored{}, err
	}

	// A device can appear more than once: one filesystem mounted in several
	// places is reported once per mount. The selectable row wins, so that an
	// extra mount listed only to explain itself cannot shadow the partition an
	// operator is actually asking for.
	listed := make(map[string]Partition, len(stored.Inventory.Partitions))
	for _, p := range stored.Inventory.Partitions {
		if p.Device == "" {
			continue
		}
		if existing, ok := listed[p.Device]; ok && existing.Selectable {
			continue
		}
		listed[p.Device] = p
	}

	expected := make(map[string]string)
	for _, device := range devices {
		p, ok := listed[device]
		switch {
		case !ok:
			return nil, stored, fmt.Errorf("%w: %s", ErrUnknownDevice, device)
		case !p.Selectable:
			return nil, stored, fmt.Errorf("%w: %s is %s", ErrNotSelectable, device, reasonOf(p))
		}
		if p.FSUUID != "" {
			expected[device] = p.FSUUID
		}
	}
	return expected, stored, nil
}

// Selectable returns the devices an operator may name by hand.
func (s Stored) Selectable() []Partition {
	var out []Partition
	for _, p := range s.Inventory.Partitions {
		if p.Selectable {
			out = append(out, p)
		}
	}
	return out
}

// reasonOf returns a partition's exclusion reason, or a stand-in when the
// agent gave none.
func reasonOf(p Partition) string {
	if p.Reason == "" {
		return "not a partition this agent can back up"
	}
	return p.Reason
}

// incompleteReason returns why an inventory was marked incomplete.
func incompleteReason(s Stored) string {
	if s.Inventory.IncompleteReason != "" {
		return s.Inventory.IncompleteReason
	}
	return "the agent could not account for every filesystem this system needs"
}

// validate checks a report before it replaces the one on file.
//
// The checks are about what the stored row is used for: a duplicate device
// would make device lookup ambiguous, and a collection time that cannot be
// believed makes staleness meaningless, which is the one judgement the
// timestamp exists to support.
func validate(systemID, agentID string, inv Inventory, now time.Time) error {
	switch {
	case systemID == "":
		return fmt.Errorf("%w: a system id is required", ErrInvalidRequest)
	case agentID == "":
		return fmt.Errorf("%w: an agent id is required", ErrInvalidRequest)
	case len(inv.Partitions) == 0:
		return fmt.Errorf("%w: an inventory with no partitions describes nothing", ErrInvalidRequest)
	case len(inv.Partitions) > maxPartitions:
		return fmt.Errorf("%w: at most %d partitions may be reported", ErrInvalidRequest, maxPartitions)
	case inv.CollectedAt.IsZero():
		return fmt.Errorf("%w: a collection time is required", ErrInvalidRequest)
	case inv.CollectedAt.After(now.Add(clockSkew)):
		return fmt.Errorf("%w: the collection time is in the future", ErrInvalidRequest)
	case inv.CollectedAt.Before(now.Add(-maxAge)):
		return fmt.Errorf("%w: the collection time is more than %s old", ErrInvalidRequest, maxAge)
	}

	// Uniqueness is required of the selectable partitions only, because that is
	// where it is relied on: a device names one partition when it is looked up.
	//
	// A device may well appear more than once otherwise. One filesystem can be
	// mounted in several places, and the agent reports the extra mounts with
	// the reason they are not backed up, so that an operator can tell an
	// exclusion from an absence. Refusing those would mean a system with any
	// bind mount of a real filesystem could never report at all.
	seen := make(map[string]bool, len(inv.Partitions))
	for _, p := range inv.Partitions {
		if p.MountPoint == "" {
			return fmt.Errorf("%w: every partition needs a mount point", ErrInvalidRequest)
		}
		if p.Device == "" || !p.Selectable {
			continue
		}
		if seen[p.Device] {
			return fmt.Errorf("%w: %s is offered twice", ErrInvalidRequest, p.Device)
		}
		seen[p.Device] = true
	}
	return nil
}
