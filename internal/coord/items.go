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

// Item is a small piece of work inside a task ("I-n").
type Item struct {
	ID        string `json:"id"`
	TaskID    int64  `json:"task_id"`
	Text      string `json:"text"`
	Status    string `json:"status"`
	Owner     string `json:"owner,omitempty"`
	Source    string `json:"source,omitempty"`
	ExtID     string `json:"ext_id,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

// TodoEntry is one entry of an agent's own todo or plan list.
type TodoEntry struct {
	ExtID  string
	Text   string
	Status string // pending | in_progress | done | cancelled
}

// sessionOwner returns "<provider> <role>" for an agent session.
func sessionOwner(q interface {
	QueryRow(string, ...any) *sql.Row
}, sid string) string {
	var provider, role string
	_ = q.QueryRow(`SELECT provider,role FROM sessions WHERE id=?`, sid).Scan(&provider, &role)
	return strings.TrimSpace(provider + " " + role)
}

func (c *Coordinator) itemTask(ctx context.Context, a Actor, taskID int64) (int64, error) {
	return c.ResolveTask(ctx, a, taskID)
}

// AddItem records a work item explicitly (agent operation or user).
func (c *Coordinator) AddItem(ctx context.Context, a Actor, taskID int64, text, status, owner string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("item text is required")
	}
	if status == "" {
		status = "pending"
	}
	if err := vocab.ItemStatus.Check(status); err != nil {
		return "", err
	}
	if err := secretscan.Check("item", text); err != nil {
		return "", err
	}
	taskID, err := c.itemTask(ctx, a, taskID)
	if err != nil {
		return "", err
	}
	var id int64
	err = c.tx(ctx, func(tx *sql.Tx) error {
		if err := c.checkWrite(tx, taskID, a, accessMember); err != nil {
			return err
		}
		if owner == "" && !a.IsUser() {
			owner = sessionOwner(tx, a.SessionID)
		}
		now := c.now()
		res, err := tx.Exec(`INSERT INTO items(task_id,text,status,owner,source,position,created_at,updated_at)
		 VALUES(?,?,?,?,?,(SELECT COALESCE(MAX(position),0)+1 FROM items WHERE task_id=?),?,?)`,
			taskID, text, status, owner, "agent", taskID, now, now)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return nil
	})
	if err == nil {
		c.changed(taskID)
	}
	return FormatID("I", id), err
}

// SetItem changes an item's status and, optionally, its text.
func (c *Coordinator) SetItem(ctx context.Context, a Actor, id, status, text string) error {
	n, err := ParseID("I", id)
	if err != nil {
		return err
	}
	if status != "" {
		if err := vocab.ItemStatus.Check(status); err != nil {
			return err
		}
	}
	if err := secretscan.Check("item", text); err != nil {
		return err
	}
	var taskID int64
	err = c.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRow(`SELECT task_id FROM items WHERE id=?`, n).Scan(&taskID); err != nil {
			return fmt.Errorf("item %s not found", id)
		}
		if err := c.checkWrite(tx, taskID, a, accessMember); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE items SET status=COALESCE(NULLIF(?,''),status), text=COALESCE(NULLIF(?,''),text), updated_at=? WHERE id=?`,
			status, strings.TrimSpace(text), c.now(), n)
		return err
	})
	if err == nil {
		c.changed(taskID)
	}
	return err
}

// ListItems lists a task's items in order, cancelled ones last.
func (c *Coordinator) ListItems(ctx context.Context, taskID int64) ([]Item, error) {
	rows, err := c.DB.QueryContext(ctx, `SELECT id,task_id,text,status,owner,source,ext_id,updated_at FROM items WHERE task_id=?
	 ORDER BY status='cancelled', position, id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		var id int64
		if err := rows.Scan(&id, &it.TaskID, &it.Text, &it.Status, &it.Owner, &it.Source, &it.ExtID, &it.UpdatedAt); err != nil {
			return nil, err
		}
		it.ID = FormatID("I", id)
		out = append(out, it)
	}
	return out, rows.Err()
}

// SyncTodos mirrors an agent's full todo or plan list (Claude TodoWrite,
// Codex update_plan). Entries are matched by the tool's own ID when it has
// one, otherwise by text; entries that disappeared and were not done are
// cancelled. Only the calling session's mirrored items are touched.
func (c *Coordinator) SyncTodos(ctx context.Context, a Actor, todos []TodoEntry) error {
	if a.IsUser() {
		return errors.New("only an agent session mirrors its todo list")
	}
	taskID, err := c.sessionTask(ctx, a)
	if err != nil {
		return err
	}
	source := "todo:" + a.SessionID
	err = c.tx(ctx, func(tx *sql.Tx) error {
		type row struct {
			id        int64
			text, ext string
			status    string
			seen      bool
		}
		rs, err := tx.Query(`SELECT id,text,ext_id,status FROM items WHERE task_id=? AND source=?`, taskID, source)
		if err != nil {
			return err
		}
		var existing []*row
		for rs.Next() {
			r := &row{}
			if err := rs.Scan(&r.id, &r.text, &r.ext, &r.status); err != nil {
				rs.Close()
				return err
			}
			existing = append(existing, r)
		}
		rs.Close()
		owner := sessionOwner(tx, a.SessionID)
		now := c.now()
		base := 0
		_ = tx.QueryRow(`SELECT COALESCE(MAX(position),0) FROM items WHERE task_id=? AND source!=?`, taskID, source).Scan(&base)
		for i, t := range todos {
			text := strings.TrimSpace(t.Text)
			if text == "" || vocab.ItemStatus.Check(t.Status) != nil || secretscan.Scan(text) != nil {
				continue
			}
			var match *row
			for _, r := range existing {
				if !r.seen && ((t.ExtID != "" && r.ext == t.ExtID) || (t.ExtID == "" && norm(r.text) == norm(text))) {
					match = r
					break
				}
			}
			if match != nil {
				match.seen = true
				if _, err := tx.Exec(`UPDATE items SET text=?,status=?,position=?,updated_at=? WHERE id=?`, text, t.Status, base+i+1, now, match.id); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(`INSERT INTO items(task_id,text,status,owner,source,ext_id,position,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
				taskID, text, t.Status, owner, source, t.ExtID, base+i+1, now, now); err != nil {
				return err
			}
		}
		for _, r := range existing {
			if !r.seen && r.status != "done" && r.status != "cancelled" {
				if _, err := tx.Exec(`UPDATE items SET status='cancelled',updated_at=? WHERE id=?`, now, r.id); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err == nil {
		c.changed(taskID)
	}
	return err
}

// UpsertTodo creates or updates one mirrored entry by the tool's own ID
// (for todo tools that create and update entries one at a time).
func (c *Coordinator) UpsertTodo(ctx context.Context, a Actor, t TodoEntry) error {
	if a.IsUser() || t.ExtID == "" {
		return errors.New("an agent session and the entry's ID are required")
	}
	taskID, err := c.sessionTask(ctx, a)
	if err != nil {
		return err
	}
	if t.Status != "" && vocab.ItemStatus.Check(t.Status) != nil {
		return fmt.Errorf("invalid status %q", t.Status)
	}
	if secretscan.Scan(t.Text) != nil {
		return nil
	}
	source := "todo:" + a.SessionID
	err = c.tx(ctx, func(tx *sql.Tx) error {
		now := c.now()
		res, err := tx.Exec(`UPDATE items SET status=COALESCE(NULLIF(?,''),status), text=COALESCE(NULLIF(?,''),text), updated_at=? WHERE task_id=? AND source=? AND ext_id=?`,
			t.Status, strings.TrimSpace(t.Text), now, taskID, source, t.ExtID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 || strings.TrimSpace(t.Text) == "" {
			return nil
		}
		status := t.Status
		if status == "" {
			status = "pending"
		}
		_, err = tx.Exec(`INSERT INTO items(task_id,text,status,owner,source,ext_id,position,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,(SELECT COALESCE(MAX(position),0)+1 FROM items WHERE task_id=?),?,?)`,
			taskID, strings.TrimSpace(t.Text), status, sessionOwner(tx, a.SessionID), source, t.ExtID, taskID, now, now)
		return err
	})
	if err == nil {
		c.changed(taskID)
	}
	return err
}

// syncAssignmentItem keeps the item that represents an assignment in step
// with the assignment's status.
func (c *Coordinator) syncAssignmentItem(tx *sql.Tx, taskID int64, aid, scope, assignmentStatus, owner string) error {
	status := map[string]string{
		"assigned": "pending", "running": "in_progress", "submitted": "review",
		"accepted": "done", "rejected": "blocked", "cancelled": "cancelled",
	}[assignmentStatus]
	if status == "" {
		return nil
	}
	source := "assignment:" + aid
	now := c.now()
	res, err := tx.Exec(`UPDATE items SET status=?, owner=COALESCE(NULLIF(?,''),owner), updated_at=? WHERE task_id=? AND source=?`,
		status, owner, now, taskID, source)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 || scope == "" {
		return nil
	}
	_, err = tx.Exec(`INSERT INTO items(task_id,text,status,owner,source,position,created_at,updated_at)
	 VALUES(?,?,?,?,?,(SELECT COALESCE(MAX(position),0)+1 FROM items WHERE task_id=?),?,?)`,
		taskID, scope, status, owner, source, taskID, now, now)
	return err
}
