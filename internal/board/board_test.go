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

	m := model{open: open, width: 80}
	m.snap = loadSnapshot(open, 0)
	v := m.View()
	for _, want := range []string{"Task #1", "approval", "P-1", "rule", "R-1", "a approve"} {
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
