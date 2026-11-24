package ha

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/jackc/pgx/v5"
)

const oneRunningFlow = "ha_flows_one_running"

// flowColumns is the column list every flow read shares.
const flowColumns = `flow_id, group_id, kind, status, step, message, requested_by,
                     started_at, updated_at, finished_at`

// lockGroupWhere selects one group by ID or name and locks it. Everything a
// flow does to a group happens under this lock, as an evaluation does, so
// that a flow, the evaluator and an acknowledgement take turns.
const lockGroupWhere = `WHERE g.group_id = $1 OR g.name = $1
 ORDER BY (g.group_id = $1) DESC LIMIT 1 FOR UPDATE OF g`

// StartFlow implements FlowStore. plan decides from the locked group what to
// start; its error is returned as is and nothing is written.
func (s *PGStore) StartFlow(ctx context.Context, idOrName string, plan func(StartContext) (FlowStart, error), ttl time.Duration) (GroupStatus, Flow, error) {
	var (
		gs   GroupStatus
		flow Flow
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if gs, err = loadGroup(ctx, tx, lockGroupWhere, idOrName); err != nil {
			return err
		}
		c := StartContext{Group: gs}
		if c.Running, err = runningFlow(ctx, tx, gs.GroupID); err != nil {
			return err
		}
		if c.Active, err = job.ActiveTx(ctx, tx, []string{gs.Primary.SystemID, gs.Standby.SystemID}); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT replication_secret FROM ha_groups WHERE group_id = $1`,
			gs.GroupID).Scan(&c.SealedSecret); err != nil {
			return err
		}

		start, err := plan(c)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
INSERT INTO ha_flows (flow_id, group_id, kind, status, step, requested_by)
VALUES ($1, $2, $3, 'running', 1, $4)`,
			start.FlowID, gs.GroupID, string(start.Kind), nullable(start.RequestedBy)); err != nil {
			if isUniqueViolation(err, oneRunningFlow) {
				return ErrFlowInProgress
			}
			return err
		}
		if start.SealedSecret != nil {
			if _, err := tx.Exec(ctx, `UPDATE ha_groups SET replication_secret = $2 WHERE group_id = $1`,
				gs.GroupID, start.SealedSecret); err != nil {
				return err
			}
		}
		if err := issueStep(ctx, tx, start.FlowID, 1, start.Jobs, start.RequestedBy, ttl); err != nil {
			return err
		}
		if err := transition(ctx, tx, &gs, start.To, nil,
			NewEvent{Kind: EventFlowStarted, To: string(start.Kind), Reasons: []Reason{{
				Code: "flow_started", Severity: VerdictOK,
				Message: fmt.Sprintf("%s %s requested by %s", start.Kind, start.FlowID, orUnknown(start.RequestedBy)),
			}}},
		); err != nil {
			return err
		}
		flow, err = loadFlow(ctx, tx, start.FlowID)
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return GroupStatus{}, Flow{}, ErrGroupNotFound
	case err != nil:
		return GroupStatus{}, Flow{}, err
	}
	return gs, flow, nil
}

// RunningFlowIDs implements FlowStore.
func (s *PGStore) RunningFlowIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT flow_id FROM ha_flows WHERE status = 'running' ORDER BY started_at, flow_id`)
	if err != nil {
		return nil, fmt.Errorf("list running HA operations: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list running HA operations: %w", err)
	}
	return ids, nil
}

// AdvanceFlow implements FlowStore. A flow that is no longer running, as when
// another server's tick got there first, is left alone.
func (s *PGStore) AdvanceFlow(ctx context.Context, flowID string, decide func(AdvanceContext) (FlowMove, error), ttl time.Duration) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var groupID string
		if err := tx.QueryRow(ctx, `SELECT group_id FROM ha_flows WHERE flow_id = $1`, flowID).Scan(&groupID); err != nil {
			return err
		}
		gs, err := loadGroup(ctx, tx, `WHERE g.group_id = $1 FOR UPDATE OF g`, groupID)
		if err != nil {
			return err
		}
		flow, err := loadFlow(ctx, tx, flowID)
		if err != nil {
			return err
		}
		if flow.Status != FlowRunning {
			return nil
		}

		c := AdvanceContext{Group: gs, Flow: flow}
		for _, fj := range flow.Jobs {
			if fj.Step != flow.Step {
				continue
			}
			j, a, err := job.GetTx(ctx, tx, fj.JobID)
			if err != nil {
				return err
			}
			c.Step = append(c.Step, StepJob{FlowJob: fj, Job: j, Attempt: a})
		}

		mv, err := decide(c)
		if err != nil {
			return err
		}
		switch mv.Kind {
		case MoveWait:
			return nil
		case MoveNext:
			if err := issueStep(ctx, tx, flowID, flow.Step+1, mv.Jobs, flow.RequestedBy, ttl); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `
UPDATE ha_flows SET step = step + 1, message = $2, updated_at = now() WHERE flow_id = $1`,
				flowID, nullable(mv.Message))
			return err
		case MoveComplete:
			return endFlow(ctx, tx, &gs, flow, FlowCompleted, mv.Message, mv.To)
		case MoveFail:
			return endFlow(ctx, tx, &gs, flow, FlowFailed, mv.Message, StateError)
		default:
			return fmt.Errorf("unknown move %d for HA operation %s", mv.Kind, flowID)
		}
	})
	if err != nil {
		return fmt.Errorf("advance HA operation %s: %w", flowID, err)
	}
	return nil
}

// endFlow ends a flow and moves its group to to. Jobs of the flow that have
// not started are ended with it, in the same transaction: left queued, an
// agent would claim them and be refused, over and over.
func endFlow(ctx context.Context, tx pgx.Tx, gs *GroupStatus, flow Flow, status FlowStatus, message string, to State) error {
	if _, err := tx.Exec(ctx, `
UPDATE ha_flows SET status = $2, message = $3, updated_at = now(), finished_at = now() WHERE flow_id = $1`,
		flow.FlowID, string(status), nullable(message)); err != nil {
		return err
	}
	for _, fj := range flow.Jobs {
		if err := job.RejectTx(ctx, tx, fj.JobID,
			fmt.Sprintf("Not run: the HA operation it belonged to %s", status)); err != nil {
			return err
		}
	}
	severity := VerdictOK
	if status == FlowFailed {
		severity = VerdictError
	}
	return transition(ctx, tx, gs, to, nil, NewEvent{
		Kind: EventFlowEnded, From: string(flow.Kind), To: string(status),
		Reasons: []Reason{{Code: "flow_" + string(status), Severity: severity, Message: message}},
	})
}

// FlowOfJob implements FlowStore. It returns errNoFlow for a job that is no
// flow's.
func (s *PGStore) FlowOfJob(ctx context.Context, jobID string) (FlowRef, error) {
	var (
		ref          FlowRef
		kind, status string
	)
	err := s.pool.QueryRow(ctx, `
SELECT f.flow_id, f.group_id, f.kind, f.status, f.step, fj.step, g.replication_secret
  FROM ha_flow_jobs fj
  JOIN ha_flows f ON f.flow_id = fj.flow_id
  JOIN ha_groups g ON g.group_id = f.group_id
 WHERE fj.job_id = $1`, jobID).Scan(&ref.FlowID, &ref.GroupID, &kind, &status, &ref.Step, &ref.JobStep, &ref.SealedSecret)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return FlowRef{}, errNoFlow
	case err != nil:
		return FlowRef{}, fmt.Errorf("find the HA operation of job %s: %w", jobID, err)
	}
	ref.Kind, ref.Status = FlowKind(kind), FlowStatus(status)
	return ref, nil
}

// SystemInFlow implements FlowStore.
func (s *PGStore) SystemInFlow(ctx context.Context, systemID string) (bool, error) {
	var in bool
	err := s.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM ha_flows f JOIN ha_nodes n ON n.group_id = f.group_id
                WHERE n.system_id = $1 AND f.status = 'running')`, systemID).Scan(&in)
	if err != nil {
		return false, fmt.Errorf("check for an HA operation on system %s: %w", systemID, err)
	}
	return in, nil
}

// Acknowledge implements FlowStore. decide's error is returned as is and
// nothing is written.
func (s *PGStore) Acknowledge(ctx context.Context, idOrName string, decide func(AckContext) (State, error), by string) (GroupStatus, error) {
	var gs GroupStatus
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if gs, err = loadGroup(ctx, tx, lockGroupWhere, idOrName); err != nil {
			return err
		}
		c := AckContext{Group: gs}
		if c.Latest, err = latestFlow(ctx, tx, gs.GroupID); err != nil {
			return err
		}
		if c.Active, err = job.ActiveTx(ctx, tx, []string{gs.Primary.SystemID, gs.Standby.SystemID}); err != nil {
			return err
		}
		to, err := decide(c)
		if err != nil {
			return err
		}
		return transition(ctx, tx, &gs, to, []Reason{{
			Code: "acknowledged", Severity: VerdictOK, Message: "Error acknowledged by " + orUnknown(by),
		}})
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return GroupStatus{}, ErrGroupNotFound
	case err != nil:
		return GroupStatus{}, err
	}
	return gs, nil
}

// LatestFlow implements FlowStore.
func (s *PGStore) LatestFlow(ctx context.Context, groupID string) (*Flow, error) {
	var out *Flow
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		out, err = latestFlow(ctx, tx, groupID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read the latest HA operation of group %s: %w", groupID, err)
	}
	return out, nil
}

// issueStep creates a step's jobs and records them as the step's.
func issueStep(ctx context.Context, tx pgx.Tx, flowID string, step int, jobs []PlannedJob, requestedBy string, ttl time.Duration) error {
	for _, pj := range jobs {
		if _, err := job.InsertTx(ctx, tx, job.NewJob{
			JobID: pj.JobID, SystemID: pj.SystemID, Kind: pj.Kind, Spec: pj.Spec, RequestedBy: requestedBy,
		}, ttl); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO ha_flow_jobs (flow_id, step, system_id, job_id) VALUES ($1, $2, $3, $4)`,
			flowID, step, pj.SystemID, pj.JobID); err != nil {
			return err
		}
	}
	return nil
}

// transition moves the locked group to state to and records the events,
// numbered after the group's last, followed by the state change, with
// reasons, when the state does change. Like apply, it is conditioned on the
// state the caller read under the lock.
func transition(ctx context.Context, tx pgx.Tx, gs *GroupStatus, to State, reasons []Reason, events ...NewEvent) error {
	if to != gs.State {
		events = append(events, NewEvent{Kind: EventStateChanged, From: string(gs.State), To: string(to), Reasons: reasons})
	}
	var lastSeq int64
	err := tx.QueryRow(ctx, `
UPDATE ha_groups SET state = $3, event_seq = event_seq + $4, updated_at = now()
 WHERE group_id = $1 AND state = $2
RETURNING event_seq`, gs.GroupID, string(gs.State), string(to), len(events)).Scan(&lastSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("group %s left state %s while it was locked", gs.GroupID, gs.State)
	}
	if err != nil {
		return err
	}
	seq := lastSeq - int64(len(events))
	for _, e := range events {
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
	gs.State, gs.EventSeq = to, lastSeq
	return nil
}

// runningFlow returns the group's running flow, nil if none.
func runningFlow(ctx context.Context, tx pgx.Tx, groupID string) (*Flow, error) {
	return oneFlow(ctx, tx, `WHERE group_id = $1 AND status = 'running'`, groupID)
}

// latestFlow returns the group's most recent flow, nil if none.
func latestFlow(ctx context.Context, tx pgx.Tx, groupID string) (*Flow, error) {
	return oneFlow(ctx, tx, `WHERE group_id = $1 ORDER BY started_at DESC, flow_id DESC LIMIT 1`, groupID)
}

func oneFlow(ctx context.Context, tx pgx.Tx, where string, args ...any) (*Flow, error) {
	var flowID string
	err := tx.QueryRow(ctx, `SELECT flow_id FROM ha_flows `+where, args...).Scan(&flowID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f, err := loadFlow(ctx, tx, flowID)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// loadFlow reads a flow and the jobs of its steps.
func loadFlow(ctx context.Context, tx pgx.Tx, flowID string) (Flow, error) {
	var (
		f                    Flow
		kind, status         string
		message, requestedBy *string
	)
	err := tx.QueryRow(ctx, `SELECT `+flowColumns+` FROM ha_flows WHERE flow_id = $1`, flowID).Scan(
		&f.FlowID, &f.GroupID, &kind, &status, &f.Step, &message, &requestedBy,
		&f.StartedAt, &f.UpdatedAt, &f.FinishedAt)
	if err != nil {
		return Flow{}, err
	}
	f.Kind, f.Status = FlowKind(kind), FlowStatus(status)
	f.Message, f.RequestedBy = deref(message), deref(requestedBy)
	f.StartedAt, f.UpdatedAt, f.FinishedAt = f.StartedAt.UTC(), f.UpdatedAt.UTC(), utc(f.FinishedAt)

	rows, err := tx.Query(ctx, `
SELECT fj.step, fj.system_id, fj.job_id, j.status, COALESCE(j.message, '')
  FROM ha_flow_jobs fj JOIN jobs j ON j.job_id = fj.job_id
 WHERE fj.flow_id = $1
 ORDER BY fj.step, fj.system_id`, flowID)
	if err != nil {
		return Flow{}, err
	}
	f.Jobs, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (FlowJob, error) {
		var (
			fj     FlowJob
			status string
		)
		if err := row.Scan(&fj.Step, &fj.SystemID, &fj.JobID, &status, &fj.Message); err != nil {
			return FlowJob{}, err
		}
		fj.Status = job.Status(status)
		return fj, nil
	})
	if err != nil {
		return Flow{}, err
	}
	return f, nil
}

// Compile-time check that PGStore keeps the flows too.
var _ FlowStore = (*PGStore)(nil)
