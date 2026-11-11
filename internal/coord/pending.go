package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dlir2404/agent-shiftboard/internal/secretscan"
	"github.com/dlir2404/agent-shiftboard/internal/vocab"
)

// Pending is an open question or approval for the user ("P-n").
type Pending struct {
	ID          string `json:"id"`
	TaskID      int64  `json:"task_id"`
	Kind        string `json:"kind"`
	Action      string `json:"action"`
	Target      string `json:"target,omitempty"`
	Requester   string `json:"requester"`
	Status      string `json:"status"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	ResolvedVia string `json:"resolved_via,omitempty"`
	Quote       string `json:"quote,omitempty"`
	Answer      string `json:"answer,omitempty"`
	CreatedAt   string `json:"created_at"`
	ResolvedAt  string `json:"resolved_at,omitempty"`
}

// AddPending opens a question or an approval request. For an approval,
// action and target must be exact (e.g. "git push", "origin fix/login").
func (c *Coordinator) AddPending(ctx context.Context, a Actor, taskID int64, kind, action, target string, expiresIn time.Duration) (string, error) {
	if err := vocab.PendingKind.Check(kind); err != nil {
		return "", err
	}
	action, target = strings.TrimSpace(action), strings.TrimSpace(target)
	if action == "" {
		return "", errors.New("action (or question text) is required")
	}
	if kind == "approval" && target == "" {
		return "", errors.New("an approval needs an exact target")
	}
	if err := secretscan.Check("pending item", action+" "+target); err != nil {
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
		exp := ""
		if expiresIn > 0 {
			exp = c.Now().Add(expiresIn).UTC().Format(time.RFC3339)
		}
		res, err := tx.Exec(`INSERT INTO pending(task_id,kind,action,target,requester,status,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			taskID, kind, action, target, a.String(), "pending", exp, c.now())
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return c.event(tx, taskID, a, "pending_added", FormatID("P", id))
	})
	if err == nil {
		c.changed(taskID)
	}
	return FormatID("P", id), err
}

// ListPending lists a task's pending items; openOnly limits to status pending.
func (c *Coordinator) ListPending(ctx context.Context, taskID int64, openOnly bool) ([]Pending, error) {
	q := `SELECT id,task_id,kind,action,target,requester,status,expires_at,resolved_via,quote,answer,created_at,resolved_at FROM pending WHERE task_id=?`
	if openOnly {
		q += ` AND status='pending'`
	}
	rows, err := c.DB.QueryContext(ctx, q+` ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pending
	for rows.Next() {
		var p Pending
		var id int64
		if err := rows.Scan(&id, &p.TaskID, &p.Kind, &p.Action, &p.Target, &p.Requester, &p.Status, &p.ExpiresAt, &p.ResolvedVia, &p.Quote, &p.Answer, &p.CreatedAt, &p.ResolvedAt); err != nil {
			return nil, err
		}
		p.ID = FormatID("P", id)
		out = append(out, p)
	}
	return out, rows.Err()
}

type pendingRow struct {
	TaskID               int64
	Kind, Action, Target string
	Status, ExpiresAt    string
}

func (c *Coordinator) loadPending(tx *sql.Tx, n int64) (pendingRow, error) {
	var p pendingRow
	err := tx.QueryRow(`SELECT task_id,kind,action,target,status,expires_at FROM pending WHERE id=?`, n).
		Scan(&p.TaskID, &p.Kind, &p.Action, &p.Target, &p.Status, &p.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, fmt.Errorf("pending item P-%d not found", n)
	}
	if err == nil && p.Status == "pending" && p.ExpiresAt != "" && c.Now().After(parseTime(p.ExpiresAt)) {
		if _, err := tx.Exec(`UPDATE pending SET status='expired',resolved_at=? WHERE id=?`, c.now(), n); err != nil {
			return p, err
		}
		p.Status = "expired"
	}
	return p, err
}

func (c *Coordinator) resolvePending(ctx context.Context, a Actor, id string, access writeAccess,
	check func(p pendingRow) error, status, via, quote, answer string) error {
	n, err := ParseID("P", id)
	if err != nil {
		return err
	}
	var taskID int64
	err = c.tx(ctx, func(tx *sql.Tx) error {
		p, err := c.loadPending(tx, n)
		if err != nil {
			return err
		}
		taskID = p.TaskID
		if err := c.checkWrite(tx, p.TaskID, a, access); err != nil {
			return err
		}
		if p.Status != "pending" {
			return fmt.Errorf("%s is %s", id, p.Status)
		}
		if err := check(p); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE pending SET status=?,resolved_via=?,quote=?,answer=?,resolved_at=? WHERE id=?`,
			status, via, quote, answer, c.now(), n); err != nil {
			return err
		}
		return c.event(tx, p.TaskID, a, "pending_"+status, id+" via "+via)
	})
	if err == nil {
		c.changed(taskID)
	}
	return err
}

// RecordAnswer records the user's answer to a question, given in the
// conversation (agent operation).
func (c *Coordinator) RecordAnswer(ctx context.Context, a Actor, id, answer string) error {
	if strings.TrimSpace(answer) == "" {
		return errors.New("answer is required")
	}
	if err := secretscan.Check("answer", answer); err != nil {
		return err
	}
	return c.resolvePending(ctx, a, id, accessMember, func(p pendingRow) error {
		if p.Kind != "question" {
			return errors.New("this is an approval; use record-approval")
		}
		return nil
	}, "answered", "conversation", "", answer)
}

// RecordApproval records an approval the user gave in the conversation
// (agent operation). It must repeat the exact action and target and quote
// the user. Refused when policy is board_only.
func (c *Coordinator) RecordApproval(ctx context.Context, a Actor, id, action, target, quote string) error {
	if c.Policy.ConversationApprovals == "board_only" {
		return errors.New("this project requires approvals on the board (conversation_approvals = \"board_only\"); ask the user to approve there")
	}
	if a.IsUser() {
		return errors.New("use `agent-shiftboard pending approve` to approve as the user")
	}
	if strings.TrimSpace(quote) == "" {
		return errors.New("quote the user's words that gave the approval")
	}
	if err := secretscan.Check("quote", quote); err != nil {
		return err
	}
	return c.resolvePending(ctx, a, id, accessLead, func(p pendingRow) error {
		if p.Kind != "approval" {
			return errors.New("this is a question; use record-answer")
		}
		if strings.TrimSpace(action) != p.Action || strings.TrimSpace(target) != p.Target {
			return fmt.Errorf("action/target do not match %s exactly (recorded: %q on %q)", id, p.Action, p.Target)
		}
		return nil
	}, "approved", "conversation", strings.TrimSpace(quote), "")
}

// WithdrawPending withdraws an item the agent no longer needs (agent operation).
func (c *Coordinator) WithdrawPending(ctx context.Context, a Actor, id string) error {
	return c.resolvePending(ctx, a, id, accessMember, func(pendingRow) error { return nil }, "withdrawn", "agent", "", "")
}

// ApprovePending approves (or, with deny, denies) an approval request from the
// board or the user's terminal (user operation).
func (c *Coordinator) ApprovePending(ctx context.Context, a Actor, id string, deny bool) error {
	if err := requireUser(a); err != nil {
		return err
	}
	status := "approved"
	if deny {
		status = "denied"
	}
	return c.resolvePending(ctx, a, id, accessLead, func(p pendingRow) error {
		if p.Kind != "approval" {
			return errors.New("this is a question; answer it instead")
		}
		return nil
	}, status, "board", "", "")
}

// AnswerPending answers a question from the board or the user's terminal
// (user operation).
func (c *Coordinator) AnswerPending(ctx context.Context, a Actor, id, answer string) error {
	if err := requireUser(a); err != nil {
		return err
	}
	if strings.TrimSpace(answer) == "" {
		return errors.New("answer is required")
	}
	return c.resolvePending(ctx, a, id, accessLead, func(p pendingRow) error {
		if p.Kind != "question" {
			return errors.New("this is an approval; approve or deny it instead")
		}
		return nil
	}, "answered", "board", "", answer)
}
