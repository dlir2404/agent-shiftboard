package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/dlir2404/agent-shiftboard/internal/vocab"
)

// Session is a managed agent session.
type Session struct {
	ID              string `json:"id"`
	TaskID          int64  `json:"task_id"`
	Provider        string `json:"provider"`
	Role            string `json:"role"`
	NativeSessionID string `json:"native_session_id,omitempty"`
	PaneID          string `json:"pane_id,omitempty"`
	PID             int    `json:"pid,omitempty"`
	PermissionMode  string `json:"permission_mode,omitempty"`
	Status          string `json:"status"`
	Reason          string `json:"reason,omitempty"`
	StatusSource    string `json:"status_source"`
	ObservedAt      string `json:"observed_at"`
	TurnsSinceBrief int    `json:"turns_since_brief"`
}

// SessionSpec describes a session to register before launching it.
type SessionSpec struct {
	Provider        string
	Role            string // lead | worker
	NativeSessionID string // pre-assigned where the CLI supports it
	PermissionMode  string
}

func (c *Coordinator) insertSession(tx *sql.Tx, taskID int64, s SessionSpec) (string, error) {
	if s.Provider == "" {
		return "", errors.New("provider is required")
	}
	if s.Role != "lead" && s.Role != "worker" {
		return "", fmt.Errorf("role must be lead or worker, got %q", s.Role)
	}
	id := uuid.NewString()
	now := c.now()
	_, err := tx.Exec(`INSERT INTO sessions(id,task_id,provider,role,native_session_id,permission_mode,status,status_source,observed_at,created_at)
	 VALUES(?,?,?,?,?,?,?,?,?,?)`, id, taskID, s.Provider, s.Role, s.NativeSessionID, s.PermissionMode, "starting", "process", now, now)
	return id, err
}

// StartLead registers a new lead session for a task (user operation:
// `run`, `lead new`, `lead switch`). The lease moves to handoff_pending:
// from vacant (first lead), from suspended (after a failure), or directly
// from held (voluntary handoff). The new session must acknowledge by
// readback before it holds the lease.
func (c *Coordinator) StartLead(ctx context.Context, a Actor, taskID int64, spec SessionSpec) (string, error) {
	if err := requireUser(a); err != nil {
		return "", err
	}
	spec.Role = "lead"
	var sid string
	err := c.tx(ctx, func(tx *sql.Tx) error {
		st, err := taskStatus(tx, taskID)
		if err != nil {
			return err
		}
		if st == "done" || st == "cancelled" {
			return fmt.Errorf("task #%d is %s; reopen it first", taskID, st)
		}
		l, err := loadLease(tx, taskID)
		if err != nil {
			return err
		}
		if l.State == "handoff_pending" {
			return fmt.Errorf("task #%d already has a new lead session waiting to acknowledge (%s)", taskID, l.Pending)
		}
		sid, err = c.insertSession(tx, taskID, spec)
		if err != nil {
			return err
		}
		note := map[string]string{"vacant": "first lead", "suspended": "after failure", "held": "voluntary handoff"}[l.State]
		if _, err := tx.Exec(`UPDATE leases SET state='handoff_pending',pending_session=?,note=?,updated_at=? WHERE task_id=?`,
			sid, note, c.now(), taskID); err != nil {
			return err
		}
		return c.event(tx, taskID, a, "lead_starting", fmt.Sprintf("%s %s (%s)", spec.Provider, sid, note))
	})
	if err == nil {
		c.changed(taskID)
	}
	return sid, err
}

// RegisterWorker registers a worker session for an assignment.
func (c *Coordinator) RegisterWorker(ctx context.Context, a Actor, assignmentID string, spec SessionSpec) (string, error) {
	n, err := ParseID("A", assignmentID)
	if err != nil {
		return "", err
	}
	spec.Role = "worker"
	var sid string
	var taskID int64
	err = c.tx(ctx, func(tx *sql.Tx) error {
		var status, worker string
		if err := tx.QueryRow(`SELECT task_id,status,worker_session FROM assignments WHERE id=?`, n).Scan(&taskID, &status, &worker); err != nil {
			return fmt.Errorf("assignment %s not found", assignmentID)
		}
		if err := c.checkWrite(tx, taskID, a, accessLead); err != nil {
			return err
		}
		if status != "assigned" && status != "running" {
			return fmt.Errorf("assignment %s is %s", assignmentID, status)
		}
		if worker != "" {
			// A worker that is gone can be replaced; a live one cannot.
			var ws string
			if err := tx.QueryRow(`SELECT status FROM sessions WHERE id=?`, worker).Scan(&ws); err == nil && ws != "stopped" && ws != "failed" {
				return fmt.Errorf("assignment %s already has a live worker (%s)", assignmentID, ws)
			}
			if _, err := tx.Exec(`UPDATE assignments SET status='assigned' WHERE id=?`, n); err != nil {
				return err
			}
		}
		sid, err = c.insertSession(tx, taskID, spec)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE assignments SET worker_session=?,provider=?,updated_at=? WHERE id=?`, sid, spec.Provider, c.now(), n)
		return err
	})
	if err == nil {
		c.changed(taskID)
	}
	return sid, err
}

// SetSessionRuntime records where a session runs (pane, pid, native ID).
func (c *Coordinator) SetSessionRuntime(ctx context.Context, sessionID, paneID string, pid int, nativeID string) error {
	_, err := c.DB.ExecContext(ctx, `UPDATE sessions SET pane_id=COALESCE(NULLIF(?,''),pane_id), pid=CASE WHEN ?>0 THEN ? ELSE pid END,
	 native_session_id=COALESCE(NULLIF(?,''),native_session_id) WHERE id=?`, paneID, pid, pid, nativeID, sessionID)
	return err
}

// Observe records a status observation for a session and applies the
// contract's lease, side-effect, assignment, and lock rules. It is called
// by hook handlers (as the session itself) and by the board (as the user).
func (c *Coordinator) Observe(ctx context.Context, a Actor, sessionID, status, reason, source string) error {
	if !a.IsUser() && a.SessionID != sessionID {
		return errors.New("a session may only report its own status")
	}
	if err := vocab.SessionStatus.Check(status); err != nil {
		return err
	}
	if err := vocab.ObservationSrc.Check(source); err != nil {
		return err
	}
	if reason != "" {
		if err := vocab.StopReason.Check(reason); err != nil {
			return err
		}
	}
	var taskID int64
	err := c.tx(ctx, func(tx *sql.Tx) error {
		s, err := loadSession(tx, sessionID)
		if err != nil {
			return err
		}
		taskID = s.TaskID
		return c.applyObservation(tx, s, status, reason, source)
	})
	if err == nil {
		c.changed(taskID)
	}
	return err
}

func (c *Coordinator) applyObservation(tx *sql.Tx, s sessionInfo, status, reason, source string) error {
	now := c.now()
	unknownSince := ""
	if status == "unknown" {
		if s.Status == "unknown" {
			if err := tx.QueryRow(`SELECT unknown_since FROM sessions WHERE id=?`, s.ID).Scan(&unknownSince); err != nil {
				return err
			}
		}
		if unknownSince == "" {
			unknownSince = now
		}
	}
	if _, err := tx.Exec(`UPDATE sessions SET status=?,reason=?,status_source=?,observed_at=?,unknown_since=? WHERE id=?`,
		status, reason, source, now, unknownSince, s.ID); err != nil {
		return err
	}
	if status != s.Status {
		if err := c.event(tx, s.TaskID, Actor{SessionID: s.ID}, "status", fmt.Sprintf("%s -> %s %s", s.Status, status, reason)); err != nil {
			return err
		}
	}
	// Side effects.
	switch {
	case status == "idle":
		if err := c.markEffectsUnknown(tx, s.ID, false); err != nil {
			return err
		}
	case vocab.Suspends(status):
		if err := c.markEffectsUnknown(tx, s.ID, true); err != nil {
			return err
		}
	}
	// Assignment progress.
	if status == "running" && s.Role == "worker" {
		if _, err := tx.Exec(`UPDATE assignments SET status='running',updated_at=? WHERE worker_session=? AND status='assigned'`, now, s.ID); err != nil {
			return err
		}
	}
	// Locks held by a session that is gone become stale.
	if status == "stopped" || status == "failed" {
		if _, err := tx.Exec(`UPDATE locks SET status='stale',updated_at=? WHERE holder_session=? AND status='held'`, now, "session:"+s.ID); err != nil {
			return err
		}
	}
	// Lease.
	l, err := loadLease(tx, s.TaskID)
	if err != nil {
		return err
	}
	if l.Holder == s.ID {
		switch {
		case l.State == "held" && vocab.Suspends(status):
			return c.setLease(tx, s.TaskID, "suspended", l.Holder, "", "holder "+status+" "+reason)
		case l.State == "suspended" && (status == "idle" || status == "running"):
			return c.setLease(tx, s.TaskID, "held", l.Holder, "", "holder recovered")
		}
	}
	return nil
}

func (c *Coordinator) setLease(tx *sql.Tx, taskID int64, state, holder, pending, note string) error {
	if err := vocab.LeaseState.Check(state); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE leases SET state=?,holder_session=?,pending_session=?,note=?,updated_at=? WHERE task_id=?`,
		state, holder, pending, note, c.now(), taskID); err != nil {
		return err
	}
	return c.event(tx, taskID, User, "lease_"+state, note)
}

// ObserveTurn counts a turn boundary for stale-brief detection.
func (c *Coordinator) ObserveTurn(ctx context.Context, sessionID string) error {
	_, err := c.DB.ExecContext(ctx, `UPDATE sessions SET turns_since_brief=turns_since_brief+1 WHERE id=?`, sessionID)
	return err
}

// RequiredReadback lists the IDs a new lead must acknowledge: active rules,
// active delegations, open pending items, and unknown side effects.
func (c *Coordinator) RequiredReadback(ctx context.Context, taskID int64) ([]string, error) {
	var ids []string
	rules, err := c.ListRules(ctx, taskID, true)
	if err != nil {
		return nil, err
	}
	for _, r := range rules {
		ids = append(ids, r.ID)
	}
	dels, err := c.ListDelegations(ctx, taskID, true)
	if err != nil {
		return nil, err
	}
	for _, d := range dels {
		ids = append(ids, d.ID)
	}
	pend, err := c.ListPending(ctx, taskID, true)
	if err != nil {
		return nil, err
	}
	for _, p := range pend {
		ids = append(ids, p.ID)
	}
	effs, err := c.ListEffects(ctx, taskID, "unknown")
	if err != nil {
		return nil, err
	}
	for _, e := range effs {
		ids = append(ids, e.ID)
	}
	return ids, nil
}

// AckHandoff completes a handoff when the pending session reads back every
// required ID. The lease becomes held by that session.
func (c *Coordinator) AckHandoff(ctx context.Context, a Actor, ids []string) error {
	if a.IsUser() {
		return errors.New("only the new lead session acknowledges a handoff")
	}
	s, err := loadSession(c.DB, a.SessionID)
	if err != nil {
		return err
	}
	required, err := c.RequiredReadback(ctx, s.TaskID)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, id := range ids {
		have[strings.ToUpper(strings.TrimSpace(id))] = true
	}
	var missing []string
	for _, id := range required {
		if !have[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("handoff not acknowledged; read and list these IDs too: %s", strings.Join(missing, ", "))
	}
	err = c.tx(ctx, func(tx *sql.Tx) error {
		l, err := loadLease(tx, s.TaskID)
		if err != nil {
			return err
		}
		if l.State != "handoff_pending" || l.Pending != a.SessionID {
			return errors.New("this session has no pending handoff to acknowledge")
		}
		old := l.Holder
		if err := c.setLease(tx, s.TaskID, "held", a.SessionID, "", "acknowledged; previous holder "+orNone(old)); err != nil {
			return err
		}
		st, err := taskStatus(tx, s.TaskID)
		if err != nil {
			return err
		}
		if st == "queued" {
			if err := c.setTaskStatus(tx, s.TaskID, "active"); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE sessions SET turns_since_brief=0 WHERE id=?`, a.SessionID); err != nil {
			return err
		}
		return c.event(tx, s.TaskID, a, "handoff_ack", strings.Join(required, ","))
	})
	if err == nil {
		c.changed(s.TaskID)
	}
	return err
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// ConfirmSwap records the user's confirmation to show the lead in the main
// slot (user operation). The tmux swap itself is done by the workspace.
func (c *Coordinator) ConfirmSwap(ctx context.Context, a Actor, taskID int64) error {
	if err := requireUser(a); err != nil {
		return err
	}
	return c.tx(ctx, func(tx *sql.Tx) error { return c.event(tx, taskID, a, "swap_confirmed", "") })
}

// Sweep applies time-based rules: running sessions without observations
// become unknown; unknown past the grace period suspends the lease and
// marks open effects unknown; expired handoff acknowledgements fall back.
func (c *Coordinator) Sweep(ctx context.Context) error {
	now := c.Now()
	window := time.Duration(c.Policy.ObservationWindowSeconds) * time.Second
	grace := time.Duration(c.Policy.UnknownGraceSeconds) * time.Second
	ackTimeout := time.Duration(c.Policy.HandoffAckTimeoutSeconds) * time.Second
	var touched []int64
	err := c.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT id,task_id,role,status,observed_at,unknown_since FROM sessions WHERE status IN ('starting','running','retrying','unknown')`)
		if err != nil {
			return err
		}
		type row struct {
			s                sessionInfo
			observed, unkSnc string
		}
		var list []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.s.ID, &r.s.TaskID, &r.s.Role, &r.s.Status, &r.observed, &r.unkSnc); err != nil {
				rows.Close()
				return err
			}
			list = append(list, r)
		}
		rows.Close()
		for _, r := range list {
			switch {
			case r.s.Status != "unknown" && now.Sub(parseTime(r.observed)) > window:
				if err := c.applyObservation(tx, r.s, "unknown", "unknown", "process"); err != nil {
					return err
				}
				touched = append(touched, r.s.TaskID)
			case r.s.Status == "unknown" && r.unkSnc != "" && now.Sub(parseTime(r.unkSnc)) > grace:
				if err := c.markEffectsUnknown(tx, r.s.ID, true); err != nil {
					return err
				}
				l, err := loadLease(tx, r.s.TaskID)
				if err != nil {
					return err
				}
				if l.State == "held" && l.Holder == r.s.ID {
					if err := c.setLease(tx, r.s.TaskID, "suspended", l.Holder, "", "holder unknown past grace period"); err != nil {
						return err
					}
					touched = append(touched, r.s.TaskID)
				}
			}
		}
		lrows, err := tx.Query(`SELECT task_id,holder_session,pending_session,updated_at FROM leases WHERE state='handoff_pending'`)
		if err != nil {
			return err
		}
		type pend struct {
			task            int64
			holder, pending string
			updated         string
		}
		var pl []pend
		for lrows.Next() {
			var p pend
			if err := lrows.Scan(&p.task, &p.holder, &p.pending, &p.updated); err != nil {
				lrows.Close()
				return err
			}
			pl = append(pl, p)
		}
		lrows.Close()
		for _, p := range pl {
			if now.Sub(parseTime(p.updated)) <= ackTimeout {
				continue
			}
			// A new session that has reported (idle, running, ...) is alive
			// and waiting for the user's first message; only a session that
			// never reported times out.
			var st string
			_ = tx.QueryRow(`SELECT status FROM sessions WHERE id=?`, p.pending).Scan(&st)
			if st != "starting" {
				continue
			}
			state := "suspended"
			if p.holder == "" {
				state = "vacant"
			}
			if _, err := tx.Exec(`UPDATE sessions SET status='failed',reason='unknown',observed_at=? WHERE id=?`, c.now(), p.pending); err != nil {
				return err
			}
			if err := c.setLease(tx, p.task, state, p.holder, "", "handoff acknowledgement timed out for "+p.pending); err != nil {
				return err
			}
			touched = append(touched, p.task)
		}
		_, err = tx.Exec(`UPDATE pending SET status='expired',resolved_at=? WHERE status='pending' AND expires_at!='' AND expires_at<?`, c.now(), c.now())
		return err
	})
	if err == nil {
		for _, t := range touched {
			c.changed(t)
		}
	}
	return err
}

// ListSessions lists a task's sessions (read operation); taskID 0 lists all.
func (c *Coordinator) ListSessions(ctx context.Context, taskID int64) ([]Session, error) {
	q := `SELECT id,task_id,provider,role,native_session_id,pane_id,pid,permission_mode,status,reason,status_source,observed_at,turns_since_brief FROM sessions`
	var args []any
	if taskID > 0 {
		q += ` WHERE task_id=?`
		args = append(args, taskID)
	}
	rows, err := c.DB.QueryContext(ctx, q+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.TaskID, &s.Provider, &s.Role, &s.NativeSessionID, &s.PaneID, &s.PID, &s.PermissionMode, &s.Status, &s.Reason, &s.StatusSource, &s.ObservedAt, &s.TurnsSinceBrief); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// StaleReasons reports why a task's brief is stale (D-17): Git HEAD moved
// since the brief was written, or too many turns passed. Unrecorded
// external actions are blocked at the hook instead.
func (c *Coordinator) StaleReasons(ctx context.Context, taskID int64) ([]string, error) {
	t, err := c.GetTask(ctx, taskID)
	if err != nil || t.LeadSession == "" {
		return nil, err
	}
	var reasons []string
	var recorded string
	_ = c.DB.QueryRowContext(ctx, `SELECT head FROM briefs WHERE task_id=? AND revision=?`, taskID, t.BriefRevision).Scan(&recorded)
	if cur := c.taskHead(c.DB, taskID); recorded != "" && cur != "" && cur != recorded {
		reasons = append(reasons, fmt.Sprintf("HEAD moved from %.7s to %.7s since brief r%d", recorded, cur, t.BriefRevision))
	}
	var turns int
	if err := c.DB.QueryRowContext(ctx, `SELECT turns_since_brief FROM sessions WHERE id=?`, t.LeadSession).Scan(&turns); err != nil {
		return nil, err
	}
	if turns >= c.Policy.StaleTurns {
		reasons = append(reasons, fmt.Sprintf("%d turns since the last brief revision", turns))
	}
	return reasons, nil
}

// CancelPendingHandoff abandons a new lead session that never acknowledged
// (user operation). The session is marked failed and the lease returns to
// suspended (if there is a previous holder) or vacant.
func (c *Coordinator) CancelPendingHandoff(ctx context.Context, a Actor, taskID int64) error {
	if err := requireUser(a); err != nil {
		return err
	}
	err := c.tx(ctx, func(tx *sql.Tx) error {
		l, err := loadLease(tx, taskID)
		if err != nil {
			return err
		}
		if l.State != "handoff_pending" {
			return fmt.Errorf("task #%d has no pending handoff", taskID)
		}
		if _, err := tx.Exec(`UPDATE sessions SET status='failed',reason='user_stopped',observed_at=? WHERE id=?`, c.now(), l.Pending); err != nil {
			return err
		}
		state := "suspended"
		if l.Holder == "" {
			state = "vacant"
		}
		return c.setLease(tx, taskID, state, l.Holder, "", "pending handoff to "+l.Pending+" cancelled by user")
	})
	if err == nil {
		c.changed(taskID)
	}
	return err
}
