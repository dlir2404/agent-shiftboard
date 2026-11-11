package board

import (
	"context"
	"os"
	"strings"
	"testing"

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
