package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dlir2404/agent-shiftboard/internal/secretscan"
)

// Task is a task row plus its lease.
type Task struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	Status        string `json:"status"`
	Priority      int    `json:"priority"`
	Worktree      string `json:"worktree,omitempty"`
	Branch        string `json:"branch,omitempty"`
	BriefRevision int64  `json:"brief_revision"`
	LeaseState    string `json:"lease_state"`
	LeadSession   string `json:"lead_session,omitempty"`
	PendingLead   string `json:"pending_lead_session,omitempty"`
	UpdatedAt     string `json:"updated_at"`
}

// CreateTask creates a queued task with an initial brief. The user or a
// managed session may create tasks; a new task has no lead until one is
// started for it.
func (c *Coordinator) CreateTask(ctx context.Context, a Actor, title, goal string) (int64, error) {
	if !a.IsUser() {
		if _, err := loadSession(c.DB, a.SessionID); err != nil {
			return 0, err
		}
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return 0, errors.New("title is required")
	}
	if goal == "" {
		goal = title
	}
	for f, v := range map[string]string{"title": title, "goal": goal} {
		if err := secretscan.Check(f, v); err != nil {
			return 0, err
		}
	}
	var id int64
	err := c.tx(ctx, func(tx *sql.Tx) error {
		now := c.now()
		res, err := tx.Exec(`INSERT INTO tasks(title,status,brief_revision,created_at,updated_at) VALUES(?,?,1,?,?)`,
			title, "queued", now, now)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		fields, _ := json.Marshal(BriefFields{Goal: goal})
		if _, err := tx.Exec(`INSERT INTO briefs(task_id,revision,fields,updated_by,updated_at) VALUES(?,1,?,?,?)`,
			id, string(fields), a.String(), now); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO leases(task_id,state,updated_at) VALUES(?,?,?)`, id, "vacant", now); err != nil {
			return err
		}
		return c.event(tx, id, a, "task_created", title)
	})
	if err == nil {
		c.changed(id)
	}
	return id, err
}

const taskSelect = `SELECT t.id,t.title,t.status,t.priority,t.worktree,t.branch,t.brief_revision,
 l.state,l.holder_session,l.pending_session,t.updated_at
 FROM tasks t JOIN leases l ON l.task_id=t.id`

func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.Title, &t.Status, &t.Priority, &t.Worktree, &t.Branch, &t.BriefRevision,
		&t.LeaseState, &t.LeadSession, &t.PendingLead, &t.UpdatedAt)
	return t, err
}

// ListTasks returns all tasks, newest first (read operation).
func (c *Coordinator) ListTasks(ctx context.Context) ([]Task, error) {
	rows, err := c.DB.QueryContext(ctx, taskSelect+` ORDER BY t.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTask returns one task (read operation).
func (c *Coordinator) GetTask(ctx context.Context, id int64) (Task, error) {
	t, err := scanTask(c.DB.QueryRowContext(ctx, taskSelect+` WHERE t.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("task #%d not found", id)
	}
	return t, err
}

func (c *Coordinator) setTaskStatus(tx *sql.Tx, id int64, status string) error {
	_, err := tx.Exec(`UPDATE tasks SET status=?,updated_at=? WHERE id=?`, status, c.now(), id)
	return err
}

func taskStatus(tx *sql.Tx, id int64) (string, error) {
	var s string
	err := tx.QueryRow(`SELECT status FROM tasks WHERE id=?`, id).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("task #%d not found", id)
	}
	return s, err
}

// BlockTask marks a task blocked (agent operation by the lead, or the user).
func (c *Coordinator) BlockTask(ctx context.Context, a Actor, id int64, blocker string) error {
	if strings.TrimSpace(blocker) == "" {
		return errors.New("describe the blocker and who can resolve it")
	}
	if err := secretscan.Check("blocker", blocker); err != nil {
		return err
	}
	return c.taskTransition(ctx, a, id, []string{"active"}, "blocked", "task_blocked", blocker)
}

// UnblockTask returns a blocked task to active.
func (c *Coordinator) UnblockTask(ctx context.Context, a Actor, id int64) error {
	return c.taskTransition(ctx, a, id, []string{"blocked"}, "active", "task_unblocked", "")
}

// CloseTask marks a task done and vacates its lease (agent operation by the lead).
func (c *Coordinator) CloseTask(ctx context.Context, a Actor, id int64) error {
	return c.taskTransition(ctx, a, id, []string{"active", "blocked"}, "done", "task_closed", "")
}

// CancelTask cancels a task (user operation).
func (c *Coordinator) CancelTask(ctx context.Context, a Actor, id int64) error {
	if err := requireUser(a); err != nil {
		return err
	}
	return c.taskTransition(ctx, a, id, []string{"queued", "active", "blocked"}, "cancelled", "task_cancelled", "")
}

// ReopenTask returns a done or cancelled task to queued (user operation).
func (c *Coordinator) ReopenTask(ctx context.Context, a Actor, id int64) error {
	if err := requireUser(a); err != nil {
		return err
	}
	return c.taskTransition(ctx, a, id, []string{"done", "cancelled"}, "queued", "task_reopened", "")
}

func (c *Coordinator) taskTransition(ctx context.Context, a Actor, id int64, from []string, to, kind, detail string) error {
	err := c.tx(ctx, func(tx *sql.Tx) error {
		if err := c.checkWrite(tx, id, a, accessLead); err != nil {
			return err
		}
		cur, err := taskStatus(tx, id)
		if err != nil {
			return err
		}
		ok := false
		for _, f := range from {
			ok = ok || f == cur
		}
		if !ok {
			return fmt.Errorf("task #%d is %s; cannot move to %s", id, cur, to)
		}
		if err := c.setTaskStatus(tx, id, to); err != nil {
			return err
		}
		if to == "done" || to == "cancelled" {
			if _, err := tx.Exec(`UPDATE leases SET state='vacant',holder_session='',pending_session='',note=?,updated_at=? WHERE task_id=?`,
				kind, c.now(), id); err != nil {
				return err
			}
		}
		return c.event(tx, id, a, kind, detail)
	})
	if err == nil {
		c.changed(id)
	}
	return err
}

// DraftTitle marks a task created before the user described it.
const DraftTitle = "New task (not described yet)"

// DescribeTask sets a task's title and, when goal is given, the brief goal
// (lead or user). The lead uses it once the user has said what they want.
func (c *Coordinator) DescribeTask(ctx context.Context, a Actor, id int64, title, goal string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("title is required")
	}
	if err := secretscan.Check("title", title+" "+goal); err != nil {
		return err
	}
	err := c.tx(ctx, func(tx *sql.Tx) error {
		if err := c.checkWrite(tx, id, a, accessLead); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE tasks SET title=?,updated_at=? WHERE id=?`, title, c.now(), id); err != nil {
			return err
		}
		return c.event(tx, id, a, "task_described", title)
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(goal) != "" {
		b, err := c.GetBrief(ctx, id)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(strings.TrimSpace(goal))
		if _, err := c.UpdateBrief(ctx, a, id, b.Revision, BriefPatch{Set: map[string]json.RawMessage{"goal": raw}}); err != nil {
			return err
		}
	}
	c.changed(id)
	return nil
}
