// Package inventory records what each protected system is made of, as its
// agent reports it, and answers the one question the framework cannot answer
// on its own: which partitions a backup of that system should capture.
//
// Before this, a backup job had to be created with its partitions named by
// hand. Nothing checked them, so a mistyped device produced a job that looked
// fine and failed once an agent picked it up — or worse, quietly captured less
// than the system needs, which only shows at restore time, when the original
// is gone.
//
// Two things follow from that, and shape this package:
//
//   - An inventory the agent could not complete is not used to choose targets.
//     Refusing to choose is recoverable; choosing wrongly is not.
//   - Being listed is not the same as being usable. A partition is recorded
//     with the reason it cannot be captured, so that naming it by hand is
//     refused with that reason rather than silently accepted.
package inventory

import (
	"context"
	"errors"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
)

// Inventory is a system's configuration as its agent reported it. It is the
// wire contract shared with the agent, so it is defined in the model package.
type Inventory = model.AgentInventoryReq

// Partition is one mounted filesystem within an Inventory.
type Partition = model.InventoryPartition

var (
	// ErrNoInventory reports that this system's agent has not described it
	// yet. It is not an error in the system: a newly enrolled agent has simply
	// not reported.
	ErrNoInventory = errors.New("no inventory has been reported for this system")
	// ErrIncomplete reports an inventory that cannot be used to choose backup
	// targets, because the agent could not account for every filesystem the
	// system needs.
	ErrIncomplete = errors.New("the reported inventory is incomplete")
	// ErrUnknownDevice reports a device this system's inventory does not list.
	ErrUnknownDevice = errors.New("the inventory does not list this device")
	// ErrNotSelectable reports a device the inventory lists as one this agent
	// cannot capture.
	ErrNotSelectable = errors.New("this device cannot be backed up")
	// ErrInvalidRequest reports an inventory report that cannot be accepted.
	ErrInvalidRequest = errors.New("invalid inventory report")
)

// Stored is an inventory as the framework holds it.
type Stored struct {
	SystemID string
	AgentID  string
	// CollectedAt is when the agent read the system; ReportedAt is when the
	// framework heard it. They are kept apart because an agent that reports
	// punctually from a stale reading is a different problem from one that has
	// gone quiet, and only the first can put a vanished partition into a job.
	CollectedAt time.Time
	ReportedAt  time.Time
	Complete    bool
	Inventory   Inventory
	// Now is the store's clock when the row was read, so that age is judged
	// against one clock rather than the caller's.
	Now time.Time
}

// Age is how long ago the agent read the system.
func (s Stored) Age() time.Duration { return s.Now.Sub(s.CollectedAt) }

// Store persists one inventory per system.
type Store interface {
	// Put records a system's inventory, replacing any earlier one. It always
	// moves reported_at forward: an inventory is only as trustworthy as the
	// moment the framework last heard it confirmed.
	Put(ctx context.Context, systemID, agentID string, inv Inventory) error

	// Get returns a system's inventory together with the store's current time.
	// It returns ErrNoInventory when the system has none.
	Get(ctx context.Context, systemID string) (Stored, error)
}
