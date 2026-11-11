// Package board is the AgentShiftboard session list (the workspace's home
// window): tasks with their lead and worker sessions and what needs the
// user on the left, a live preview of the selected row on the right. It is
// a view/controller over coordinator state and holds no durable state
// (AGENTS.md).
package board

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
)

// Opener opens the coordinator for one refresh or action.
type Opener func(ctx context.Context) (*coord.Coordinator, error)

// Workspace hooks, set by the CLI when running inside tmux.
var (
	// Refresh runs on every refresh, e.g. to reconcile tmux panes.
	Refresh func(ctx context.Context, c *coord.Coordinator)
	// Focus shows the window that holds pane.
	Focus func(pane string) error
	// OpenLead shows the task's lead, or starts one (mode auto|resume|new).
	OpenLead func(taskID int64, mode, provider string) error
	// NewTask creates a draft task and opens its lead.
	NewTask func() error
	// Leave detaches the user's terminal; sessions keep running.
	Leave func()
	// Capture returns the visible tail of a pane (display only, never stored).
	Capture func(pane string, lines int) string
	// Providers lists the CLIs a lead can be switched to.
	Providers []string
)

// Run starts the board. It refuses to run inside a managed agent session,
// because the board performs user operations.
func Run(open Opener, _ int64) error {
	if !coord.ActorFromEnv().IsUser() {
		return coord.ErrUserOnly
	}
	m := model{open: open}
	if exe, err := os.Executable(); err == nil {
		if st, err := os.Stat(exe); err == nil {
			m.exe, m.exeMod = exe, st.ModTime()
		}
	}
	final, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if err != nil {
		return err
	}
	if fm, ok := final.(model); ok && fm.restart {
		// A new binary was installed: restart the board with it.
		return syscall.Exec(fm.exe, os.Args, os.Environ())
	}
	return nil
}

// item is one row: a task, a session, or something that needs the user.
type item struct {
	kind  string // task | session | approval | question | rule | delegation | review | effect | lock | stale
	id    string
	text  string
	keys  string // actions for this row
	scope string
	depth int
	// task rows
	task  coord.Task
	brief coord.Brief
	// session rows
	pane, provider, role, status, reason, observed string
	assignment                                     *coord.Assignment
	output                                         string
}

type snapshot struct {
	items   []item
	root    string
	running int
	waiting int
	err     error
	at      time.Time
}

type model struct {
	open          Opener
	snap          snapshot
	cursor        int
	msg           string
	width, height int
	input         *input
	exe           string
	exeMod        time.Time
	restart       bool
	selectedID    string // keeps the selection across refreshes
}

type input struct {
	purpose string // answer | provider
	id      string
	taskID  int64
	text    string
}

type tickMsg time.Time
type snapMsg snapshot
type actionMsg struct{ msg string }

func (m model) Init() tea.Cmd { return tea.Batch(m.load(), tick()) }

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) load() tea.Cmd {
	open := m.open
	return func() tea.Msg { return snapMsg(loadSnapshot(open)) }
}

func loadSnapshot(open Opener) snapshot {
	ctx := context.Background()
	s := snapshot{at: time.Now()}
	c, err := open(ctx)
	if err != nil {
		s.err = err
		return s
	}
	defer c.Close()
	s.root = c.Root
	if Refresh != nil {
		Refresh(ctx, c)
	}
	tasks, err := c.ListTasks(ctx)
	if err != nil {
		s.err = err
		return s
	}
	var openTasks []coord.Task
	for _, t := range tasks {
		if t.Status == "queued" || t.Status == "active" || t.Status == "blocked" {
			openTasks = append(openTasks, t)
		}
	}
	sort.SliceStable(openTasks, func(i, j int) bool { return openTasks[i].UpdatedAt > openTasks[j].UpdatedAt })
	seen := map[string]bool{}
	for _, t := range openTasks {
		b, _ := c.GetBrief(ctx, t.ID)
		s.items = append(s.items, item{kind: "task", id: fmt.Sprintf("#%d", t.ID), text: t.Title, task: t, brief: b,
			keys: "enter open lead · s switch lead CLI · n new task"})
		ss, _ := c.ListSessions(ctx, t.ID)
		as, _ := c.ListAssignments(ctx, t.ID)
		var lead []item
		var workers []item
		for _, x := range ss {
			if (x.Status == "stopped" || x.Status == "failed") && x.ID != t.LeadSession {
				continue
			}
			role := x.Role
			switch x.ID {
			case t.LeadSession:
				role = "lead"
			case t.PendingLead:
				role = "lead (new)"
			}
			it := item{kind: "session", id: x.ID, task: t, pane: x.PaneID, provider: x.Provider, role: role,
				status: x.Status, reason: x.Reason, observed: x.ObservedAt, depth: 1, keys: "enter open"}
			for i := range as {
				if as[i].WorkerSession == x.ID {
					it.assignment = &as[i]
					it.role = "worker " + as[i].ID
				}
			}
			if Capture != nil && x.PaneID != "" {
				it.output = Capture(x.PaneID, 60)
				if p := nativePrompt(it.output); p != "" {
					it.status, it.reason = "waiting_for_approval", p
					it.keys = "enter open and answer it"
				}
			}
			switch it.status {
			case "running", "retrying":
				s.running++
			case "waiting_for_approval":
				s.waiting++
			}
			if strings.HasPrefix(role, "lead") {
				lead = append(lead, it)
			} else {
				workers = append(workers, it)
			}
		}
		s.items = append(s.items, lead...)
		s.items = append(s.items, workers...)
		for _, n := range needsYou(ctx, c, t.ID) {
			if seen[n.kind+n.id] {
				continue
			}
			seen[n.kind+n.id] = true
			n.depth, n.task = 1, t
			s.items = append(s.items, n)
			s.waiting++
		}
	}
	return s
}

// needsYou lists what waits for the user, most urgent first.
func needsYou(ctx context.Context, c *coord.Coordinator, taskID int64) []item {
	var out []item
	pend, _ := c.ListPending(ctx, taskID, true)
	for _, p := range pend {
		if p.Kind == "approval" {
			out = append(out, item{kind: "approval", id: p.ID, text: fmt.Sprintf("%s on %s", p.Action, p.Target), keys: "a approve · x deny"})
		} else {
			out = append(out, item{kind: "question", id: p.ID, text: p.Action, keys: "enter answer"})
		}
	}
	effs, _ := c.ListEffects(ctx, taskID, "unknown")
	for _, e := range effs {
		out = append(out, item{kind: "effect", id: e.ID, text: fmt.Sprintf("outcome UNKNOWN: %s on %s (lead must reconcile)", e.Action, e.Target)})
	}
	rules, _ := c.ListRules(ctx, taskID, false)
	for _, r := range rules {
		if r.Status == "proposed" {
			out = append(out, item{kind: "rule", id: r.ID, text: r.Text, scope: r.Scope, keys: "c confirm · v revoke"})
		}
	}
	dels, _ := c.ListDelegations(ctx, taskID, false)
	for _, d := range dels {
		switch {
		case d.Status == "proposed":
			out = append(out, item{kind: "delegation", id: d.ID, text: fmt.Sprintf("%s (until %s)", d.Scope, short(d.ExpiresAt)), keys: "c confirm · v revoke"})
		default:
			n := 0
			for _, l := range d.Log {
				if l.NeedsReview {
					n++
				}
			}
			if n > 0 {
				out = append(out, item{kind: "review", id: d.ID, text: fmt.Sprintf("%d decision(s) taken under delegation", n), keys: "r mark reviewed"})
			}
		}
	}
	locks, _ := c.ListLocks(ctx)
	for _, l := range locks {
		if l.Status == "stale" {
			out = append(out, item{kind: "lock", id: l.Name, text: "held by a session that is gone", keys: "b break"})
		}
	}
	if t, err := c.GetTask(ctx, taskID); err == nil && t.PendingLead != "" {
		ss, _ := c.ListSessions(ctx, taskID)
		for _, s := range ss {
			if s.ID == t.PendingLead && s.Status == "starting" {
				if obs, err := time.Parse(time.RFC3339, s.ObservedAt); err == nil && time.Since(obs) > 30*time.Second {
					out = append(out, item{kind: "stale", id: "lead", text: "the new lead has not reported for " + ago(s.ObservedAt) +
						"; it may be waiting on a prompt (folder trust, login) in its window (Ctrl-b w)"})
				}
			}
		}
	}
	stale, _ := c.StaleReasons(ctx, taskID)
	for _, s := range stale {
		out = append(out, item{kind: "stale", id: "brief", text: s + " — ask the lead to update the brief"})
	}
	// Newly recorded active rules are shown so the user can correct them.
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i].kind) < rank(out[j].kind) })
	return out
}

func rank(kind string) int {
	return map[string]int{"approval": 0, "question": 1, "effect": 2, "delegation": 3, "rule": 4, "review": 5, "lock": 6, "stale": 7}[kind]
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		if m.exe != "" {
			if st, err := os.Stat(m.exe); err == nil && st.ModTime().After(m.exeMod) {
				m.restart = true
				return m, tea.Quit
			}
		}
		return m, tea.Batch(m.load(), tick())
	case snapMsg:
		m.snap = snapshot(msg)
		m.cursor = 0
		for i, it := range m.snap.items {
			if it.kind+it.id == m.selectedID {
				m.cursor = i
			}
		}
	case actionMsg:
		m.msg = msg.msg
		return m, m.load()
	case tea.KeyMsg:
		if m.input != nil {
			return m.updateInput(msg)
		}
		m.msg = ""
		switch msg.String() {
		case "ctrl+c", "q", "ctrl+q":
			if Leave != nil {
				Leave()
				return m, nil
			}
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.snap.items)-1 {
				m.cursor++
			}
		case "n":
			if NewTask == nil {
				m.msg = "new tasks need the tmux workspace"
				break
			}
			return m, func() tea.Msg {
				if err := NewTask(); err != nil {
					return actionMsg{msg: "error: " + err.Error()}
				}
				return actionMsg{msg: "new task started"}
			}
		default:
			if it, ok := m.selected(); ok {
				return m.onKey(it, msg.String())
			}
		}
		if it, ok := m.selected(); ok {
			m.selectedID = it.kind + it.id
		}
	}
	return m, nil
}

func (m model) onKey(it item, key string) (tea.Model, tea.Cmd) {
	switch {
	case key == "enter" && it.kind == "task":
		return m, openLead(it.task.ID, "auto", "")
	case key == "enter" && it.kind == "session":
		if it.pane != "" && Focus != nil {
			pane := it.pane
			return m, func() tea.Msg {
				if err := Focus(pane); err != nil {
					return actionMsg{msg: "error: " + err.Error()}
				}
				return actionMsg{msg: ""}
			}
		}
		if strings.HasPrefix(it.role, "lead") {
			return m, openLead(it.task.ID, "auto", "")
		}
		m.msg = "this session has no window"
	case key == "s" && (it.kind == "task" || strings.HasPrefix(it.role, "lead")):
		m.input = &input{purpose: "provider", taskID: it.task.ID}
	case key == "enter" && it.kind == "question":
		m.input = &input{purpose: "answer", id: it.id}
	default:
		return m, m.act(it, key)
	}
	return m, nil
}

func openLead(taskID int64, mode, provider string) tea.Cmd {
	return func() tea.Msg {
		if OpenLead == nil {
			return actionMsg{msg: "opening sessions needs the tmux workspace"}
		}
		if err := OpenLead(taskID, mode, provider); err != nil {
			return actionMsg{msg: "error: " + err.Error()}
		}
		return actionMsg{msg: ""}
	}
}

func (m model) updateInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.input = nil
	case tea.KeyEnter:
		in := *m.input
		m.input = nil
		text := strings.TrimSpace(in.text)
		if in.purpose == "provider" {
			return m, openLead(in.taskID, "new", text)
		}
		open := m.open
		return m, func() tea.Msg {
			return actionMsg{msg: runAction(open, func(ctx context.Context, c *coord.Coordinator) error {
				return c.AnswerPending(ctx, coord.User, in.id, text)
			}, in.id+" answered")}
		}
	case tea.KeyBackspace:
		if r := []rune(m.input.text); len(r) > 0 {
			m.input.text = string(r[:len(r)-1])
		}
	default:
		m.input.text += msg.String()
	}
	return m, nil
}

func (m model) selected() (item, bool) {
	if m.cursor >= 0 && m.cursor < len(m.snap.items) {
		return m.snap.items[m.cursor], true
	}
	return item{}, false
}

func (m model) act(it item, key string) tea.Cmd {
	var fn func(ctx context.Context, c *coord.Coordinator) error
	var done string
	u := coord.User
	switch {
	case it.kind == "approval" && key == "a":
		fn, done = func(ctx context.Context, c *coord.Coordinator) error { return c.ApprovePending(ctx, u, it.id, false) }, it.id+" approved"
	case it.kind == "approval" && key == "x":
		fn, done = func(ctx context.Context, c *coord.Coordinator) error { return c.ApprovePending(ctx, u, it.id, true) }, it.id+" denied"
	case it.kind == "rule" && key == "c":
		fn, done = func(ctx context.Context, c *coord.Coordinator) error { return c.ConfirmRule(ctx, u, it.id) }, it.id+" confirmed"
	case it.kind == "rule" && key == "v":
		fn, done = func(ctx context.Context, c *coord.Coordinator) error { return c.RevokeRule(ctx, u, it.id) }, it.id+" revoked"
	case it.kind == "delegation" && key == "c":
		fn, done = func(ctx context.Context, c *coord.Coordinator) error { return c.ConfirmDelegation(ctx, u, it.id) }, it.id+" confirmed"
	case it.kind == "delegation" && key == "v":
		fn, done = func(ctx context.Context, c *coord.Coordinator) error { return c.RevokeDelegation(ctx, u, it.id) }, it.id+" revoked"
	case it.kind == "review" && key == "r":
		fn, done = func(ctx context.Context, c *coord.Coordinator) error { return c.ReviewDecisions(ctx, u, it.id) }, it.id+" reviewed"
	case it.kind == "lock" && key == "b":
		fn, done = func(ctx context.Context, c *coord.Coordinator) error { return c.BreakLock(ctx, u, it.id) }, "lock "+it.id+" broken"
	default:
		return nil
	}
	open := m.open
	return func() tea.Msg { return actionMsg{msg: runAction(open, fn, done)} }
}

func runAction(open Opener, fn func(ctx context.Context, c *coord.Coordinator) error, done string) string {
	ctx := context.Background()
	c, err := open(ctx)
	if err != nil {
		return "error: " + err.Error()
	}
	defer c.Close()
	if err := fn(ctx, c); err != nil {
		return "error: " + err.Error()
	}
	return done
}

var (
	title   = lipgloss.NewStyle().Bold(true)
	dim     = lipgloss.NewStyle().Faint(true)
	warn    = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	bad     = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	good    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	accent  = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	cursor  = lipgloss.NewStyle().Background(lipgloss.Color("24")).Foreground(lipgloss.Color("255")).Bold(true)
	keyHint = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	header  = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
)

func (m model) View() string {
	s := m.snap
	if s.err != nil {
		return bad.Render("error: "+s.err.Error()) + "\n"
	}
	w, h := m.width, m.height
	if w == 0 {
		w, h = 100, 30
	}
	top := fmt.Sprintf("%s  %s  %s  %s", accent.Render("AgentShiftboard"), dim.Render(trunc(s.root, 50)),
		good.Render(fmt.Sprintf("● %d running", s.running)), warn.Render(fmt.Sprintf("◐ %d need you", s.waiting)))
	leftW := max(30, w*42/100)
	rightW := max(20, w-leftW-3)
	bodyH := max(5, h-4)

	// Left: tasks, sessions, needs-you rows.
	var left []string
	left = append(left, header.Render("SESSIONS"))
	if len(s.items) == 0 {
		left = append(left, dim.Render("No open tasks. Press n to start one."))
	}
	for i, it := range s.items {
		line := rowText(it, leftW-2)
		if i == m.cursor {
			line = cursor.Render(padRight("▶ "+stripLead(line), leftW))
		}
		left = append(left, line)
	}
	if len(left) > bodyH {
		start := min(max(0, m.cursor-bodyH/2), len(left)-bodyH)
		left = left[start : start+bodyH]
	}

	// Right: preview of the selected row.
	right := header.Render("PREVIEW") + "\n"
	if it, ok := m.selected(); ok {
		right += preview(it, rightW, bodyH-1)
	}

	l := lipgloss.NewStyle().Width(leftW).Height(bodyH).Render(strings.Join(left, "\n"))
	r := lipgloss.NewStyle().Width(rightW).Height(bodyH).MaxHeight(bodyH).Render(right)
	sep := dim.Render(strings.Repeat("│\n", bodyH))
	body := lipgloss.JoinHorizontal(lipgloss.Top, l, " ", sep, " ", r)

	foot := dim.Render("↑↓ move · enter open · n new task · s switch lead CLI · q leave")
	if it, ok := m.selected(); ok && it.keys != "" {
		foot = keyHint.Render(it.keys) + dim.Render("   ↑↓ move · n new task · q leave")
	}
	if m.input != nil {
		prompt := "Answer " + m.input.id
		if m.input.purpose == "provider" {
			prompt = "Switch the lead of task #" + fmt.Sprint(m.input.taskID) + " to (" + strings.Join(Providers, ", ") + ")"
		}
		foot = accent.Render(prompt+": ") + m.input.text + "█" + dim.Render("  enter ok · esc cancel")
	} else if m.msg != "" {
		foot = m.msg
	}
	return top + "\n\n" + body + "\n" + foot
}

func rowText(it item, w int) string {
	indent := strings.Repeat("  ", it.depth)
	switch it.kind {
	case "task":
		t := it.task
		return trunc(fmt.Sprintf("%s %s %s", title.Render(it.id), it.text, dim.Render("· "+t.Status)), w+30)
	case "session":
		state := it.status
		if it.reason != "" && it.status == "waiting_for_approval" {
			state = warn.Render(it.reason)
		} else {
			state = dim.Render(state)
		}
		return indent + trunc(fmt.Sprintf("%s %-7s %s %s", statusIcon(it.status), it.provider, it.role, state), w+30)
	default:
		return indent + warn.Render("! ") + trunc(fmt.Sprintf("%s %s %s", it.kind, it.id, it.text), w-len(indent)-2)
	}
}

func stripLead(s string) string { return strings.TrimLeft(s, " ") }

func padRight(s string, w int) string {
	if n := lipgloss.Width(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

func statusIcon(st string) string {
	switch st {
	case "running", "retrying":
		return good.Render("●")
	case "idle":
		return dim.Render("○")
	case "waiting_for_approval", "cooling_down", "starting":
		return warn.Render("◐")
	case "failed", "quota_unavailable", "authentication_required", "unknown":
		return bad.Render("✕")
	}
	return dim.Render("·")
}

func preview(it item, w, h int) string {
	var b strings.Builder
	wrap := lipgloss.NewStyle().Width(w)
	switch it.kind {
	case "task":
		t, f := it.task, it.brief.Fields
		fmt.Fprintf(&b, "%s %s\n", title.Render(it.id), it.text)
		fmt.Fprintf(&b, "%s · lead %s · brief r%d\n\n", t.Status, leaseStyle(t.LeaseState), t.BriefRevision)
		if f.Goal != "" {
			b.WriteString(wrap.Render(title.Render("Goal: ")+f.Goal) + "\n")
		}
		if f.NextAction != "" {
			b.WriteString(wrap.Render(title.Render("Next: ")+f.NextAction) + "\n")
		}
		for _, x := range f.Blockers {
			b.WriteString(wrap.Render(warn.Render("Blocked: ")+x) + "\n")
		}
		if len(f.Completed) > 0 {
			b.WriteString("\n" + title.Render("Done") + "\n")
			for _, x := range lastN(f.Completed, 5) {
				b.WriteString(wrap.Render("• "+x) + "\n")
			}
		}
		if len(f.Decisions) > 0 {
			b.WriteString("\n" + title.Render("Decisions") + "\n")
			for _, x := range lastN(f.Decisions, 5) {
				b.WriteString(wrap.Render("• "+x) + "\n")
			}
		}
	case "session":
		fmt.Fprintf(&b, "%s %s · task %s\n", title.Render(it.provider), it.role, fmt.Sprintf("#%d", it.task.ID))
		fmt.Fprintf(&b, "Status: %s %s %s\n", statusIcon(it.status), it.status, dim.Render(it.reason+" "+ago(it.observed)))
		if a := it.assignment; a != nil {
			b.WriteString(wrap.Render(title.Render("Assignment: ")+a.Scope) + "\n")
			if a.Result != "" {
				b.WriteString(wrap.Render(title.Render("Result: ")+a.Result) + "\n")
			}
		}
		b.WriteString("\n" + header.Render("Output") + "\n")
		out := strings.TrimRight(it.output, "\n ")
		lines := strings.Split(out, "\n")
		room := max(3, h-strings.Count(b.String(), "\n")-1)
		if len(lines) > room {
			lines = lines[len(lines)-room:]
		}
		for _, l := range lines {
			b.WriteString(trunc(l, w) + "\n")
		}
	default:
		fmt.Fprintf(&b, "%s %s\n\n", title.Render(it.kind), it.id)
		b.WriteString(wrap.Render(it.text) + "\n")
		if it.keys != "" {
			b.WriteString("\n" + keyHint.Render(it.keys) + "\n")
		}
	}
	return b.String()
}

func lastN(xs []string, n int) []string {
	if len(xs) > n {
		return xs[len(xs)-n:]
	}
	return xs
}

func statusStyle(s string) string {
	switch s {
	case "running", "idle":
		return good.Render(s)
	case "waiting_for_approval", "retrying", "cooling_down", "starting":
		return warn.Render(s)
	case "failed", "quota_unavailable", "authentication_required", "unknown":
		return bad.Render(s)
	}
	return s
}

func leaseStyle(s string) string {
	switch s {
	case "held":
		return good.Render(s)
	case "suspended", "handoff_pending":
		return warn.Render(s)
	}
	return s
}

func ago(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

func short(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.Local().Format("Jan 2 15:04")
	}
	return ts
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// nativePrompt recognizes a provider CLI's own blocking prompt in the
// visible pane (display only). AgentShiftboard never answers it; it only
// tells the user that the session waits for them.
func nativePrompt(screen string) string {
	low := strings.ToLower(screen)
	switch {
	case strings.Contains(low, "trust this folder") || strings.Contains(low, "trust the files in this folder") || strings.Contains(low, "is this a project you created or one you trust"):
		return "asks to trust the folder"
	case strings.Contains(low, "/hooks") && strings.Contains(low, "review"):
		return "asks to review hooks"
	case strings.Contains(low, "please log in") || strings.Contains(low, "sign in"):
		return "asks to log in"
	}
	return ""
}
