package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dlir2404/agent-shiftboard/internal/secretscan"
)

// BriefFields are the lead-owned Tier 1 fields (docs/STATE-CONTRACT.md).
type BriefFields struct {
	Goal         string   `json:"goal"`
	DoneWhen     []string `json:"done_when,omitempty"`
	Decisions    []string `json:"decisions,omitempty"`
	Completed    []string `json:"completed,omitempty"`
	InProgress   string   `json:"in_progress,omitempty"`
	Blockers     []string `json:"blockers,omitempty"`
	NextAction   string   `json:"next_action,omitempty"`
	RelevantRefs []string `json:"relevant_refs,omitempty"`
	Verification []string `json:"verification,omitempty"`
}

// Brief is one revision of the lead-owned fields.
type Brief struct {
	TaskID    int64       `json:"task_id"`
	Revision  int64       `json:"revision"`
	Fields    BriefFields `json:"fields"`
	UpdatedBy string      `json:"updated_by"`
	UpdatedAt string      `json:"updated_at"`
}

// BriefPatch changes some fields. Set replaces a field; Append adds items to
// a list field. Field names are the JSON names of BriefFields.
type BriefPatch struct {
	Set    map[string]json.RawMessage `json:"set,omitempty"`
	Append map[string][]string        `json:"append,omitempty"`
}

var listFields = map[string]bool{
	"done_when": true, "decisions": true, "completed": true, "blockers": true,
	"relevant_refs": true, "verification": true,
}
var stringFields = map[string]bool{"goal": true, "in_progress": true, "next_action": true}

// GetBrief returns the current brief revision (read operation).
func (c *Coordinator) GetBrief(ctx context.Context, taskID int64) (Brief, error) {
	return getBrief(c.DB, taskID)
}

func getBrief(q interface {
	QueryRow(string, ...any) *sql.Row
}, taskID int64) (Brief, error) {
	var b Brief
	var raw string
	err := q.QueryRow(`SELECT b.task_id,b.revision,b.fields,b.updated_by,b.updated_at
	 FROM briefs b JOIN tasks t ON t.id=b.task_id AND t.brief_revision=b.revision WHERE b.task_id=?`, taskID).
		Scan(&b.TaskID, &b.Revision, &raw, &b.UpdatedBy, &b.UpdatedAt)
	if err == sql.ErrNoRows {
		return b, fmt.Errorf("task #%d not found", taskID)
	}
	if err != nil {
		return b, err
	}
	return b, json.Unmarshal([]byte(raw), &b.Fields)
}

// UpdateBrief applies a patch on top of baseRevision. It fails when the brief
// has moved past baseRevision (optimistic concurrency). Only the lead or the
// user may update the brief.
func (c *Coordinator) UpdateBrief(ctx context.Context, a Actor, taskID, baseRevision int64, p BriefPatch) (int64, error) {
	if len(p.Set) == 0 && len(p.Append) == 0 {
		return 0, fmt.Errorf("empty brief update")
	}
	var newRev int64
	err := c.tx(ctx, func(tx *sql.Tx) error {
		if err := c.checkWrite(tx, taskID, a, accessLead); err != nil {
			return err
		}
		cur, err := getBrief(tx, taskID)
		if err != nil {
			return err
		}
		if baseRevision != cur.Revision {
			return fmt.Errorf("brief is at revision %d, not %d; re-read it (`agent-shiftboard brief show`) and apply your change on top", cur.Revision, baseRevision)
		}
		f := cur.Fields
		if err := applyPatch(&f, p); err != nil {
			return err
		}
		raw, _ := json.Marshal(f)
		if err := secretscan.Check("brief", string(raw)); err != nil {
			return err
		}
		newRev = cur.Revision + 1
		now := c.now()
		if _, err := tx.Exec(`INSERT INTO briefs(task_id,revision,fields,updated_by,updated_at) VALUES(?,?,?,?,?)`,
			taskID, newRev, string(raw), a.String(), now); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE tasks SET brief_revision=?,updated_at=? WHERE id=?`, newRev, now, taskID); err != nil {
			return err
		}
		if !a.IsUser() {
			if _, err := tx.Exec(`UPDATE sessions SET turns_since_brief=0 WHERE id=?`, a.SessionID); err != nil {
				return err
			}
		}
		return c.event(tx, taskID, a, "brief_updated", fmt.Sprintf("r%d", newRev))
	})
	if err == nil {
		c.changed(taskID)
	}
	return newRev, err
}

func applyPatch(f *BriefFields, p BriefPatch) error {
	m := map[string]any{}
	raw, _ := json.Marshal(f)
	_ = json.Unmarshal(raw, &m)
	for k, v := range p.Set {
		switch {
		case stringFields[k]:
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return fmt.Errorf("%s must be a string", k)
			}
			m[k] = strings.TrimSpace(s)
		case listFields[k]:
			var l []string
			if err := json.Unmarshal(v, &l); err != nil {
				return fmt.Errorf("%s must be a list of strings", k)
			}
			m[k] = l
		default:
			return fmt.Errorf("unknown or coordinator-owned brief field %q", k)
		}
	}
	for k, items := range p.Append {
		if !listFields[k] {
			return fmt.Errorf("cannot append to %q (list fields: done_when, decisions, completed, blockers, relevant_refs, verification)", k)
		}
		var cur []any
		if existing, ok := m[k].([]any); ok {
			cur = existing
		}
		for _, it := range items {
			if s := strings.TrimSpace(it); s != "" {
				cur = append(cur, s)
			}
		}
		m[k] = cur
	}
	raw, _ = json.Marshal(m)
	var out BriefFields
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	if strings.TrimSpace(out.Goal) == "" {
		return fmt.Errorf("goal cannot be empty")
	}
	*f = out
	return nil
}
