// Package coord is the coordinator: AgentShiftboard's core state logic over
// SQLite. It is not a daemon; every entry point (CLI, MCP server, hook
// handler, board) runs it in-process (docs/STATE-CONTRACT.md).
package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dlir2404/agent-shiftboard/internal/paths"
	"github.com/dlir2404/agent-shiftboard/internal/policy"
	"github.com/dlir2404/agent-shiftboard/internal/store"
)

// SessionEnv is set only in the environment of managed agent processes.
const SessionEnv = "AGENT_SHIFTBOARD_SESSION_ID"

// Actor is who performs an operation: the user, or a managed agent session.
type Actor struct {
	SessionID string
}

// User is the human user.
var User = Actor{}

// ActorFromEnv returns the agent session named by SessionEnv, or the user.
func ActorFromEnv() Actor { return Actor{SessionID: os.Getenv(SessionEnv)} }

// IsUser reports whether the actor is the human user.
func (a Actor) IsUser() bool { return a.SessionID == "" }

func (a Actor) String() string {
	if a.IsUser() {
		return "user"
	}
	return "session:" + a.SessionID
}

// ErrUserOnly is returned when an agent session calls a user operation.
var ErrUserOnly = errors.New("this is a user operation; it cannot run inside a managed agent session (use the board or your own terminal)")

func requireUser(a Actor) error {
	if !a.IsUser() {
		return ErrUserOnly
	}
	return nil
}

// Coordinator holds open databases and policy for one project.
type Coordinator struct {
	Root   string
	DB     *sql.DB // project database
	UserDB *sql.DB // user-scope rules and resources
	Policy policy.Policy
	Now    func() time.Time
	// OnTaskChange is called after a committed write that affects a task;
	// the CLI uses it to re-render the brief file.
	OnTaskChange func(taskID int64)
}

// Open opens the project at root and the user database.
func Open(root string) (*Coordinator, error) {
	db, err := store.Open(paths.ProjectDB(root))
	if err != nil {
		return nil, err
	}
	udbPath, err := paths.UserDB()
	if err != nil {
		db.Close()
		return nil, err
	}
	udb, err := store.Open(udbPath)
	if err != nil {
		// The user-scope database lives outside the project; an agent
		// sandbox may forbid writing there. Fall back to reading it (or to
		// an empty one) so project operations keep working; user-scope
		// writes then fail with a clear error.
		if udb, err = store.OpenReadOnly(udbPath); err != nil {
			if udb, err = store.OpenMemory(); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	userDir, _ := paths.UserDir()
	pol, err := policy.Load(userDir, paths.StateDir(root))
	if err != nil {
		db.Close()
		udb.Close()
		return nil, err
	}
	return &Coordinator{Root: root, DB: db, UserDB: udb, Policy: pol, Now: time.Now}, nil
}

// Close closes both databases.
func (c *Coordinator) Close() error {
	err1 := c.DB.Close()
	err2 := c.UserDB.Close()
	return errors.Join(err1, err2)
}

func (c *Coordinator) now() string { return c.Now().UTC().Format(time.RFC3339) }

func (c *Coordinator) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	return store.Tx(ctx, c.DB, fn)
}

func (c *Coordinator) changed(taskID int64) {
	if c.OnTaskChange != nil && taskID > 0 {
		c.OnTaskChange(taskID)
	}
}

func (c *Coordinator) event(tx *sql.Tx, taskID int64, actor Actor, kind, detail string) error {
	var tid any
	if taskID > 0 {
		tid = taskID
	}
	_, err := tx.Exec(`INSERT INTO events(task_id,at,actor,kind,detail) VALUES(?,?,?,?,?)`,
		tid, c.now(), actor.String(), kind, detail)
	return err
}

// FormatID renders a record ID with its prefix, e.g. ("P", 4) -> "P-4".
func FormatID(prefix string, id int64) string { return fmt.Sprintf("%s-%d", prefix, id) }

// ParseID parses "P-4" (or "4") for the given prefix.
func ParseID(prefix, s string) (int64, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, prefix+"-"), "#")
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid %s ID %q", prefix, s)
	}
	return n, nil
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// sessionInfo is the subset of a session row the write guards need.
type sessionInfo struct {
	ID     string
	TaskID int64
	Role   string
	Status string
}

func loadSession(q interface {
	QueryRow(string, ...any) *sql.Row
}, id string) (sessionInfo, error) {
	var s sessionInfo
	err := q.QueryRow(`SELECT id,task_id,role,status FROM sessions WHERE id=?`, id).
		Scan(&s.ID, &s.TaskID, &s.Role, &s.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return s, fmt.Errorf("unknown session %s", id)
	}
	return s, err
}

type leaseInfo struct {
	State   string
	Holder  string
	Pending string
	Updated string
}

func loadLease(tx *sql.Tx, taskID int64) (leaseInfo, error) {
	var l leaseInfo
	err := tx.QueryRow(`SELECT state,holder_session,pending_session,updated_at FROM leases WHERE task_id=?`, taskID).
		Scan(&l.State, &l.Holder, &l.Pending, &l.Updated)
	if errors.Is(err, sql.ErrNoRows) {
		return l, fmt.Errorf("task #%d not found", taskID)
	}
	return l, err
}

// writeAccess describes what an actor may write on a task.
type writeAccess int

const (
	accessLead   writeAccess = iota // lease holder (or the user)
	accessMember                    // any session of the task in good standing
)

// checkWrite enforces the lease write rules: once a lease is
// handoff_pending, the old holder cannot write and the new session's first
// accepted write must be its acknowledgement.
func (c *Coordinator) checkWrite(tx *sql.Tx, taskID int64, a Actor, need writeAccess) error {
	if a.IsUser() {
		_, err := loadLease(tx, taskID)
		return err
	}
	s, err := loadSession(tx, a.SessionID)
	if err != nil {
		return err
	}
	if s.TaskID != taskID {
		return fmt.Errorf("session %s belongs to task #%d, not #%d", a.SessionID, s.TaskID, taskID)
	}
	l, err := loadLease(tx, taskID)
	if err != nil {
		return err
	}
	if l.State == "handoff_pending" {
		if a.SessionID == l.Pending {
			return errors.New("acknowledge the handoff first: `agent-shiftboard handoff ack` with the IDs of active rules, active delegations, open pending items, and unknown side effects")
		}
		if a.SessionID == l.Holder {
			return errors.New("the lead role is being handed to another session; this session can no longer write task state")
		}
	}
	if need == accessLead && a.SessionID != l.Holder {
		return errors.New("only the current lead can do this")
	}
	return nil
}

// sessionTask returns the task a managed session belongs to.
func (c *Coordinator) sessionTask(ctx context.Context, a Actor) (int64, error) {
	if a.IsUser() {
		return 0, errors.New("a task is required")
	}
	s, err := loadSession(c.DB, a.SessionID)
	return s.TaskID, err
}

// ResolveTask returns taskID when given, otherwise the actor's own task.
func (c *Coordinator) ResolveTask(ctx context.Context, a Actor, taskID int64) (int64, error) {
	if taskID > 0 {
		return taskID, nil
	}
	return c.sessionTask(ctx, a)
}
