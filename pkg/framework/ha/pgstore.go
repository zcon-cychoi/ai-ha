package ha

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

const (
	uniqueViolation  = "23505"
	nameConstraint   = "ha_groups_name_unique"
	systemConstraint = "ha_nodes_system_unique"
)

// Migrate creates the HA tables if they do not exist.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply HA schema: %w", err)
	}
	return nil
}

// PGStore is a Store backed by PostgreSQL.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore returns a Store using pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// groupColumns is the column list every group read shares. now() is read
// with the row so that ages are judged against the database's clock.
const groupColumns = `g.group_id, g.name, g.state, g.verdict, g.reasons, g.evaluated_at,
                      g.resource, g.service, g.service_address,
                      g.last_sync_confirmed_at, g.link_down_since, g.primary_failing_since,
                      g.event_seq, g.created_by, g.created_at, now()`

const nodeColumns = `system_id, role, address, COALESCE(disk, ''), COALESCE(node_id, 0),
                     fenced, agent_id, report, report_received_at`

// Create implements Store.
func (s *PGStore) Create(ctx context.Context, g NewGroup) (GroupStatus, error) {
	resource, err := json.Marshal(g.Spec.Resource)
	if err != nil {
		return GroupStatus{}, fmt.Errorf("encode the resource: %w", err)
	}
	service, err := json.Marshal(g.Spec.Service)
	if err != nil {
		return GroupStatus{}, fmt.Errorf("encode the service: %w", err)
	}

	var gs GroupStatus
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO ha_groups (group_id, name, state, resource, service, service_address, event_seq, created_by)
VALUES ($1, $2, $3, $4, $5, $6, 1, $7)`,
			g.GroupID, g.Spec.Name, string(StateRegistered), resource, service,
			nullable(g.Spec.ServiceAddress), nullable(g.CreatedBy)); err != nil {
			return err
		}
		// The node numbers follow the roles the group is registered with.
		// They stay with the systems from here on, whatever the roles become.
		if _, err := tx.Exec(ctx, `
INSERT INTO ha_nodes (system_id, group_id, role, address, disk, node_id)
VALUES ($1, $3, 'primary', $2, $6, 0), ($4, $3, 'standby', $5, $7, 1)`,
			g.Spec.Primary.SystemID, g.Spec.Primary.Address, g.GroupID,
			g.Spec.Standby.SystemID, g.Spec.Standby.Address,
			nullable(g.Spec.Primary.Disk), nullable(g.Spec.Standby.Disk)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO ha_events (group_id, seq, kind, to_value) VALUES ($1, 1, $2, $3)`,
			g.GroupID, EventGroupCreated, string(StateRegistered)); err != nil {
			return err
		}
		gs, err = loadGroup(ctx, tx, `WHERE g.group_id = $1`, g.GroupID)
		return err
	})
	switch {
	case isUniqueViolation(err, nameConstraint):
		return GroupStatus{}, fmt.Errorf("%w: %s", ErrNameTaken, g.Spec.Name)
	case isUniqueViolation(err, systemConstraint):
		return GroupStatus{}, ErrSystemInGroup
	case err != nil:
		return GroupStatus{}, fmt.Errorf("create the HA group: %w", err)
	}
	return gs, nil
}

// List implements Store.
func (s *PGStore) List(ctx context.Context) ([]GroupStatus, error) {
	var out []GroupStatus
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+groupColumns+` FROM ha_groups g ORDER BY g.created_at, g.group_id`)
		if err != nil {
			return err
		}
		groups, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (GroupStatus, error) {
			return scanGroup(row)
		})
		if err != nil {
			return err
		}
		for i := range groups {
			if err := loadNodes(ctx, tx, &groups[i]); err != nil {
				return err
			}
		}
		out = groups
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list HA groups: %w", err)
	}
	return out, nil
}

// Get implements Store. An ID match wins over a name match, though names are
// refused that look like IDs, so that the two cannot meet.
func (s *PGStore) Get(ctx context.Context, idOrName string) (GroupStatus, error) {
	var gs GroupStatus
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		gs, err = loadGroup(ctx, tx,
			`WHERE g.group_id = $1 OR g.name = $1 ORDER BY (g.group_id = $1) DESC LIMIT 1`, idOrName)
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return GroupStatus{}, ErrGroupNotFound
	case err != nil:
		return GroupStatus{}, fmt.Errorf("read the HA group: %w", err)
	}
	return gs, nil
}

// GroupIDs implements Store.
func (s *PGStore) GroupIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT group_id FROM ha_groups ORDER BY created_at, group_id`)
	if err != nil {
		return nil, fmt.Errorf("list HA group ids: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list HA group ids: %w", err)
	}
	return ids, nil
}

// Events implements Store.
func (s *PGStore) Events(ctx context.Context, groupID string, afterSeq int64, limit int) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
SELECT event_id, group_id, seq, kind, from_value, to_value, reasons, at
  FROM ha_events
 WHERE group_id = $1 AND seq > $2
 ORDER BY seq
 LIMIT $3`, groupID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list HA events: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var (
			e        Event
			from, to *string
			reasons  []byte
		)
		if err := row.Scan(&e.EventID, &e.GroupID, &e.Seq, &e.Kind, &from, &to, &reasons, &e.At); err != nil {
			return Event{}, err
		}
		e.From, e.To = deref(from), deref(to)
		if err := json.Unmarshal(reasons, &e.Reasons); err != nil {
			return Event{}, fmt.Errorf("decode the reasons of event %d: %w", e.EventID, err)
		}
		e.At = e.At.UTC()
		return e, nil
	})
	if err != nil {
		return nil, fmt.Errorf("list HA events: %w", err)
	}
	return events, nil
}

// recordReportSQL stores a report only when it was measured under the node's
// current assignment. A report for another group or role is not this node's
// current state, and judging it against the new role would be wrong.
const recordReportSQL = `
UPDATE ha_nodes
   SET report = $4, report_collected_at = $5, report_received_at = now(), agent_id = $6
 WHERE system_id = $1 AND group_id = $2 AND role = $3`

// Report implements Store.
//
// The group's row is locked first and held to the end. That is what makes
// the two nodes' reports, and the server's own evaluations, take turns: each
// sees the other's effect, and the events each records are numbered in the
// order they commit.
func (s *PGStore) Report(ctx context.Context, systemID, agentID string, r model.AgentHealthReq, decide Decide) (GroupStatus, bool, error) {
	encoded, err := json.Marshal(r)
	if err != nil {
		return GroupStatus{}, false, fmt.Errorf("encode the health report: %w", err)
	}

	var (
		gs       GroupStatus
		recorded bool
	)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		gs, err = loadGroup(ctx, tx,
			`WHERE g.group_id = (SELECT group_id FROM ha_nodes WHERE system_id = $1) FOR UPDATE OF g`, systemID)
		if err != nil {
			return err
		}
		if r.GroupID == "" {
			return nil
		}
		tag, err := tx.Exec(ctx, recordReportSQL,
			systemID, r.GroupID, r.Role, encoded, r.CollectedAt.UTC(), agentID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		recorded = true
		if err := loadNodes(ctx, tx, &gs); err != nil {
			return err
		}
		return apply(ctx, tx, &gs, decide(gs))
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return GroupStatus{}, false, ErrNotAssigned
	case err != nil:
		return GroupStatus{}, false, fmt.Errorf("record the health report: %w", err)
	}
	return gs, recorded, nil
}

// Evaluate implements Store.
func (s *PGStore) Evaluate(ctx context.Context, groupID string, decide Decide) (GroupStatus, error) {
	var gs GroupStatus
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		gs, err = loadGroup(ctx, tx, `WHERE g.group_id = $1 FOR UPDATE OF g`, groupID)
		if err != nil {
			return err
		}
		return apply(ctx, tx, &gs, decide(gs))
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return GroupStatus{}, ErrGroupNotFound
	case err != nil:
		return GroupStatus{}, fmt.Errorf("evaluate the HA group: %w", err)
	}
	return gs, nil
}

// applySQL writes an evaluation. It is conditioned on the state the decision
// was made from: the caller holds the row lock, so a mismatch means the lock
// was not held, and writing anyway could undo another mover's change.
const applySQL = `
UPDATE ha_groups
   SET state = $3, verdict = $4, reasons = $5, evaluated_at = now(),
       last_sync_confirmed_at = $6, link_down_since = $7, primary_failing_since = $8,
       event_seq = event_seq + $9, updated_at = now()
 WHERE group_id = $1 AND state = $2
RETURNING event_seq, evaluated_at`

// apply writes ch to the locked group and records its events, numbering them
// after the group's last.
func apply(ctx context.Context, tx pgx.Tx, gs *GroupStatus, ch Change) error {
	reasons, err := encodeReasons(ch.Reasons)
	if err != nil {
		return err
	}
	var (
		lastSeq     int64
		evaluatedAt time.Time
	)
	err = tx.QueryRow(ctx, applySQL,
		gs.GroupID, string(gs.State), string(ch.State), string(ch.Verdict), reasons,
		ch.LastSyncConfirmedAt, ch.LinkDownSince, ch.PrimaryFailingSince, len(ch.Events),
	).Scan(&lastSeq, &evaluatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("group %s left state %s while it was being evaluated", gs.GroupID, gs.State)
	}
	if err != nil {
		return err
	}

	seq := lastSeq - int64(len(ch.Events))
	for _, e := range ch.Events {
		seq++
		encoded, err := encodeReasons(e.Reasons)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO ha_events (group_id, seq, kind, from_value, to_value, reasons)
VALUES ($1, $2, $3, $4, $5, $6)`,
			gs.GroupID, seq, e.Kind, nullable(e.From), nullable(e.To), encoded); err != nil {
			return err
		}
	}

	evaluatedAt = evaluatedAt.UTC()
	gs.State, gs.Verdict, gs.Reasons, gs.EvaluatedAt = ch.State, ch.Verdict, ch.Reasons, &evaluatedAt
	gs.LastSyncConfirmedAt, gs.LinkDownSince, gs.PrimaryFailingSince =
		ch.LastSyncConfirmedAt, ch.LinkDownSince, ch.PrimaryFailingSince
	gs.EventSeq = lastSeq
	return nil
}

// loadGroup reads one group selected by where, and its nodes.
func loadGroup(ctx context.Context, tx pgx.Tx, where string, args ...any) (GroupStatus, error) {
	gs, err := scanGroup(tx.QueryRow(ctx, `SELECT `+groupColumns+` FROM ha_groups g `+where, args...))
	if err != nil {
		return GroupStatus{}, err
	}
	if err := loadNodes(ctx, tx, &gs); err != nil {
		return GroupStatus{}, err
	}
	return gs, nil
}

// loadNodes reads the nodes of gs.
func loadNodes(ctx context.Context, tx pgx.Tx, gs *GroupStatus) error {
	rows, err := tx.Query(ctx, `SELECT `+nodeColumns+` FROM ha_nodes WHERE group_id = $1`, gs.GroupID)
	if err != nil {
		return err
	}
	nodes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Node, error) {
		var (
			n       Node
			role    string
			agentID *string
			report  []byte
		)
		if err := row.Scan(&n.SystemID, &role, &n.Address, &n.Disk, &n.NodeID,
			&n.Fenced, &agentID, &report, &n.ReceivedAt); err != nil {
			return Node{}, err
		}
		n.Role, n.AgentID = Role(role), deref(agentID)
		if report != nil {
			n.Report = &model.AgentHealthReq{}
			if err := json.Unmarshal(report, n.Report); err != nil {
				return Node{}, fmt.Errorf("decode the report of %s: %w", n.SystemID, err)
			}
		}
		n.ReceivedAt = utc(n.ReceivedAt)
		return n, nil
	})
	if err != nil {
		return err
	}
	for _, n := range nodes {
		switch n.Role {
		case RolePrimary:
			gs.Primary = n
		case RoleStandby:
			gs.Standby = n
		}
	}
	if gs.Primary.SystemID == "" || gs.Standby.SystemID == "" {
		return fmt.Errorf("group %s does not have both a primary and a standby", gs.GroupID)
	}
	return nil
}

// scanner abstracts pgx.Row and pgx.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanGroup(sc scanner) (GroupStatus, error) {
	var (
		gs                         GroupStatus
		state, verdict             string
		reasons, resource, service []byte
		serviceAddress, createdBy  *string
	)
	if err := sc.Scan(&gs.GroupID, &gs.Name, &state, &verdict, &reasons, &gs.EvaluatedAt,
		&resource, &service, &serviceAddress,
		&gs.LastSyncConfirmedAt, &gs.LinkDownSince, &gs.PrimaryFailingSince,
		&gs.EventSeq, &createdBy, &gs.CreatedAt, &gs.Now); err != nil {
		return GroupStatus{}, err
	}
	gs.State, gs.Verdict = State(state), Verdict(verdict)
	gs.ServiceAddress, gs.CreatedBy = deref(serviceAddress), deref(createdBy)
	if err := json.Unmarshal(reasons, &gs.Reasons); err != nil {
		return GroupStatus{}, fmt.Errorf("decode the reasons of group %s: %w", gs.GroupID, err)
	}
	if err := json.Unmarshal(resource, &gs.Resource); err != nil {
		return GroupStatus{}, fmt.Errorf("decode the resource of group %s: %w", gs.GroupID, err)
	}
	if err := json.Unmarshal(service, &gs.Service); err != nil {
		return GroupStatus{}, fmt.Errorf("decode the service of group %s: %w", gs.GroupID, err)
	}
	gs.EvaluatedAt = utc(gs.EvaluatedAt)
	gs.LastSyncConfirmedAt = utc(gs.LastSyncConfirmedAt)
	gs.LinkDownSince = utc(gs.LinkDownSince)
	gs.PrimaryFailingSince = utc(gs.PrimaryFailingSince)
	gs.CreatedAt = gs.CreatedAt.UTC()
	gs.Now = gs.Now.UTC()
	return gs, nil
}

// encodeReasons encodes a reason list, never as null: the column holds a list.
func encodeReasons(rs []Reason) ([]byte, error) {
	if rs == nil {
		rs = []Reason{}
	}
	b, err := json.Marshal(rs)
	if err != nil {
		return nil, fmt.Errorf("encode the reasons: %w", err)
	}
	return b, nil
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == constraint
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// Compile-time check that PGStore still satisfies Store.
var _ Store = (*PGStore)(nil)
