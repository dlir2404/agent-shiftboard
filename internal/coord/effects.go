package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/dlir2404/agent-shiftboard/internal/secretscan"
	"github.com/dlir2404/agent-shiftboard/internal/vocab"
)

// Effect is a side-effect intent and receipt ("E-n").
type Effect struct {
	ID           string `json:"id"`
	TaskID       int64  `json:"task_id"`
	SessionID    string `json:"session"`
	Action       string `json:"action"`
	Target       string `json:"target"`
	Status       string `json:"status"`
	ApprovalID   string `json:"approval_id,omitempty"`
	Evidence     string `json:"evidence,omitempty"`
	RollbackRef  string `json:"rollback_ref,omitempty"`
	CreatedAt    string `json:"created_at"`
	StartedAt    string `json:"started_at,omitempty"`
	FinishedAt   string `json:"finished_at,omitempty"`
	ReconciledAt string `json:"reconciled_at,omitempty"`
}

// PlanEffect records the intent for an external or non-idempotent action.
// approvalID ("P-n") links a required approval; Start refuses until it is approved.
func (c *Coordinator) PlanEffect(ctx context.Context, a Actor, taskID int64, action, target, approvalID, rollbackRef string) (string, error) {
	action, target = strings.TrimSpace(action), strings.TrimSpace(target)
	if action == "" || target == "" {
		return "", errors.New("action and target are required")
	}
	if err := secretscan.Check("side effect", action+" "+target+" "+rollbackRef); err != nil {
		return "", err
	}
	taskID, err := c.ResolveTask(ctx, a, taskID)
	if err != nil {
		return "", err
	}
	var appr any
	if approvalID != "" {
		n, err := ParseID("P", approvalID)
		if err != nil {
			return "", err
		}
		appr = n
	}
	var id int64
	err = c.tx(ctx, func(tx *sql.Tx) error {
		if err := c.checkWrite(tx, taskID, a, accessMember); err != nil {
			return err
		}
		if appr != nil {
			p, err := c.loadPending(tx, appr.(int64))
			if err != nil {
				return err
			}
			if p.TaskID != taskID || p.Kind != "approval" {
				return fmt.Errorf("%s is not an approval of task #%d", approvalID, taskID)
			}
		}
		res, err := tx.Exec(`INSERT INTO side_effects(task_id,session_id,action,target,status,approval_id,rollback_ref,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			taskID, a.String(), action, target, "planned", appr, rollbackRef, c.now())
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return c.event(tx, taskID, a, "effect_planned", FormatID("E", id))
	})
	if err == nil {
		c.changed(taskID)
	}
	return FormatID("E", id), err
}

type effectRow struct {
	TaskID     int64
	SessionID  string
	Status     string
	ApprovalID sql.NullInt64
}

func loadEffect(tx *sql.Tx, n int64) (effectRow, error) {
	var e effectRow
	err := tx.QueryRow(`SELECT task_id,session_id,status,approval_id FROM side_effects WHERE id=?`, n).
		Scan(&e.TaskID, &e.SessionID, &e.Status, &e.ApprovalID)
	if errors.Is(err, sql.ErrNoRows) {
		return e, fmt.Errorf("side effect E-%d not found", n)
	}
	return e, err
}

func (c *Coordinator) effectOp(ctx context.Context, a Actor, id string, access writeAccess, fn func(tx *sql.Tx, n int64, e effectRow) error) error {
	n, err := ParseID("E", id)
	if err != nil {
		return err
	}
	var taskID int64
	err = c.tx(ctx, func(tx *sql.Tx) error {
		e, err := loadEffect(tx, n)
		if err != nil {
			return err
		}
		taskID = e.TaskID
		if err := c.checkWrite(tx, e.TaskID, a, access); err != nil {
			return err
		}
		return fn(tx, n, e)
	})
	if err == nil {
		c.changed(taskID)
	}
	return err
}

// StartEffect marks a planned effect in_flight; call it immediately before
// the action runs.
func (c *Coordinator) StartEffect(ctx context.Context, a Actor, id string) error {
	return c.effectOp(ctx, a, id, accessMember, func(tx *sql.Tx, n int64, e effectRow) error {
		if e.SessionID != a.String() {
			return fmt.Errorf("%s was planned by %s", id, e.SessionID)
		}
		if e.Status != "planned" {
			return fmt.Errorf("%s is %s, not planned", id, e.Status)
		}
		if e.ApprovalID.Valid {
			p, err := c.loadPending(tx, e.ApprovalID.Int64)
			if err != nil {
				return err
			}
			if p.Status != "approved" {
				return fmt.Errorf("%s needs approval %s, which is %s", id, FormatID("P", e.ApprovalID.Int64), p.Status)
			}
		}
		if _, err := tx.Exec(`UPDATE side_effects SET status='in_flight',started_at=? WHERE id=?`, c.now(), n); err != nil {
			return err
		}
		return c.event(tx, e.TaskID, a, "effect_started", id)
	})
}

// FinishEffect records the observed outcome. From in_flight: confirmed,
// failed, or not_performed. From planned: not_performed only.
func (c *Coordinator) FinishEffect(ctx context.Context, a Actor, id, status, evidence string) error {
	if err := vocab.EffectStatus.Check(status); err != nil {
		return err
	}
	if err := secretscan.Check("evidence", evidence); err != nil {
		return err
	}
	return c.effectOp(ctx, a, id, accessMember, func(tx *sql.Tx, n int64, e effectRow) error {
		if e.SessionID != a.String() {
			return fmt.Errorf("%s was planned by %s", id, e.SessionID)
		}
		switch {
		case e.Status == "in_flight" && (status == "confirmed" || status == "failed" || status == "not_performed"):
		case e.Status == "planned" && status == "not_performed":
		case e.Status == "unknown":
			return fmt.Errorf("%s is unknown; resolve it with `effect reconcile` and evidence from a read-only check", id)
		default:
			return fmt.Errorf("cannot finish %s from %s as %s", id, e.Status, status)
		}
		if _, err := tx.Exec(`UPDATE side_effects SET status=?,evidence=?,finished_at=? WHERE id=?`, status, evidence, c.now(), n); err != nil {
			return err
		}
		return c.event(tx, e.TaskID, a, "effect_"+status, id)
	})
}

// ReconcileEffect resolves an unknown effect from a read-only check. The
// lease holder or the user may reconcile.
func (c *Coordinator) ReconcileEffect(ctx context.Context, a Actor, id, status, evidence string) error {
	if status != "confirmed" && status != "failed" && status != "not_performed" {
		return errors.New("reconcile to confirmed, failed, or not_performed")
	}
	if strings.TrimSpace(evidence) == "" {
		return errors.New("evidence from the read-only check is required")
	}
	if err := secretscan.Check("evidence", evidence); err != nil {
		return err
	}
	return c.effectOp(ctx, a, id, accessLead, func(tx *sql.Tx, n int64, e effectRow) error {
		if e.Status != "unknown" {
			return fmt.Errorf("%s is %s, not unknown", id, e.Status)
		}
		now := c.now()
		if _, err := tx.Exec(`UPDATE side_effects SET status=?,evidence=?,reconciled_at=?,finished_at=? WHERE id=?`, status, evidence, now, now, n); err != nil {
			return err
		}
		return c.event(tx, e.TaskID, a, "effect_reconciled", id+" "+status)
	})
}

// ListEffects lists a task's side effects (read operation). status filters
// when non-empty.
func (c *Coordinator) ListEffects(ctx context.Context, taskID int64, status string) ([]Effect, error) {
	q := `SELECT id,task_id,session_id,action,target,status,approval_id,evidence,rollback_ref,created_at,started_at,finished_at,reconciled_at FROM side_effects WHERE task_id=?`
	args := []any{taskID}
	if status != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	rows, err := c.DB.QueryContext(ctx, q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Effect
	for rows.Next() {
		var e Effect
		var id int64
		var appr sql.NullInt64
		if err := rows.Scan(&id, &e.TaskID, &e.SessionID, &e.Action, &e.Target, &e.Status, &appr, &e.Evidence, &e.RollbackRef, &e.CreatedAt, &e.StartedAt, &e.FinishedAt, &e.ReconciledAt); err != nil {
			return nil, err
		}
		e.ID = FormatID("E", id)
		if appr.Valid {
			e.ApprovalID = FormatID("P", appr.Int64)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// markEffectsUnknown applies the session-loss rule: in_flight effects become
// unknown when a turn ends; planned and in_flight effects become unknown
// when the session stops, fails, or stays unknown past the grace period.
func (c *Coordinator) markEffectsUnknown(tx *sql.Tx, sessionID string, includePlanned bool) error {
	q := `UPDATE side_effects SET status='unknown' WHERE session_id=? AND status='in_flight'`
	if includePlanned {
		q = `UPDATE side_effects SET status='unknown' WHERE session_id=? AND status IN ('planned','in_flight')`
	}
	_, err := tx.Exec(q, "session:"+sessionID)
	return err
}

// Milestone is a delivery milestone record ("M-n").
type Milestone struct {
	ID         string `json:"id"`
	TaskID     int64  `json:"task_id"`
	Kind       string `json:"kind"`
	Target     string `json:"target"`
	Status     string `json:"status"`
	Evidence   string `json:"evidence,omitempty"`
	ObservedAt string `json:"observed_at"`
}

// RecordMilestone records or updates a delivery milestone for (kind, target).
func (c *Coordinator) RecordMilestone(ctx context.Context, a Actor, taskID int64, kind, target, status, evidence string) (string, error) {
	if err := vocab.MilestoneStatus.Check(status); err != nil {
		return "", err
	}
	kind, target = strings.TrimSpace(kind), strings.TrimSpace(target)
	if kind == "" || target == "" {
		return "", errors.New("kind (commit, push, merge_request, build, deploy, verify, ...) and target are required")
	}
	if err := secretscan.Check("milestone", kind+" "+target+" "+evidence); err != nil {
		return "", err
	}
	taskID, err := c.ResolveTask(ctx, a, taskID)
	if err != nil {
		return "", err
	}
	var id int64
	err = c.tx(ctx, func(tx *sql.Tx) error {
		if err := c.checkWrite(tx, taskID, a, accessMember); err != nil {
			return err
		}
		err := tx.QueryRow(`SELECT id FROM milestones WHERE task_id=? AND kind=? AND target=?`, taskID, kind, target).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			res, err := tx.Exec(`INSERT INTO milestones(task_id,kind,target,status,evidence,observed_at) VALUES(?,?,?,?,?,?)`,
				taskID, kind, target, status, evidence, c.now())
			if err != nil {
				return err
			}
			id, _ = res.LastInsertId()
		case err != nil:
			return err
		default:
			if _, err := tx.Exec(`UPDATE milestones SET status=?,evidence=?,observed_at=? WHERE id=?`, status, evidence, c.now(), id); err != nil {
				return err
			}
		}
		return c.event(tx, taskID, a, "milestone", fmt.Sprintf("%s %s %s", kind, target, status))
	})
	if err == nil {
		c.changed(taskID)
	}
	return FormatID("M", id), err
}

// ListMilestones lists a task's milestones (read operation).
func (c *Coordinator) ListMilestones(ctx context.Context, taskID int64) ([]Milestone, error) {
	rows, err := c.DB.QueryContext(ctx, `SELECT id,task_id,kind,target,status,evidence,observed_at FROM milestones WHERE task_id=? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Milestone
	for rows.Next() {
		var m Milestone
		var id int64
		if err := rows.Scan(&id, &m.TaskID, &m.Kind, &m.Target, &m.Status, &m.Evidence, &m.ObservedAt); err != nil {
			return nil, err
		}
		m.ID = FormatID("M", id)
		out = append(out, m)
	}
	return out, rows.Err()
}
