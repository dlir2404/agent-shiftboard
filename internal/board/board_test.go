package board

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/paths"
)

func TestViewShowsWhatNeedsTheUser(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_SHIFTBOARD_USER_DIR", t.TempDir())
	if err := os.MkdirAll(paths.StateDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	open := func(ctx context.Context) (*coord.Coordinator, error) { return coord.Open(root) }
	ctx := context.Background()
	c, _ := open(ctx)
	id, _ := c.CreateTask(ctx, coord.User, "implement cache", "")
	sid, _ := c.StartLead(ctx, coord.User, id, coord.SessionSpec{Provider: "claude"})
	lead := coord.Actor{SessionID: sid}
	if err := c.AckHandoff(ctx, lead, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddPending(ctx, lead, id, "approval", "git push", "origin main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddRule(ctx, lead, coord.RuleInput{Scope: "project", Text: "small MRs", Source: "lead", Inferred: true}); err != nil {
		t.Fatal(err)
	}
	c.Close()

	m := model{open: open, width: 38, height: 40}
	m.snap = loadSnapshot(open)
	kinds := []string{}
	for _, it := range m.snap.items {
		kinds = append(kinds, it.kind)
	}
	if strings.Join(kinds, ",") != "task,session,approval,rule" {
		t.Fatalf("rows: %v", kinds)
	}
	m.cursor = 2 // the approval
	v := m.View()
	for _, want := range []string{"AgentShiftboard", "#1", "claude", "lead", "▶", "approval", "P-1", "rule", "R-1", "a approve"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view missing %q:\n%s", want, v)
		}
	}
	// Approve through the board's action path.
	it, _ := m.selected()
	msg := m.act(it, "a")()
	if !strings.Contains(msg.(actionMsg).msg, "approved") {
		t.Fatalf("action: %v", msg)
	}
}

func TestNativePrompt(t *testing.T) {
	if nativePrompt("Folder access\nTrust this folder? Codex can read...") == "" {
		t.Fatal("codex trust prompt not detected")
	}
	if nativePrompt("Quick safety check: Is this a project you created or one you trust?") == "" {
		t.Fatal("claude trust prompt not detected")
	}
	if nativePrompt("● wrote README.md") != "" {
		t.Fatal("false positive")
	}
}

func TestWakeLeadOncePerResult(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_SHIFTBOARD_USER_DIR", t.TempDir())
	t.Setenv("TMUX_PANE", "%0")
	if err := os.MkdirAll(paths.StateDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	open := func(ctx context.Context) (*coord.Coordinator, error) { return coord.Open(root) }
	ctx := context.Background()
	c, _ := open(ctx)
	id, _ := c.CreateTask(ctx, coord.User, "t", "")
	sid, _ := c.StartLead(ctx, coord.User, id, coord.SessionSpec{Provider: "claude"})
	lead := coord.Actor{SessionID: sid}
	_ = c.AckHandoff(ctx, lead, nil)
	_ = c.SetSessionRuntime(ctx, sid, "%5", 0, "")
	_ = c.Observe(ctx, lead, sid, "idle", "", "hook")
	aid, _ := c.CreateAssignment(ctx, lead, coord.AssignmentInput{TaskID: id, Scope: "write README"})
	wid, _ := c.RegisterWorker(ctx, lead, aid, coord.SessionSpec{Provider: "codex"})
	if err := c.SubmitAssignment(ctx, coord.Actor{SessionID: wid}, aid, "done", ""); err != nil {
		t.Fatal(err)
	}
	c.Close()

	var sent []string
	Wake = func(pane, text string) error { sent = append(sent, pane+": "+text); return nil }
	defer func() { Wake = nil }()
	loadSnapshot(open)
	loadSnapshot(open)
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "%5: AgentShiftboard: codex worker finished A-1") {
		t.Fatalf("wakes: %v", sent)
	}
}

func TestVisibleFoldsAndFilters(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-100 * time.Hour).Format(time.RFC3339)
	m := model{snap: snapshot{items: []item{
		{kind: "task", id: "#4", text: "README via Codex", active: true, task: coord.Task{ID: 4}},
		{kind: "session", id: "s1", provider: "claude", depth: 1},
		{kind: "session", id: "s2", provider: "codex", depth: 1},
		{kind: "task", id: "#2", text: "quiet task", task: coord.Task{ID: 2}},
		{kind: "session", id: "s3", provider: "claude", depth: 1},
		{kind: "task", id: "#1", text: "ancient task", old: true, task: coord.Task{ID: 1, UpdatedAt: old}},
		{kind: "session", id: "s4", provider: "claude", depth: 1},
	}}}
	ids := func() string {
		var out []string
		for _, it := range m.visible() {
			out = append(out, it.id)
		}
		return strings.Join(out, ",")
	}
	if got := ids(); got != "#4,s1,s2,#2,older" {
		t.Fatalf("default: %s", got)
	}
	m.toggled = map[int64]bool{2: true}
	m.showOlder = true
	if got := ids(); got != "#4,s1,s2,#2,s3,older,#1" {
		t.Fatalf("expanded: %s", got)
	}
	m.toggled, m.showOlder, m.filter = nil, false, "ancient"
	if got := ids(); got != "#1,s4" {
		t.Fatalf("filter: %s", got)
	}
}
