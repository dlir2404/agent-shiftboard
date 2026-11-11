package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Lock is a named advisory lock on an external resource (D-20).
type Lock struct {
	Name       string `json:"name"`
	Holder     string `json:"holder"`
	TaskID     int64  `json:"task_id,omitempty"`
	Status     string `json:"status"`
	AcquiredAt string `json:"acquired_at"`
}

// AcquireLock takes a named lock for the actor. A stale lock still blocks:
// only the user may break it.
func (c *Coordinator) AcquireLock(ctx context.Context, a Actor, name string) error {
	if err := checkName("lock", name); err != nil {
		return err
	}
	var taskArg any
	if !a.IsUser() {
		tid, err := c.sessionTask(ctx, a)
		if err != nil {
			return err
		}
		taskArg = tid
	}
	return c.tx(ctx, func(tx *sql.Tx) error {
		var holder, status string
		err := tx.QueryRow(`SELECT holder_session,status FROM locks WHERE name=?`, name).Scan(&holder, &status)
		switch {
		case errors.Is(err, sql.ErrNoRows), err == nil && status == "released":
		case err != nil:
			return err
		case holder == a.String():
			return nil // already held
		case status == "stale":
			return fmt.Errorf("lock %s is held by %s, which is no longer running (stale); ask the user to run `agent-shiftboard lock break %s`", name, holder, name)
		default:
			return fmt.Errorf("lock %s is held by %s", name, holder)
		}
		now := c.now()
		_, err = tx.Exec(`INSERT INTO locks(name,holder_session,task_id,status,acquired_at,updated_at) VALUES(?,?,?,?,?,?)
		 ON CONFLICT(name) DO UPDATE SET holder_session=excluded.holder_session,task_id=excluded.task_id,status='held',acquired_at=excluded.acquired_at,updated_at=excluded.updated_at`,
			name, a.String(), taskArg, "held", now, now)
		return err
	})
}

// ReleaseLock releases a lock the actor holds.
func (c *Coordinator) ReleaseLock(ctx context.Context, a Actor, name string) error {
	res, err := c.DB.ExecContext(ctx, `UPDATE locks SET status='released',updated_at=? WHERE name=? AND holder_session=? AND status IN ('held','stale')`,
		c.now(), name, a.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("you do not hold lock %s", name)
	}
	return nil
}

// BreakLock releases any holder's lock (user operation).
func (c *Coordinator) BreakLock(ctx context.Context, a Actor, name string) error {
	if err := requireUser(a); err != nil {
		return err
	}
	res, err := c.DB.ExecContext(ctx, `UPDATE locks SET status='released',updated_at=? WHERE name=? AND status IN ('held','stale')`, c.now(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("lock %s is not held", name)
	}
	c.logEvent(ctx, 0, a, "lock_broken", name)
	return nil
}

// ListLocks lists held and stale locks (read operation).
func (c *Coordinator) ListLocks(ctx context.Context) ([]Lock, error) {
	rows, err := c.DB.QueryContext(ctx, `SELECT name,holder_session,COALESCE(task_id,0),status,acquired_at FROM locks WHERE status!='released' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Lock
	for rows.Next() {
		var l Lock
		if err := rows.Scan(&l.Name, &l.Holder, &l.TaskID, &l.Status, &l.AcquiredAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
