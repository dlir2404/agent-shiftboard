package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/dlir2404/agent-shiftboard/internal/secretscan"
	"github.com/dlir2404/agent-shiftboard/internal/store"
	"github.com/dlir2404/agent-shiftboard/internal/vocab"
)

// Rule is a durable user rule. ID is "U-n" for user scope (user database)
// and "R-n" for project and task scope (project database).
type Rule struct {
	ID           string `json:"id"`
	Scope        string `json:"scope"`
	TaskID       int64  `json:"task_id,omitempty"`
	Text         string `json:"text"`
	Source       string `json:"source"`
	Confirmation string `json:"confirmation"`
	Status       string `json:"status"`
	ValidFrom    string `json:"valid_from"`
	SupersededBy string `json:"superseded_by,omitempty"`
	CreatedBy    string `json:"created_by"`
}

// RuleInput describes a new rule.
type RuleInput struct {
	Scope  string // user | project | task
	TaskID int64  // required for task scope
	Text   string
	Source string // who/when/where, e.g. "user in chat 2026-11-11"
	// Inferred marks a rule the lead inferred rather than heard; it stays
	// proposed until the user confirms it.
	Inferred bool
}

func (c *Coordinator) ruleDB(scope string) *sql.DB {
	if scope == "user" {
		return c.UserDB
	}
	return c.DB
}

func rulePrefix(scope string) string {
	if scope == "user" {
		return "U"
	}
	return "R"
}

// AddRule records a rule. A rule the user states is active; an agent's
// inferred rule is proposed. Agents may record user-scope rules only as
// proposed, since those apply to every project.
func (c *Coordinator) AddRule(ctx context.Context, a Actor, in RuleInput) (string, error) {
	if err := vocab.RuleScope.Check(in.Scope); err != nil {
		return "", err
	}
	in.Text = strings.TrimSpace(in.Text)
	if in.Text == "" || strings.TrimSpace(in.Source) == "" {
		return "", errors.New("rule text and source are required")
	}
	if err := secretscan.Check("rule", in.Text+" "+in.Source); err != nil {
		return "", err
	}
	status, confirmation := "active", "stated"
	switch {
	case a.IsUser():
		confirmation = "confirmed"
	case in.Inferred || in.Scope == "user":
		status = "proposed"
	}
	var taskArg any
	if in.Scope == "task" {
		tid, err := c.ResolveTask(ctx, a, in.TaskID)
		if err != nil {
			return "", err
		}
		in.TaskID = tid
		taskArg = tid
	}
	if !a.IsUser() {
		tid, err := c.sessionTask(ctx, a)
		if err != nil {
			return "", err
		}
		if err := store.Tx(ctx, c.DB, func(tx *sql.Tx) error { return c.checkWrite(tx, tid, a, accessMember) }); err != nil {
			return "", err
		}
		if in.Scope == "task" && in.TaskID != tid {
			return "", fmt.Errorf("session belongs to task #%d", tid)
		}
	}
	var id int64
	err := store.Tx(ctx, c.ruleDB(in.Scope), func(tx *sql.Tx) error {
		now := c.now()
		res, err := tx.Exec(`INSERT INTO rules(scope,task_id,text,source,confirmation,status,valid_from,created_by,created_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`, in.Scope, taskArg, in.Text, in.Source, confirmation, status, now, a.String(), now)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return nil
	})
	if err != nil {
		return "", err
	}
	rid := FormatID(rulePrefix(in.Scope), id)
	c.logEvent(ctx, in.TaskID, a, "rule_added", rid+" "+status)
	c.changedAll(in)
	return rid, nil
}

func (c *Coordinator) changedAll(in RuleInput) {
	if in.Scope == "task" {
		c.changed(in.TaskID)
		return
	}
	ts, _ := c.ListTasks(context.Background())
	for _, t := range ts {
		c.changed(t.ID)
	}
}

func (c *Coordinator) logEvent(ctx context.Context, taskID int64, a Actor, kind, detail string) {
	_ = c.tx(ctx, func(tx *sql.Tx) error { return c.event(tx, taskID, a, kind, detail) })
}

// ListRules returns rules relevant to a task: user scope, project scope, and
// that task's rules. taskID 0 lists user and project rules only. When
// activeOnly is set, only active rules are returned (read operation).
func (c *Coordinator) ListRules(ctx context.Context, taskID int64, activeOnly bool) ([]Rule, error) {
	var out []Rule
	for _, src := range []struct {
		db     *sql.DB
		prefix string
		where  string
		args   []any
	}{
		{c.UserDB, "U", `scope='user'`, nil},
		{c.DB, "R", `(scope='project' OR (scope='task' AND task_id=?))`, []any{taskID}},
	} {
		q := `SELECT id,scope,COALESCE(task_id,0),text,source,confirmation,status,valid_from,superseded_by,created_by FROM rules WHERE ` + src.where
		if activeOnly {
			q += ` AND status='active'`
		}
		rows, err := src.db.QueryContext(ctx, q+` ORDER BY id`, src.args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r Rule
			var id int64
			if err := rows.Scan(&id, &r.Scope, &r.TaskID, &r.Text, &r.Source, &r.Confirmation, &r.Status, &r.ValidFrom, &r.SupersededBy, &r.CreatedBy); err != nil {
				rows.Close()
				return nil, err
			}
			r.ID = FormatID(src.prefix, id)
			out = append(out, r)
		}
		rows.Close()
	}
	return out, nil
}

func (c *Coordinator) ruleRef(id string) (*sql.DB, int64, error) {
	switch {
	case strings.HasPrefix(id, "U-"):
		n, err := ParseID("U", id)
		return c.UserDB, n, err
	case strings.HasPrefix(id, "R-"):
		n, err := ParseID("R", id)
		return c.DB, n, err
	}
	return nil, 0, fmt.Errorf("invalid rule ID %q (expected U-n or R-n)", id)
}

// ConfirmRule activates a proposed rule (user operation).
func (c *Coordinator) ConfirmRule(ctx context.Context, a Actor, id string) error {
	return c.setRuleStatus(ctx, a, id, "proposed", "active", "confirmed")
}

// RevokeRule revokes an active or proposed rule (user operation).
func (c *Coordinator) RevokeRule(ctx context.Context, a Actor, id string) error {
	return c.setRuleStatus(ctx, a, id, "", "revoked", "")
}

func (c *Coordinator) setRuleStatus(ctx context.Context, a Actor, id, from, to, confirmation string) error {
	if err := requireUser(a); err != nil {
		return err
	}
	db, n, err := c.ruleRef(id)
	if err != nil {
		return err
	}
	err = store.Tx(ctx, db, func(tx *sql.Tx) error {
		var cur string
		if err := tx.QueryRow(`SELECT status FROM rules WHERE id=?`, n).Scan(&cur); err != nil {
			return fmt.Errorf("rule %s not found", id)
		}
		if from != "" && cur != from {
			return fmt.Errorf("rule %s is %s", id, cur)
		}
		if cur == "revoked" || cur == "superseded" {
			return fmt.Errorf("rule %s is already %s", id, cur)
		}
		q := `UPDATE rules SET status=? WHERE id=?`
		args := []any{to, n}
		if confirmation != "" {
			q = `UPDATE rules SET status=?,confirmation=? WHERE id=?`
			args = []any{to, confirmation, n}
		}
		_, err := tx.Exec(q, args...)
		return err
	})
	if err == nil {
		c.logEvent(ctx, 0, a, "rule_"+to, id)
		c.changedAll(RuleInput{Scope: "project"})
	}
	return err
}

// SupersedeRule replaces an active rule with new text in the same scope
// (user operation). It returns the new rule's ID.
func (c *Coordinator) SupersedeRule(ctx context.Context, a Actor, id, text, source string) (string, error) {
	if err := requireUser(a); err != nil {
		return "", err
	}
	db, n, err := c.ruleRef(id)
	if err != nil {
		return "", err
	}
	var in RuleInput
	var taskID sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT scope,task_id FROM rules WHERE id=? AND status='active'`, n).Scan(&in.Scope, &taskID); err != nil {
		return "", fmt.Errorf("rule %s is not an active rule", id)
	}
	in.TaskID, in.Text, in.Source = taskID.Int64, text, source
	newID, err := c.AddRule(ctx, a, in)
	if err != nil {
		return "", err
	}
	err = store.Tx(ctx, db, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE rules SET status='superseded',superseded_by=? WHERE id=?`, newID, n)
		return err
	})
	return newID, err
}
