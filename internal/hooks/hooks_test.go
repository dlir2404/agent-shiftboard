package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/paths"
)

func setup(t *testing.T) (*coord.Coordinator, coord.Actor, int64) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AGENT_SHIFTBOARD_USER_DIR", t.TempDir())
	if err := os.MkdirAll(paths.StateDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	coord.GitHead = func(string) string { return "" }
	c, err := coord.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := context.Background()
	id, _ := c.CreateTask(ctx, coord.User, "t", "")
	sid, _ := c.StartLead(ctx, coord.User, id, coord.SessionSpec{Provider: "claude", NativeSessionID: "n-1"})
	lead := coord.Actor{SessionID: sid}
	if err := c.AckHandoff(ctx, lead, nil); err != nil {
		t.Fatal(err)
	}
	return c, lead, id
}

func call(t *testing.T, c *coord.Coordinator, a coord.Actor, payload map[string]any) Result {
	t.Helper()
	b, _ := json.Marshal(payload)
	var errb bytes.Buffer
	r := HandleClaude(context.Background(), c, a, payload["hook_event_name"].(string), bytes.NewReader(b), &errb, time.Now())
	if errb.Len() > 0 {
		t.Logf("stderr: %s", errb.String())
	}
	return r
}

func status(t *testing.T, c *coord.Coordinator, id int64) (string, string) {
	ss, err := c.ListSessions(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ss[0].Status, ss[0].Reason
}

func TestStatusFromHooks(t *testing.T) {
	c, lead, id := setup(t)
	call(t, c, lead, map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "n-1", "prompt": "go"})
	if s, _ := status(t, c, id); s != "running" {
		t.Fatalf("got %s", s)
	}
	call(t, c, lead, map[string]any{"hook_event_name": "Notification", "notification_type": "permission_prompt"})
	if s, _ := status(t, c, id); s != "waiting_for_approval" {
		t.Fatalf("got %s", s)
	}
	call(t, c, lead, map[string]any{"hook_event_name": "StopFailure", "error": "rate_limit", "last_assistant_message": "You've hit your session limit · resets 3:45pm"})
	if s, r := status(t, c, id); s != "cooling_down" || r != "quota" {
		t.Fatalf("got %s %s", s, r)
	}
	call(t, c, lead, map[string]any{"hook_event_name": "StopFailure", "error": "invalid_request"})
	if s, _ := status(t, c, id); s != "unknown" {
		t.Fatalf("ambiguous error must stay unknown, got %s", s)
	}
}

func TestPreToolUseRequiresIntent(t *testing.T) {
	c, lead, id := setup(t)
	push := map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "cd repo && GIT_TRACE=0 git push origin fix/a"}}
	r := call(t, c, lead, push)
	if !strings.Contains(string(r.Stdout), `"permissionDecision":"deny"`) {
		t.Fatalf("expected deny, got %s", r.Stdout)
	}
	ctx := context.Background()
	eid, err := c.PlanEffect(ctx, lead, id, "git push", "origin fix/a", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartEffect(ctx, lead, eid); err != nil {
		t.Fatal(err)
	}
	if r := call(t, c, lead, push); len(r.Stdout) != 0 {
		t.Fatalf("expected allow after effect start, got %s", r.Stdout)
	}
	ls := map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "git status && agent-shiftboard effect list"}}
	if r := call(t, c, lead, ls); len(r.Stdout) != 0 {
		t.Fatalf("read-only command denied: %s", r.Stdout)
	}
}

func TestStaleNudgeAndCompactionContext(t *testing.T) {
	c, lead, _ := setup(t)
	c.Policy.StaleTurns = 2
	var last Result
	for i := 0; i < 2; i++ {
		last = call(t, c, lead, map[string]any{"hook_event_name": "UserPromptSubmit"})
	}
	if !strings.Contains(string(last.Stdout), "brief may be stale") {
		t.Fatalf("no nudge: %s", last.Stdout)
	}
	r := call(t, c, lead, map[string]any{"hook_event_name": "SessionStart", "source": "compact", "session_id": "n-1"})
	if !strings.Contains(string(r.Stdout), "additionalContext") || !strings.Contains(string(r.Stdout), "Task #1") {
		t.Fatalf("no brief after compaction: %s", r.Stdout)
	}
}

func TestUnmanagedProcessIsIgnored(t *testing.T) {
	c, _, _ := setup(t)
	r := call(t, c, coord.User, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "git push"}})
	if len(r.Stdout) != 0 {
		t.Fatalf("unmanaged sessions must not be blocked: %s", r.Stdout)
	}
}
