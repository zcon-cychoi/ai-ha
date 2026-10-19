package inventory

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaSQL creates the inventory table. It is a single statement.
//
//go:embed schema.sql
var schemaSQL string

// Migrate creates the inventory table if it does not exist. It does not alter
// an existing table.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply inventory schema: %w", err)
	}
	return nil
}

// PGStore is a Store backed by PostgreSQL.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore returns a Store using pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

// A system keeps one inventory, so a repeat report replaces it rather than
// accumulating. reported_at is set on every write, including one that changes
// nothing: it records when the framework last heard this description
// confirmed, which is what makes the row trustworthy.
const putSQL = `
INSERT INTO system_inventories (system_id, agent_id, collected_at, reported_at, complete, inventory)
VALUES ($1, $2, $3, now(), $4, $5)
ON CONFLICT (system_id) DO UPDATE SET
    agent_id     = EXCLUDED.agent_id,
    collected_at = EXCLUDED.collected_at,
    reported_at  = now(),
    complete     = EXCLUDED.complete,
    inventory    = EXCLUDED.inventory`

// Put implements Store.
func (s *PGStore) Put(ctx context.Context, systemID, agentID string, inv Inventory) error {
	encoded, err := json.Marshal(inv)
	if err != nil {
		return fmt.Errorf("encode the inventory: %w", err)
	}
	if _, err := s.pool.Exec(ctx, putSQL,
		systemID, agentID, inv.CollectedAt.UTC(), inv.Complete, encoded,
	); err != nil {
		return fmt.Errorf("store the inventory: %w", err)
	}
	return nil
}

const getSQL = `
SELECT agent_id, collected_at, reported_at, complete, inventory, now()
FROM system_inventories
WHERE system_id = $1`

// Get implements Store.
func (s *PGStore) Get(ctx context.Context, systemID string) (Stored, error) {
	var (
		out     = Stored{SystemID: systemID}
		encoded []byte
	)
	err := s.pool.QueryRow(ctx, getSQL, systemID).Scan(
		&out.AgentID, &out.CollectedAt, &out.ReportedAt, &out.Complete, &encoded, &out.Now)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Stored{}, ErrNoInventory
	case err != nil:
		return Stored{}, fmt.Errorf("read the inventory: %w", err)
	}

	if err := json.Unmarshal(encoded, &out.Inventory); err != nil {
		return Stored{}, fmt.Errorf("decode the stored inventory: %w", err)
	}
	// The timestamps are read from their own columns rather than from the
	// document, so that a report is dated by what the store recorded.
	out.CollectedAt = out.CollectedAt.UTC()
	out.ReportedAt = out.ReportedAt.UTC()
	out.Now = out.Now.UTC()
	return out, nil
}

// Compile-time check that PGStore still satisfies Store.
var _ Store = (*PGStore)(nil)
