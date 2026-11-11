package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/dlir2404/agent-shiftboard/internal/secretscan"
)

// Assignment is work delegated to a worker session ("A-n").
type Assignment struct {
	ID            string `json:"id"`
	TaskID        int64  `json:"task_id"`
	WorkerSession string `json:"worker_session,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Scope         string `json:"scope"`
	Expected      string `json:"expected_output,omitempty"`
	RequiredTools string `json:"required_tools,omitempty"`
	Constraints   string `json:"constraints,omitempty"`
	Worktree      string `json:"worktree,omitempty"`
	Status        string `json:"status"`
	Result        string `json:"result,omitempty"`
	Evidence      string `json:"evidence,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// AssignmentInput describes new delegated work.
type AssignmentInput struct {
	TaskID        int64
	Scope         string
	Expected      string
	RequiredTools string
	Constraints   string
	Worktree      string
}

// CreateAssignment records an assignment before a worker starts (agent
// operation by the lead, or the user).
func (c *Coordinator) CreateAssignment(ctx context.Context, a Actor, in AssignmentInput) (string, error) {
	if strings.TrimSpace(in.Scope) == "" {
		return "", errors.New("scope is required")
	}
	if err := secretscan.Check("assignment", strings.Join([]string{in.Scope, in.Expected, in.RequiredTools, in.Constraints}, " ")); err != nil {
		return "", err
	}
	taskID, err := c.ResolveTask(ctx, a, in.TaskID)
	if err != nil {
		return "", err
	}
	var id int64
	err = c.tx(ctx, func(tx *sql.Tx) error {
		if err := c.checkWrite(tx, taskID, a, accessLead); err != nil {
			return err
		}
		now := c.now()
		res, err := tx.Exec(`INSERT INTO assignments(task_id,scope,expected,required_tools,constraints,worktree,status,created_by,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`, taskID, in.Scope, in.Expected, in.RequiredTools, in.Constraints, in.Worktree, "assigned", a.String(), now, now)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return c.event(tx, taskID, a, "assignment_created", FormatID("A", id))
	})
	if err == nil {
		c.changed(taskID)
	}
	return FormatID("A", id), err
}

func (c *Coordinator) assignmentOp(ctx context.Context, a Actor, id string, fn func(tx *sql.Tx, n, taskID int64, status, worker string) error) error {
	n, err := ParseID("A", id)
	if err != nil {
		return err
	}
	var taskID int64
	err = c.tx(ctx, func(tx *sql.Tx) error {
		var status, worker string
		if err := tx.QueryRow(`SELECT task_id,status,worker_session FROM assignments WHERE id=?`, n).Scan(&taskID, &status, &worker); err != nil {
			return fmt.Errorf("assignment %s not found", id)
		}
		return fn(tx, n, taskID, status, worker)
	})
	if err == nil {
		c.changed(taskID)
	}
	return err
}

// SubmitAssignment submits a worker's result (agent operation by that worker).
func (c *Coordinator) SubmitAssignment(ctx context.Context, a Actor, id, result, evidence string) error {
	if strings.TrimSpace(result) == "" {
		return errors.New("a concise result is required")
	}
	if err := secretscan.Check("result", result+" "+evidence); err != nil {
		return err
	}
	return c.assignmentOp(ctx, a, id, func(tx *sql.Tx, n, taskID int64, status, worker string) error {
		if err := c.checkWrite(tx, taskID, a, accessMember); err != nil {
			return err
		}
		if a.SessionID != worker {
			return fmt.Errorf("only the worker session of %s can submit it", id)
		}
		if status != "assigned" && status != "running" {
			return fmt.Errorf("%s is %s", id, status)
		}
		if _, err := tx.Exec(`UPDATE assignments SET status='submitted',result=?,evidence=?,updated_at=? WHERE id=?`, result, evidence, c.now(), n); err != nil {
			return err
		}
		return c.event(tx, taskID, a, "assignment_submitted", id)
	})
}

// ReviewAssignment accepts or rejects a submitted result (agent operation by
// the lead, or the user).
func (c *Coordinator) ReviewAssignment(ctx context.Context, a Actor, id string, accept bool, reason string) error {
	if !accept && strings.TrimSpace(reason) == "" {
		return errors.New("give a reason when rejecting")
	}
	return c.assignmentOp(ctx, a, id, func(tx *sql.Tx, n, taskID int64, status, _ string) error {
		if err := c.checkWrite(tx, taskID, a, accessLead); err != nil {
			return err
		}
		if status != "submitted" {
			return fmt.Errorf("%s is %s, not submitted", id, status)
		}
		to := "accepted"
		if !accept {
			to = "rejected"
		}
		if _, err := tx.Exec(`UPDATE assignments SET status=?,reason=?,updated_at=? WHERE id=?`, to, reason, c.now(), n); err != nil {
			return err
		}
		return c.event(tx, taskID, a, "assignment_"+to, id)
	})
}

// CancelAssignment cancels an assignment that is not yet accepted (the lead or the user).
func (c *Coordinator) CancelAssignment(ctx context.Context, a Actor, id, reason string) error {
	return c.assignmentOp(ctx, a, id, func(tx *sql.Tx, n, taskID int64, status, _ string) error {
		if err := c.checkWrite(tx, taskID, a, accessLead); err != nil {
			return err
		}
		switch status {
		case "accepted", "rejected", "cancelled":
			return fmt.Errorf("%s is already %s", id, status)
		}
		if _, err := tx.Exec(`UPDATE assignments SET status='cancelled',reason=?,updated_at=? WHERE id=?`, reason, c.now(), n); err != nil {
			return err
		}
		return c.event(tx, taskID, a, "assignment_cancelled", id)
	})
}

// ListAssignments lists a task's assignments (read operation).
func (c *Coordinator) ListAssignments(ctx context.Context, taskID int64) ([]Assignment, error) {
	rows, err := c.DB.QueryContext(ctx, `SELECT id,task_id,worker_session,provider,scope,expected,required_tools,constraints,worktree,status,result,evidence,reason
	 FROM assignments WHERE task_id=? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Assignment
	for rows.Next() {
		var x Assignment
		var id int64
		if err := rows.Scan(&id, &x.TaskID, &x.WorkerSession, &x.Provider, &x.Scope, &x.Expected, &x.RequiredTools, &x.Constraints, &x.Worktree, &x.Status, &x.Result, &x.Evidence, &x.Reason); err != nil {
			return nil, err
		}
		x.ID = FormatID("A", id)
		out = append(out, x)
	}
	return out, rows.Err()
}
