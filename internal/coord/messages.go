package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dlir2404/agent-shiftboard/internal/secretscan"
)

// MaxMessageLen bounds a message body in characters (D-25).
const MaxMessageLen = 500

// Message is a short note between sessions of one task ("M-n", D-25). Its
// body is held only until the board delivers it.
type Message struct {
	ID          string `json:"id"`
	TaskID      int64  `json:"task_id"`
	From        string `json:"from"` // session ID, or "" for the user
	FromLabel   string `json:"from_label"`
	To          string `json:"to"`
	ToLabel     string `json:"to_label"`
	Body        string `json:"body,omitempty"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
	DeliveredAt string `json:"delivered_at,omitempty"`
}

// MessageTarget is a session another session of the task can message.
type MessageTarget struct {
	Session  string `json:"session"`
	Label    string `json:"label"` // e.g. "codex lead 509b1bdd"
	Status   string `json:"status"`
	Assigned string `json:"assignment,omitempty"`
}

// SendMessage queues a message to another session of the same task (agent
// operation by any session of the task, or the user). to is "lead", an
// assignment ID, a session ID or unique prefix, or a provider name that
// matches exactly one other open session.
func (c *Coordinator) SendMessage(ctx context.Context, a Actor, taskID int64, to, body string) (Message, error) {
	body = strings.Join(strings.Fields(body), " ")
	if body == "" {
		return Message{}, errors.New("message text is required")
	}
	if n := utf8.RuneCountInString(body); n > MaxMessageLen {
		return Message{}, fmt.Errorf("message is %d characters; keep it to %d (put details in the brief, an item, or a file and refer to it)", n, MaxMessageLen)
	}
	if err := secretscan.Check("message", body); err != nil {
		return Message{}, err
	}
	taskID, err := c.ResolveTask(ctx, a, taskID)
	if err != nil {
		return Message{}, err
	}
	var m Message
	err = c.tx(ctx, func(tx *sql.Tx) error {
		if err := c.checkWrite(tx, taskID, a, accessMember); err != nil {
			return err
		}
		ss, l, as, err := messageContext(tx, taskID)
		if err != nil {
			return err
		}
		recipient, err := resolveRecipient(ss, l, as, a.SessionID, to)
		if err != nil {
			return err
		}
		if recipient.Status == "stopped" || recipient.Status == "failed" {
			return fmt.Errorf("%s has exited; the user can resume it from the board", sessionLabel(recipient, l))
		}
		now := c.now()
		res, err := tx.Exec(`INSERT INTO messages(task_id,from_session,to_session,body,status,created_at) VALUES(?,?,?,?,?,?)`,
			taskID, a.SessionID, recipient.ID, body, "queued", now)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		m = Message{ID: FormatID("M", id), TaskID: taskID, From: a.SessionID, FromLabel: actorLabel(ss, l, a.SessionID),
			To: recipient.ID, ToLabel: sessionLabel(recipient, l), Body: body, Status: "queued", CreatedAt: now}
		return c.event(tx, taskID, a, "message_queued", m.ID)
	})
	if err == nil {
		c.changed(taskID)
	}
	return m, err
}

// CancelMessage cancels a queued message (its sender, or the user).
func (c *Coordinator) CancelMessage(ctx context.Context, a Actor, id string) error {
	n, err := ParseID("M", id)
	if err != nil {
		return err
	}
	var taskID int64
	err = c.tx(ctx, func(tx *sql.Tx) error {
		var from, status string
		if err := tx.QueryRow(`SELECT task_id,from_session,status FROM messages WHERE id=?`, n).Scan(&taskID, &from, &status); err != nil {
			return fmt.Errorf("message %s not found", id)
		}
		if err := c.checkWrite(tx, taskID, a, accessMember); err != nil {
			return err
		}
		if !a.IsUser() && a.SessionID != from {
			return fmt.Errorf("only the sender or the user can cancel %s", id)
		}
		if status != "queued" {
			return fmt.Errorf("%s is %s", id, status)
		}
		if _, err := tx.Exec(`UPDATE messages SET status='cancelled',body='' WHERE id=?`, n); err != nil {
			return err
		}
		return c.event(tx, taskID, a, "message_cancelled", id)
	})
	if err == nil {
		c.changed(taskID)
	}
	return err
}

// ListMessages lists a task's messages, newest first (read operation).
// Bodies of delivered or cancelled messages are already cleared.
func (c *Coordinator) ListMessages(ctx context.Context, taskID int64, queuedOnly bool) ([]Message, error) {
	q := `SELECT id,task_id,from_session,to_session,body,status,created_at,delivered_at FROM messages WHERE task_id=?`
	if queuedOnly {
		q += ` AND status='queued'`
	}
	rows, err := c.DB.QueryContext(ctx, q+` ORDER BY id DESC LIMIT 50`, taskID)
	if err != nil {
		return nil, err
	}
	var out []Message
	for rows.Next() {
		var m Message
		var id int64
		if err := rows.Scan(&id, &m.TaskID, &m.From, &m.To, &m.Body, &m.Status, &m.CreatedAt, &m.DeliveredAt); err != nil {
			rows.Close()
			return nil, err
		}
		m.ID = FormatID("M", id)
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}
	ss, l, _, err := messageContext(c.DB, taskID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].FromLabel = actorLabel(ss, l, out[i].From)
		out[i].ToLabel = actorLabel(ss, l, out[i].To)
	}
	return out, nil
}

// MessageTargets lists the sessions of a task a caller can message.
func (c *Coordinator) MessageTargets(ctx context.Context, a Actor, taskID int64) ([]MessageTarget, error) {
	taskID, err := c.ResolveTask(ctx, a, taskID)
	if err != nil {
		return nil, err
	}
	ss, l, as, err := messageContext(c.DB, taskID)
	if err != nil {
		return nil, err
	}
	var out []MessageTarget
	for _, s := range ss {
		if s.ID == a.SessionID || s.Status == "stopped" || s.Status == "failed" {
			continue
		}
		t := MessageTarget{Session: s.ID, Label: sessionLabel(s, l), Status: s.Status}
		for _, x := range as {
			if x.worker == s.ID {
				t.Assigned = x.id
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// claimTimeout is how long a claim keeps other boards from typing the
// same message; after it a claim left by a crashed board is taken over.
const claimTimeout = time.Minute

// DeliverMessage claims a queued message, hands it to send, then marks it
// delivered and clears its body. The claim is atomic, so overlapping relays
// never both type it; typing comes before the record, so a crash cannot
// lose a message (after claimTimeout another relay types it again). If
// send fails, the claim is released and nothing else changes. It reports
// false when the message was not delivered by this call.
func (c *Coordinator) DeliverMessage(ctx context.Context, id string, send func(Message) error) (bool, error) {
	n, err := ParseID("M", id)
	if err != nil {
		return false, err
	}
	var m Message
	claim := c.now()
	claimed := false
	err = c.tx(ctx, func(tx *sql.Tx) error {
		var nid int64
		var prev string
		err := tx.QueryRow(`SELECT id,task_id,from_session,to_session,body,status,created_at,claimed_at FROM messages WHERE id=?`, n).
			Scan(&nid, &m.TaskID, &m.From, &m.To, &m.Body, &m.Status, &m.CreatedAt, &prev)
		if err != nil {
			return fmt.Errorf("message %s not found", id)
		}
		if m.Status != "queued" || (prev != "" && c.Now().Sub(parseTime(prev)) < claimTimeout) {
			return nil
		}
		ss, l, _, err := messageContext(tx, m.TaskID)
		if err != nil {
			return err
		}
		m.ID = FormatID("M", nid)
		m.FromLabel, m.ToLabel = actorLabel(ss, l, m.From), actorLabel(ss, l, m.To)
		if _, err := tx.Exec(`UPDATE messages SET claimed_at=? WHERE id=?`, claim, n); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	if err != nil || !claimed {
		return false, err
	}
	if err := send(m); err != nil {
		_ = c.tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE messages SET claimed_at='' WHERE id=? AND claimed_at=?`, n, claim)
			return err
		})
		return false, err
	}
	marked := false
	err = c.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE messages SET status='delivered',body='',delivered_at=?,claimed_at='' WHERE id=? AND status='queued'`, c.now(), n)
		if err != nil {
			return err
		}
		if k, _ := res.RowsAffected(); k == 0 {
			return nil // cancelled while it was being typed
		}
		marked = true
		return c.event(tx, m.TaskID, User, "message_delivered", m.ID)
	})
	if err != nil || !marked {
		return false, err
	}
	c.changed(m.TaskID)
	return true, nil
}

// QueuedMessages lists a task's undelivered messages, oldest first, with
// their bodies (for delivery).
func (c *Coordinator) QueuedMessages(ctx context.Context, taskID int64) ([]Message, error) {
	rows, err := c.DB.QueryContext(ctx, `SELECT id,task_id,from_session,to_session,body,status,created_at FROM messages
	 WHERE task_id=? AND status='queued' ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var id int64
		if err := rows.Scan(&id, &m.TaskID, &m.From, &m.To, &m.Body, &m.Status, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.ID = FormatID("M", id)
		out = append(out, m)
	}
	return out, rows.Err()
}

type messageSession struct {
	ID, Provider, Role, Status string
}

type messageAssignment struct{ id, worker string }

type querier interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

func messageContext(tx querier, taskID int64) ([]messageSession, leaseInfo, []messageAssignment, error) {
	var l leaseInfo
	err := tx.QueryRow(`SELECT state,holder_session,pending_session,updated_at FROM leases WHERE task_id=?`, taskID).
		Scan(&l.State, &l.Holder, &l.Pending, &l.Updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, l, nil, fmt.Errorf("task #%d not found", taskID)
	}
	if err != nil {
		return nil, l, nil, err
	}
	rows, err := tx.Query(`SELECT id,provider,role,status FROM sessions WHERE task_id=? ORDER BY created_at`, taskID)
	if err != nil {
		return nil, l, nil, err
	}
	var ss []messageSession
	for rows.Next() {
		var s messageSession
		if err := rows.Scan(&s.ID, &s.Provider, &s.Role, &s.Status); err != nil {
			rows.Close()
			return nil, l, nil, err
		}
		ss = append(ss, s)
	}
	rows.Close()
	rows, err = tx.Query(`SELECT id,worker_session FROM assignments WHERE task_id=? AND worker_session<>''`, taskID)
	if err != nil {
		return nil, l, nil, err
	}
	defer rows.Close()
	var as []messageAssignment
	for rows.Next() {
		var id int64
		var w string
		if err := rows.Scan(&id, &w); err != nil {
			return nil, l, nil, err
		}
		as = append(as, messageAssignment{FormatID("A", id), w})
	}
	return ss, l, as, rows.Err()
}

// currentLead is the session the lead role belongs to: the incoming
// session during a handoff, otherwise the holder.
func currentLead(l leaseInfo) string {
	if l.State == "handoff_pending" && l.Pending != "" {
		return l.Pending
	}
	return l.Holder
}

func sessionLabel(s messageSession, l leaseInfo) string {
	role := s.Role
	switch {
	case s.ID == currentLead(l):
		role = "lead"
	case s.Role == "lead":
		role = "former lead"
	}
	return fmt.Sprintf("%s %s %s", s.Provider, role, shortID(s.ID))
}

func actorLabel(ss []messageSession, l leaseInfo, id string) string {
	if id == "" {
		return "user"
	}
	for _, s := range ss {
		if s.ID == id {
			return sessionLabel(s, l)
		}
	}
	return shortID(id)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func resolveRecipient(ss []messageSession, l leaseInfo, as []messageAssignment, sender, to string) (messageSession, error) {
	to = strings.TrimSpace(to)
	if to == "" {
		return messageSession{}, errors.New("a recipient is required: lead, an assignment ID, a session ID, or a provider name")
	}
	find := func(id string) (messageSession, bool) {
		for _, s := range ss {
			if s.ID == id {
				return s, true
			}
		}
		return messageSession{}, false
	}
	pick := func(id string) (messageSession, error) {
		s, ok := find(id)
		switch {
		case !ok:
			return s, fmt.Errorf("session %s is not part of this task", shortID(id))
		case s.ID == sender:
			return s, errors.New("that is your own session")
		}
		return s, nil
	}
	low := strings.ToLower(to)
	if low == "lead" {
		lead := currentLead(l)
		if lead == "" {
			return messageSession{}, errors.New("this task has no lead")
		}
		return pick(lead)
	}
	if strings.HasPrefix(strings.ToUpper(to), "A-") {
		for _, x := range as {
			if strings.EqualFold(x.id, to) {
				return pick(x.worker)
			}
		}
		return messageSession{}, fmt.Errorf("assignment %s has no worker session in this task", strings.ToUpper(to))
	}
	if _, ok := find(to); ok {
		return pick(to)
	}
	var matches []messageSession
	for _, s := range ss {
		if s.ID == sender {
			continue
		}
		open := s.Status != "stopped" && s.Status != "failed"
		if (strings.EqualFold(s.Provider, to) && open) || (len(to) >= 6 && strings.HasPrefix(s.ID, low)) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return messageSession{}, fmt.Errorf("no open session of this task matches %q; list targets with `message list`", to)
	}
	var names []string
	for _, s := range matches {
		names = append(names, sessionLabel(s, l))
	}
	return messageSession{}, fmt.Errorf("%q matches several sessions (%s); use a session ID", to, strings.Join(names, ", "))
}
