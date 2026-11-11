package adapter

import (
	"encoding/json"
	"regexp"
	"strings"
)

// TodoEntry is one entry of an agent's own todo/plan list, as seen in a
// tool call (hook payload), never read from transcripts.
type TodoEntry struct {
	ExtID  string
	Text   string
	Status string // pending | in_progress | done | cancelled
}

func todoStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "completed", "complete", "done":
		return "done"
	case "in_progress", "in-progress", "active":
		return "in_progress"
	case "deleted", "cancelled", "canceled":
		return "cancelled"
	case "":
		return ""
	}
	return "pending"
}

// TodoCall extracts todo updates from a tool call:
//   - full: the whole list (Claude TodoWrite, Codex update_plan)
//   - one:  a single created or updated entry (Claude TaskCreate/TaskUpdate)
func TodoCall(h HookInput) (full []TodoEntry, isFull bool, one *TodoEntry) {
	switch h.ToolName {
	case "TodoWrite":
		var in struct {
			Todos []struct {
				ID      string `json:"id"`
				Content string `json:"content"`
				Status  string `json:"status"`
			} `json:"todos"`
		}
		if json.Unmarshal(h.ToolInput, &in) != nil {
			return nil, false, nil
		}
		for _, t := range in.Todos {
			full = append(full, TodoEntry{ExtID: t.ID, Text: t.Content, Status: todoStatus(t.Status)})
		}
		return full, true, nil
	case "update_plan":
		var in struct {
			Plan []struct {
				Step   string `json:"step"`
				Status string `json:"status"`
			} `json:"plan"`
		}
		if json.Unmarshal(h.ToolInput, &in) != nil {
			return nil, false, nil
		}
		for _, p := range in.Plan {
			full = append(full, TodoEntry{Text: p.Step, Status: todoStatus(p.Status)})
		}
		return full, true, nil
	case "TaskCreate":
		var in struct {
			Subject string `json:"subject"`
		}
		_ = json.Unmarshal(h.ToolInput, &in)
		id := responseID(h.ToolResponse)
		if id == "" || in.Subject == "" {
			return nil, false, nil
		}
		return nil, false, &TodoEntry{ExtID: id, Text: in.Subject, Status: "pending"}
	case "TaskUpdate":
		var in struct {
			TaskID  json.RawMessage `json:"taskId"`
			Status  string          `json:"status"`
			Subject string          `json:"subject"`
		}
		_ = json.Unmarshal(h.ToolInput, &in)
		id := strings.Trim(string(in.TaskID), `" `)
		if id == "" {
			return nil, false, nil
		}
		return nil, false, &TodoEntry{ExtID: id, Text: in.Subject, Status: todoStatus(in.Status)}
	}
	return nil, false, nil
}

var idRe = regexp.MustCompile(`(?i)(?:task\s*#|"id"\s*:\s*"?|"taskId"\s*:\s*"?)(\d+)`)

// responseID finds the created entry's ID in a tool response.
func responseID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if m := idRe.FindStringSubmatch(string(raw)); m != nil {
		return m[1]
	}
	return ""
}
