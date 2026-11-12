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
	if strings.Join(kinds, ",") != "task,approval,rule" {
		t.Fatalf("rows: %v", kinds)
	}
	if vis := m.visible(); len(vis) != 1 || vis[0].needCount != 2 {
		t.Fatalf("list should show one session with 2 things that need the user: %+v", vis)
	}
	v := m.View()
	for _, want := range []string{"AgentShiftboard", "SESSIONS", "#1", "implement cache", "◐ 2", "NEEDS YOU", "space"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view missing %q:\n%s", want, v)
		}
	}
	// Approve through the action mapping used by the list and the panel.
	fn, done := actFn(m.snap.items[1], "a")
	if msg := runAction(open, fn, done); !strings.Contains(msg, "approved") {
		t.Fatalf("action: %v", msg)
	}
}

func TestCompletedTaskKeepsLiveLeadVisible(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_SHIFTBOARD_USER_DIR", t.TempDir())
	if err := os.MkdirAll(paths.StateDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	open := func(ctx context.Context) (*coord.Coordinator, error) { return coord.Open(root) }
	ctx := context.Background()
	c, err := open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.CreateTask(ctx, coord.User, "orientation", "")
	if err != nil {
		t.Fatal(err)
	}
	sid, err := c.StartLead(ctx, coord.User, id, coord.SessionSpec{Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	lead := coord.Actor{SessionID: sid}
	if err := c.AckHandoff(ctx, lead, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.SetSessionRuntime(ctx, sid, "%9", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseTask(ctx, lead, id); err != nil {
		t.Fatal(err)
	}
	c.Close()

	m := model{open: open, width: 38, height: 40}
	m.snap = loadSnapshot(open)
	if len(m.snap.items) == 0 || m.snap.items[0].task.ID != id || m.snap.items[0].pane != "%9" {
		t.Fatalf("completed task with live lead disappeared: %+v", m.snap.items)
	}
	if !strings.Contains(m.snap.items[0].agents, "codex former lead") {
		t.Fatalf("missing agent details: %+v", m.snap.items[0])
	}
	if !strings.Contains(m.View(), "orientation") {
		t.Fatal("completed task missing from board")
	}
	if v := m.View(); strings.Contains(v, "NO CURRENT LEAD") || strings.Contains(v, "no lead") {
		t.Fatalf("a completed session needs no lead:\n%s", v)
	}
}

func TestReopenedTaskShowsVacantLeadAndFormerSession(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_SHIFTBOARD_USER_DIR", t.TempDir())
	if err := os.MkdirAll(paths.StateDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	open := func(ctx context.Context) (*coord.Coordinator, error) { return coord.Open(root) }
	ctx := context.Background()
	c, err := open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.CreateTask(ctx, coord.User, "repo improvements", "")
	if err != nil {
		t.Fatal(err)
	}
	sid, err := c.StartLead(ctx, coord.User, id, coord.SessionSpec{Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	lead := coord.Actor{SessionID: sid}
	if err := c.AckHandoff(ctx, lead, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.SetSessionRuntime(ctx, sid, "%9", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseTask(ctx, lead, id); err != nil {
		t.Fatal(err)
	}
	if err := c.ReopenTask(ctx, coord.User, id); err != nil {
		t.Fatal(err)
	}
	c.Close()

	m := model{open: open, width: 42, height: 40}
	m.snap = loadSnapshot(open)
	if len(m.snap.items) == 0 || m.snap.items[0].pane != "" || m.snap.items[0].task.LeaseState != "vacant" {
		t.Fatalf("reopened task should not point to former lead: %+v", m.snap.items)
	}
	if len(m.snap.items[0].agentList) != 1 || m.snap.items[0].agentList[0].role != "former lead" {
		t.Fatalf("former lead mislabeled: %+v", m.snap.items[0].agentList)
	}
	view := m.View()
	for _, want := range []string{"no lead", "NO CURRENT LEAD"} {
		if !strings.Contains(view, want) {
			t.Fatalf("board missing %q:\n%s", want, view)
		}
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
	if nativePrompt("Earlier I said to review /hooks.\n\n• Working on the source code\n\n› Ask Codex to do anything") != "" {
		t.Fatal("old conversation was mistaken for a current native prompt")
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
	if got := ids(); got != "#4,#2,older" {
		t.Fatalf("default: %s", got)
	}
	m.toggled = map[int64]bool{2: true}
	m.showOlder = true
	if got := ids(); got != "#4,#2,older,#1" {
		t.Fatalf("expanded: %s", got)
	}
	m.toggled, m.showOlder, m.filter = nil, false, "ancient"
	if got := ids(); got != "#1" {
		t.Fatalf("filter: %s", got)
	}
}

func TestSessionRowsShowWorkItems(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_SHIFTBOARD_USER_DIR", t.TempDir())
	if err := os.MkdirAll(paths.StateDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	open := func(ctx context.Context) (*coord.Coordinator, error) { return coord.Open(root) }
	ctx := context.Background()
	c, _ := open(ctx)
	id, _ := c.CreateTask(ctx, coord.User, "README + goodbye", "")
	sid, _ := c.StartLead(ctx, coord.User, id, coord.SessionSpec{Provider: "claude"})
	lead := coord.Actor{SessionID: sid}
	_ = c.AckHandoff(ctx, lead, nil)
	_ = c.SyncTodos(ctx, lead, []coord.TodoEntry{{Text: "create goodbye.txt", Status: "done"}, {Text: "commit files", Status: "in_progress"}})
	aid, _ := c.CreateAssignment(ctx, lead, coord.AssignmentInput{TaskID: id, Scope: "write README"})
	_, _ = c.RegisterWorker(ctx, lead, aid, coord.SessionSpec{Provider: "codex"})
	c.Close()

	m := model{open: open, width: 38, height: 40}
	m.snap = loadSnapshot(open)
	var rows []string
	for _, it := range m.snap.items {
		rows = append(rows, it.kind+":"+it.text)
	}
	got := strings.Join(rows, " | ")
	for _, want := range []string{"task:README + goodbye", "item:commit files", "item:write README", "item:create goodbye.txt"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
	if m.snap.items[0].progress != "1/3" || !strings.Contains(m.snap.items[0].agents, "codex worker") {
		t.Fatalf("session row: %+v", m.snap.items[0])
	}
}

func TestCloseFinishedIdleWorkers(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_SHIFTBOARD_USER_DIR", t.TempDir())
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
	aid, _ := c.CreateAssignment(ctx, lead, coord.AssignmentInput{TaskID: id, Scope: "poem"})
	wid, _ := c.RegisterWorker(ctx, lead, aid, coord.SessionSpec{Provider: "codex"})
	w := coord.Actor{SessionID: wid}
	_ = c.SetSessionRuntime(ctx, wid, "%9", 0, "")
	_ = c.Observe(ctx, w, wid, "running", "", "hook")
	_ = c.SubmitAssignment(ctx, w, aid, "done", "")
	_ = c.Observe(ctx, w, wid, "idle", "", "hook")
	c.Close()

	var closed []string
	ClosePane = func(p string) error { closed = append(closed, p); return nil }
	defer func() { ClosePane = nil }()
	loadSnapshot(open) // submitted, not yet reviewed: keep
	if len(closed) != 0 {
		t.Fatalf("closed too early: %v", closed)
	}
	c, _ = open(ctx)
	_ = c.ReviewAssignment(ctx, lead, aid, true, "")
	c.Close()
	loadSnapshot(open)
	loadSnapshot(open)
	if strings.Join(closed, ",") != "%9" {
		t.Fatalf("closed: %v", closed)
	}
}

func TestViewFitsHeight(t *testing.T) {
	long := strings.Repeat("a long decision that wraps several times in a narrow list ", 3)
	var dec []string
	for i := 0; i < 20; i++ {
		dec = append(dec, long)
	}
	m := model{width: 38, height: 30, snap: snapshot{items: []item{
		{kind: "task", id: "#4", text: "README via Codex", active: true, task: coord.Task{ID: 4},
			brief: coord.Brief{Fields: coord.BriefFields{Goal: long, NextAction: long, Decisions: dec, Completed: dec}}},
		{kind: "item", id: "I-1", text: "write README", status: "done", depth: 1},
	}}}
	v := m.View()
	if n := strings.Count(v, "\n") + 1; n > 30 {
		t.Fatalf("view has %d lines for a 30-line pane", n)
	}
	if !strings.Contains(v, "AgentShiftboard") || !strings.Contains(v, "#4") {
		t.Fatalf("top of the list scrolled away:\n%s", v)
	}
}

func TestPanelViews(t *testing.T) {
	p := panel{width: 100, height: 30,
		row: item{kind: "task", id: "#4", text: "README", agents: "claude lead ○", progress: "1/2",
			agentList: []item{{kind: "agent", text: "claude lead", role: "lead", status: "idle"}},
			brief:     coord.Brief{Fields: coord.BriefFields{Goal: "write docs", NextAction: "ask the user"}}},
		kids: []item{
			{kind: "item", text: "write README", status: "done", role: "codex worker"},
			{kind: "item", text: "commit", status: "in_progress", role: "claude lead"},
			{kind: "approval", id: "P-2", text: "git push on origin main", keys: "a approve · x deny"},
		}}
	v := p.View()
	for _, want := range []string{"#4 README", "Overview", "Checklist", "Needs you", "Goal:", "write docs"} {
		if !strings.Contains(v, want) {
			t.Fatalf("overview missing %q:\n%s", want, v)
		}
	}
	m, _ := p.onKey("down") // Checklist
	p = m.(panel)
	m, _ = p.onKey("enter")
	p = m.(panel)
	if v := p.View(); !strings.Contains(v, "write README") || !strings.Contains(v, "commit") {
		t.Fatalf("checklist:\n%s", v)
	}
	m, _ = p.onKey("left")
	p = m.(panel)
	m, _ = p.onKey("down") // Agents
	p = m.(panel)
	m, _ = p.onKey("down") // Needs you
	p = m.(panel)
	if p.count("Needs you") != 1 || !strings.Contains(p.View(), "P-2") {
		t.Fatalf("needs you:\n%s", p.View())
	}
}

// An unknown effect can only be reconciled by the lead, so opening its row
// must open the lead instead of reporting nothing to open.
func TestPanelEffectRowOpensLead(t *testing.T) {
	var opened int64
	prev := OpenLead
	OpenLead = func(taskID int64, mode, provider string) error { opened = taskID; return nil }
	defer func() { OpenLead = prev }()
	p := panel{taskID: 7, width: 100, height: 30, menu: 3, inContent: true,
		kids: []item{{kind: "effect", id: "E-4", text: "outcome UNKNOWN: x on y (lead must reconcile)"}}}
	m, cmd := p.onKey("enter")
	if msg := m.(panel).msg; msg != "" || cmd == nil {
		t.Fatalf("effect row did nothing: %q", msg)
	}
	if got := cmd(); got != (actionMsg{msg: "quit"}) || opened != 7 {
		t.Fatalf("got %v, opened %d", got, opened)
	}
}

func TestAttentionPrefersWaitingAgent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_SHIFTBOARD_USER_DIR", t.TempDir())
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
	_, _ = c.AddPending(ctx, lead, id, "approval", "git push", "origin main", 0)
	aid, _ := c.CreateAssignment(ctx, lead, coord.AssignmentInput{TaskID: id, Scope: "x"})
	wid, _ := c.RegisterWorker(ctx, lead, aid, coord.SessionSpec{Provider: "codex"})
	c.Close()
	Passive = true
	defer func() { Passive = false }()

	pane, task, ok := Attention(open)
	if !ok || pane != "" || task != id {
		t.Fatalf("pending approval: %q %d %v", pane, task, ok)
	}
	c, _ = open(ctx)
	_ = c.SetSessionRuntime(ctx, wid, "%7", 0, "")
	_ = c.Observe(ctx, coord.Actor{SessionID: wid}, wid, "waiting_for_approval", "", "hook")
	c.Close()
	if pane, _, _ := Attention(open); pane != "%7" {
		t.Fatalf("waiting worker should come first, got %q", pane)
	}
}

// The list is a tmux pane: while tmux resizes it (attaching from a smaller
// terminal, for example) it can be narrower than the layout. Rendering must
// never panic, or the pane closes and the workspace loses its list.
func TestViewsNeverPanicInNarrowPanes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_SHIFTBOARD_USER_DIR", t.TempDir())
	if err := os.MkdirAll(paths.StateDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	open := func(ctx context.Context) (*coord.Coordinator, error) { return coord.Open(root) }
	ctx := context.Background()
	c, _ := open(ctx)
	id, _ := c.CreateTask(ctx, coord.User, coord.DraftTitle, "(not described yet: ask the user what they want)")
	sid, _ := c.StartLead(ctx, coord.User, id, coord.SessionSpec{Provider: "claude", NativeSessionID: "n"})
	c.Close()
	for _, status := range []string{"", "running", "waiting for approval", "stopped"} {
		if status != "" {
			c, _ := open(ctx)
			_ = c.Observe(ctx, coord.User, sid, status, "", "hook")
			c.Close()
		}
		snap := loadSnapshot(open)
		var row item
		if len(snap.items) > 0 {
			row = snap.items[0]
		}
		for w := 0; w <= 40; w++ {
			for _, h := range []int{0, 1, 3, 8, 40} {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("status %q, %dx%d: %v", status, w, h, r)
						}
					}()
					_ = model{open: open, width: w, height: h, snap: snap}.View()
					_ = panel{width: w, height: h, row: row, kids: snap.items}.View()
				}()
			}
		}
	}
}

func TestHandoffShowsOneLead(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_SHIFTBOARD_USER_DIR", t.TempDir())
	if err := os.MkdirAll(paths.StateDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	open := func(ctx context.Context) (*coord.Coordinator, error) { return coord.Open(root) }
	ctx := context.Background()
	c, err := open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.CreateTask(ctx, coord.User, "switch test", "")
	if err != nil {
		t.Fatal(err)
	}
	old, err := c.StartLead(ctx, coord.User, id, coord.SessionSpec{Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AckHandoff(ctx, coord.Actor{SessionID: old}, nil); err != nil {
		t.Fatal(err)
	}
	next, err := c.StartLead(ctx, coord.User, id, coord.SessionSpec{Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if tk, _ := c.GetTask(ctx, id); tk.PendingLead != next || tk.LeadSession != old {
		t.Fatalf("expected a pending handoff: %+v", tk)
	}
	c.Close()

	snap := loadSnapshot(open)
	roles := map[string]string{}
	for _, a := range snap.items[0].agentList {
		roles[a.provider] = a.role
	}
	if roles["claude"] != "lead" || roles["codex"] != "outgoing lead" {
		t.Fatalf("handoff roles = %v", roles)
	}
	chips := agentChips(snap.items[0])
	if n := strings.Count(chips, "[LEAD]"); n != 1 || !strings.Contains(chips, "codex [handing off]") {
		t.Fatalf("want one [LEAD] and codex handing off, got %d: %s", n, chips)
	}
}

func TestInputState(t *testing.T) {
	// Captured from live panes (tmux capture-pane -e -J).
	const (
		claudeBorder = "\x1b[38;5;244m────────"
		claudeFooter = "\n\x1b[39m  \x1b[38;5;220m⏵⏵ auto mode on\x1b[38;5;246m (shift+tab to cycle)\x1b[39m"
		codexTop     = "\x1b[49m  ⚠ 5h limit: 20% left\n\x1b[48;2;49;52;57m\n"
		codexFooter  = "\n\x1b[49m  \x1b[38;2;246;226;183mGPT-6-Astra high\x1b[38;2;140;142;144m · \x1b[38;2;171;223;167m~/code\x1b[39m"
	)
	cases := []struct {
		name   string
		screen string
		want   bool // typed
	}{
		{"claude suggestion is dim", "\x1b[38;5;246m\x1b[48;5;237m❯ \x1b[38;5;231mok\x1b[39m\nanswer\n" + claudeBorder + "\n\x1b[39m❯\u00a0\x1b[2mpush đi\x1b[0m\n" + claudeBorder + claudeFooter, false},
		{"claude empty", claudeBorder + "\n\x1b[39m❯\u00a0\x1b[0m\n" + claudeBorder + claudeFooter, false},
		{"claude typed", claudeBorder + "\n\x1b[39m❯\u00a0push now\x1b[0m\n" + claudeBorder + claudeFooter, true},
		{"claude draft, blank line, typed third line", claudeBorder + "\n\x1b[39m❯\u00a0\n\n  third line\n" + claudeBorder + claudeFooter, true},
		{"codex placeholder", codexTop + "\x1b[1m›\x1b[0m\x1b[48;2;49;52;57m \x1b[2mAsk Codex to do anything\x1b[0m\x1b[48;2;49;52;57m\n" + codexFooter, false},
		{"codex typed", codexTop + "\x1b[1m›\x1b[0m\x1b[48;2;49;52;57m fix the tests\n" + codexFooter, true},
		{"codex draft, blank line, typed third line", codexTop + "\x1b[1m›\x1b[0m\x1b[48;2;49;52;57m \n\n  third line\n" + codexFooter, true},
		{"color 2 is not dim", "\x1b[38;5;2m❯ \x1b[38;2;2;2;2mtyped", true},
		// From the A-1 review: a pasted glyph or a typed "─" line inside the draft.
		{"claude draft above a pasted glyph", claudeBorder + "\n\x1b[39m❯\u00a0\n  unfinished\n  › \n" + claudeBorder + claudeFooter, true},
		{"claude typed box-drawing line", claudeBorder + "\n\x1b[39m❯\u00a0\n  ─\n  unfinished\n" + claudeBorder + claudeFooter, true},
		{"codex draft above a pasted glyph", codexTop + "\x1b[1m›\x1b[0m\x1b[48;2;49;52;57m unfinished\n  › \n" + codexFooter, true},
		{"claude top border with a title", "\x1b[38;5;244m──────────── my session ─\n\x1b[39m❯\u00a0\x1b[2mpush đi\x1b[0m\n" + claudeBorder + claudeFooter, false},
		{"box end not on screen", "\x1b[39m❯\u00a0\x1b[0m\n\n", true},
	}
	for _, c := range cases {
		found, typed := inputState(c.screen)
		if !found || typed != c.want {
			t.Errorf("%s: found=%v typed=%v", c.name, found, typed)
		}
	}
	if found, _ := inputState("Thinking…\n  1. Yes  2. No"); found {
		t.Error("a screen without an input line must not count as found")
	}
}

func TestCursorAtEmptyInput(t *testing.T) {
	for _, c := range []struct {
		line string
		x    int
		want bool
	}{
		{"❯ push đi", 2, true}, // dim suggestion: cursor stays at the start
		{"› Ask Codex to do anything", 2, true},
		{"❯ push đi", 9, false},
		{"  ❯ ", 4, true},
		{"  third line", 12, false},
		{"", 0, false},
	} {
		if got := cursorAtEmptyInput(c.line, c.x); got != c.want {
			t.Errorf("%q at %d: got %v", c.line, c.x, got)
		}
	}
}

func TestRelayMessages(t *testing.T) {
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
	old, _ := c.StartLead(ctx, coord.User, id, coord.SessionSpec{Provider: "claude"})
	_ = c.AckHandoff(ctx, coord.Actor{SessionID: old}, nil)
	lead, _ := c.StartLead(ctx, coord.User, id, coord.SessionSpec{Provider: "codex"})
	if err := c.AckHandoff(ctx, coord.Actor{SessionID: lead}, nil); err != nil {
		t.Fatal(err)
	}
	_ = c.SetSessionRuntime(ctx, old, "%2", 0, "")
	_ = c.Observe(ctx, coord.Actor{SessionID: old}, old, "idle", "", "hook")
	m1, err := c.SendMessage(ctx, coord.Actor{SessionID: lead}, 0, "claude", "first")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.SendMessage(ctx, coord.Actor{SessionID: lead}, 0, "claude", "second")
	c.Close()

	screen := "────────\n\x1b[39m❯ half typed\x1b[0m\n────────"
	cursorLine, cursorX := "❯ ", 2
	InputView = func(string) (string, string, int, error) { return screen, cursorLine, cursorX, nil }
	var sent []string
	Wake = func(pane, text string) error { sent = append(sent, pane+": "+text); return nil }
	defer func() { Wake, InputView = nil, nil }()

	loadSnapshot(open)
	if len(sent) != 0 {
		t.Fatalf("typed over the user's draft: %v", sent)
	}
	screen = "" // capture failed
	loadSnapshot(open)
	if len(sent) != 0 {
		t.Fatalf("typed into an unreadable pane: %v", sent)
	}
	// Prompt, blank line, typed third line: the cursor left the prompt row.
	screen, cursorLine, cursorX = "\x1b[1m›\x1b[0m \n\n  third line\n\n  GPT · ~/code", "  third line", 12
	loadSnapshot(open)
	if len(sent) != 0 {
		t.Fatalf("typed into a multi-line draft: %v", sent)
	}
	cursorLine, cursorX = "❯ ", 2
	screen = "────────\n\x1b[39m❯ \x1b[2mtry this\x1b[0m\n────────"
	loadSnapshot(open)
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "%2: AgentShiftboard "+m1.ID+" from codex lead ") || !strings.Contains(sent[0], ": first (reply: agent-shiftboard message send "+lead[:8]) {
		t.Fatalf("first refresh: %v", sent)
	}
	loadSnapshot(open)
	if len(sent) != 2 || !strings.Contains(sent[1], ": second (reply:") {
		t.Fatalf("second refresh: %v", sent)
	}
	loadSnapshot(open)
	if len(sent) != 2 {
		t.Fatalf("delivered twice: %v", sent)
	}
}

func TestCodexNativeStates(t *testing.T) {
	// Captured from live Codex panes.
	rateDialog := "• Ran git status --short\n  Approaching rate limits\n  Switch to gpt-6-luna for lower credit usage?\n\n› 1. Switch to gpt-6-luna   Fast and affordable model for easier tasks.\n  2. Keep current model\n  3. Keep current model (never show again)   Hide future rate limit reminders\n\n  enter select · esc back"
	if got := nativePrompt(rateDialog); got != "asks whether to switch models (rate limits)" {
		t.Fatalf("rate dialog: %q", got)
	}
	// A numbered list the user once typed, above an idle composer, is not a dialog.
	if got := nativePrompt("› 1. fix tests\n  2. push\n• Done.\n› Ask Codex to do anything\n  GPT-6 · ~/code"); got != "" {
		t.Fatalf("idle screen read as a prompt: %q", got)
	}
	limit := "• Ran gofmt -w internal/coord/sessions.go\n  └ (no output)\n■ You’ve hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/settings/usage to purchase more credits or\ntry again at 6:49 PM.\n\n› Ask Codex to do anything\n  GPT-6-Astra high · ~/code/agent-shiftboard\n  ← for agents · ? for shortcuts"
	if ok, at := usageLimit(limit); !ok || at != "6:49 PM" {
		t.Fatalf("usage limit: %v %q", ok, at)
	}
	resumed := strings.Replace(limit, "\n\n› Ask", "\n› continue\n• Working on it\n\n› Ask", 1)
	if ok, _ := usageLimit(resumed); ok {
		t.Fatal("an old notice above newer output must not count")
	}

	// The board records the session as out of quota (source pane).
	root := t.TempDir()
	t.Setenv("AGENT_SHIFTBOARD_USER_DIR", t.TempDir())
	if err := os.MkdirAll(paths.StateDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	open := func(ctx context.Context) (*coord.Coordinator, error) { return coord.Open(root) }
	ctx := context.Background()
	c, _ := open(ctx)
	id, _ := c.CreateTask(ctx, coord.User, "t", "")
	lead, _ := c.StartLead(ctx, coord.User, id, coord.SessionSpec{Provider: "claude"})
	_ = c.AckHandoff(ctx, coord.Actor{SessionID: lead}, nil)
	aid, _ := c.CreateAssignment(ctx, coord.Actor{SessionID: lead}, coord.AssignmentInput{TaskID: id, Scope: "x"})
	wid, _ := c.RegisterWorker(ctx, coord.Actor{SessionID: lead}, aid, coord.SessionSpec{Provider: "codex"})
	_ = c.SetSessionRuntime(ctx, wid, "%6", 0, "")
	_ = c.Observe(ctx, coord.Actor{SessionID: wid}, wid, "running", "", "hook")
	c.Close()
	Capture = func(pane string, _ int) string {
		if pane == "%6" {
			return limit
		}
		return ""
	}
	defer func() { Capture = nil }()
	snap := loadSnapshot(open)
	c, _ = open(ctx)
	defer c.Close()
	ss, _ := c.ListSessions(ctx, id)
	for _, s := range ss {
		if s.ID == wid && (s.Status != "quota_unavailable" || s.Reason != "quota" || s.StatusSource != "pane") {
			t.Fatalf("worker after the notice: %+v", s)
		}
	}
	found := false
	for _, it := range snap.items {
		for _, a := range it.agentList {
			if a.id == wid && a.reason == "out of usage, try again at 6:49 PM" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("board does not show when to try again")
	}
}
