package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dlir2404/agent-shiftboard/internal/secretscan"
)

// Delegation is standing authority granted by the user ("G-n").
type Delegation struct {
	ID         string          `json:"id"`
	TaskID     int64           `json:"task_id"`
	Scope      string          `json:"scope"`
	Exclusions string          `json:"exclusions,omitempty"`
	GrantedBy  string          `json:"granted_by"`
	GrantedAt  string          `json:"granted_at"`
	ExpiresAt  string          `json:"expires_at"`
	Status     string          `json:"status"`
	Log        []DelegationLog `json:"log,omitempty"`
}

// DelegationLog is one decision taken under a delegation.
type DelegationLog struct {
	Decision    string `json:"decision"`
	SessionID   string `json:"session"`
	At          string `json:"at"`
	NeedsReview bool   `json:"needs_review"`
}

// ProposeDelegation records a delegation the user stated. From an agent it
// stays proposed until the user confirms it; from the user it is active.
func (c *Coordinator) ProposeDelegation(ctx context.Context, a Actor, taskID int64, scope, exclusions, grantedBy string, expiresAt time.Time) (string, error) {
	scope = strings.TrimSpace(scope)
	if scope == "" || strings.TrimSpace(grantedBy) == "" {
		return "", errors.New("scope and granted-by are required")
	}
	if !expiresAt.After(c.Now()) {
		return "", errors.New("a delegation needs an expiry in the future")
	}
	if err := secretscan.Check("delegation", scope+" "+exclusions); err != nil {
		return "", err
	}
	taskID, err := c.ResolveTask(ctx, a, taskID)
	if err != nil {
		return "", err
	}
	status := "proposed"
	if a.IsUser() {
		status = "active"
	}
	var id int64
	err = c.tx(ctx, func(tx *sql.Tx) error {
		if err := c.checkWrite(tx, taskID, a, accessLead); err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO delegations(task_id,scope,exclusions,granted_by,granted_at,expires_at,status,created_by) VALUES(?,?,?,?,?,?,?,?)`,
			taskID, scope, exclusions, grantedBy, c.now(), expiresAt.UTC().Format(time.RFC3339), status, a.String())
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return c.event(tx, taskID, a, "delegation_"+status, FormatID("G", id))
	})
	if err == nil {
		c.changed(taskID)
	}
	return FormatID("G", id), err
}

// ConfirmDelegation activates a proposed delegation (user operation).
func (c *Coordinator) ConfirmDelegation(ctx context.Context, a Actor, id string) error {
	return c.setDelegation(ctx, a, id, "proposed", "active")
}

// RevokeDelegation revokes a delegation (user operation).
func (c *Coordinator) RevokeDelegation(ctx context.Context, a Actor, id string) error {
	return c.setDelegation(ctx, a, id, "", "revoked")
}

func (c *Coordinator) setDelegation(ctx context.Context, a Actor, id, from, to string) error {
	if err := requireUser(a); err != nil {
		return err
	}
	n, err := ParseID("G", id)
	if err != nil {
		return err
	}
	var taskID int64
	err = c.tx(ctx, func(tx *sql.Tx) error {
		var cur string
		if err := tx.QueryRow(`SELECT task_id,status FROM delegations WHERE id=?`, n).Scan(&taskID, &cur); err != nil {
			return fmt.Errorf("delegation %s not found", id)
		}
		if from != "" && cur != from {
			return fmt.Errorf("delegation %s is %s", id, cur)
		}
		if cur == "revoked" || cur == "expired" {
			return fmt.Errorf("delegation %s is already %s", id, cur)
		}
		if _, err := tx.Exec(`UPDATE delegations SET status=? WHERE id=?`, to, n); err != nil {
			return err
		}
		return c.event(tx, taskID, a, "delegation_"+to, id)
	})
	if err == nil {
		c.changed(taskID)
	}
	return err
}

// LogDecision records a decision taken under an active delegation
// (agent operation). Decisions are marked for the user's review.
func (c *Coordinator) LogDecision(ctx context.Context, a Actor, id, decision string) error {
	n, err := ParseID("G", id)
	if err != nil {
		return err
	}
	if strings.TrimSpace(decision) == "" {
		return errors.New("decision text is required")
	}
	if err := secretscan.Check("decision", decision); err != nil {
		return err
	}
	var taskID int64
	err = c.tx(ctx, func(tx *sql.Tx) error {
		var status, expires string
		if err := tx.QueryRow(`SELECT task_id,status,expires_at FROM delegations WHERE id=?`, n).Scan(&taskID, &status, &expires); err != nil {
			return fmt.Errorf("delegation %s not found", id)
		}
		if err := c.checkWrite(tx, taskID, a, accessLead); err != nil {
			return err
		}
		if status == "active" && c.Now().After(parseTime(expires)) {
			tx.Exec(`UPDATE delegations SET status='expired' WHERE id=?`, n)
			status = "expired"
		}
		if status != "active" {
			return fmt.Errorf("delegation %s is %s; decisions cannot be taken under it", id, status)
		}
		if _, err := tx.Exec(`INSERT INTO delegation_log(delegation_id,decision,session_id,at,needs_review) VALUES(?,?,?,?,1)`,
			n, decision, a.String(), c.now()); err != nil {
			return err
		}
		return c.event(tx, taskID, a, "delegation_decision", id)
	})
	if err == nil {
		c.changed(taskID)
	}
	return err
}

// ReviewDecisions marks all logged decisions of a delegation as reviewed
// (user operation).
func (c *Coordinator) ReviewDecisions(ctx context.Context, a Actor, id string) error {
	if err := requireUser(a); err != nil {
		return err
	}
	n, err := ParseID("G", id)
	if err != nil {
		return err
	}
	_, err = c.DB.ExecContext(ctx, `UPDATE delegation_log SET needs_review=0 WHERE delegation_id=?`, n)
	return err
}

// ListDelegations lists a task's delegations with their logs (read operation).
// Expired delegations are marked on read.
func (c *Coordinator) ListDelegations(ctx context.Context, taskID int64, activeOnly bool) ([]Delegation, error) {
	now := c.now()
	if _, err := c.DB.ExecContext(ctx, `UPDATE delegations SET status='expired' WHERE status IN ('active','proposed') AND expires_at<?`, now); err != nil {
		return nil, err
	}
	q := `SELECT id,task_id,scope,exclusions,granted_by,granted_at,expires_at,status FROM delegations WHERE task_id=?`
	if activeOnly {
		q += ` AND status='active'`
	}
	rows, err := c.DB.QueryContext(ctx, q+` ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	var out []Delegation
	var ids []int64
	for rows.Next() {
		var d Delegation
		var id int64
		if err := rows.Scan(&id, &d.TaskID, &d.Scope, &d.Exclusions, &d.GrantedBy, &d.GrantedAt, &d.ExpiresAt, &d.Status); err != nil {
			rows.Close()
			return nil, err
		}
		d.ID = FormatID("G", id)
		out = append(out, d)
		ids = append(ids, id)
	}
	rows.Close()
	for i, id := range ids {
		lr, err := c.DB.QueryContext(ctx, `SELECT decision,session_id,at,needs_review FROM delegation_log WHERE delegation_id=? ORDER BY id`, id)
		if err != nil {
			return nil, err
		}
		for lr.Next() {
			var l DelegationLog
			var nr int
			if err := lr.Scan(&l.Decision, &l.SessionID, &l.At, &nr); err != nil {
				lr.Close()
				return nil, err
			}
			l.NeedsReview = nr == 1
			out[i].Log = append(out[i].Log, l)
		}
		lr.Close()
	}
	return out, nil
}
