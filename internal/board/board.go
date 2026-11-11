// Package board is the Bubble Tea status board shown beside the lead's
// native CLI pane. It is a view/controller over coordinator state; it holds
// no durable state of its own (AGENTS.md).
package board

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
)

// Opener opens the coordinator for one refresh or action.
type Opener func(ctx context.Context) (*coord.Coordinator, error)

// Refresh runs on every board refresh, e.g. to reconcile tmux panes.
var Refresh func(ctx context.Context, c *coord.Coordinator)

// Run starts the board. It refuses to run inside a managed agent session,
// because the board performs user operations.
func Run(open Opener, taskID int64) error {
	if !coord.ActorFromEnv().IsUser() {
		return coord.ErrUserOnly
	}
	m := model{open: open, taskID: taskID}
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

// item is one thing on the board that needs the user.
type item struct {
	kind  string // approval | question | rule | delegation | review | effect | lock | stale
	id    string
	text  string
	keys  string // available actions
	scope string
}

type snapshot struct {
	tasks    []coord.Task
	task     coord.Task
	brief    coord.Brief
	sessions []coord.Session
	items    []item
	err      error
	at       time.Time
}

type model struct {
	open   Opener
	taskID int64
	snap   snapshot
	cursor int
	msg    string
	width  int
	height int
	input  *answerInput
}

type answerInput struct {
	id   string
	text string
}

type tickMsg time.Time
type snapMsg snapshot
type actionMsg struct{ msg string }

func (m model) Init() tea.Cmd { return tea.Batch(m.load(), tick()) }

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) load() tea.Cmd {
	open, taskID := m.open, m.taskID
	return func() tea.Msg { return snapMsg(loadSnapshot(open, taskID)) }
}

func loadSnapshot(open Opener, taskID int64) snapshot {
	ctx := context.Background()
	s := snapshot{at: time.Now()}
	c, err := open(ctx)
	if err != nil {
		s.err = err
		return s
	}
	defer c.Close()
	if Refresh != nil {
		Refresh(ctx, c)
	}
	if s.tasks, err = c.ListTasks(ctx); err != nil {
		s.err = err
		return s
	}
	if taskID == 0 {
		for _, t := range s.tasks {
			if t.Status == "active" || t.Status == "blocked" || t.Status == "queued" {
				taskID = t.ID
				break
			}
		}
	}
	if taskID == 0 {
		return s
	}
	if s.task, err = c.GetTask(ctx, taskID); err != nil {
		s.err = err
		return s
	}
	s.brief, _ = c.GetBrief(ctx, taskID)
	s.sessions, _ = c.ListSessions(ctx, taskID)
	s.items = needsYou(ctx, c, taskID)
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
		return m, tea.Batch(m.load(), tick())
	case snapMsg:
		m.snap = snapshot(msg)
		if m.taskID == 0 {
			m.taskID = m.snap.task.ID
		}
		if m.cursor >= len(m.snap.items) {
			m.cursor = max(0, len(m.snap.items)-1)
		}
	case actionMsg:
		m.msg = msg.msg
		return m, m.load()
	case tea.KeyMsg:
		if m.input != nil {
			return m.updateInput(msg)
		}
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q":
			if os.Getenv("TMUX_PANE") == "" {
				return m, tea.Quit
			}
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.snap.items)-1 {
				m.cursor++
			}
		case "t":
			m.taskID = nextTask(m.snap.tasks, m.taskID)
			m.cursor = 0
			return m, m.load()
		default:
			if it, ok := m.selected(); ok {
				if it.kind == "question" && msg.String() == "enter" {
					m.input = &answerInput{id: it.id}
					return m, nil
				}
				return m, m.act(it, msg.String())
			}
		}
	}
	return m, nil
}

func (m model) updateInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.input = nil
	case tea.KeyEnter:
		id, text := m.input.id, m.input.text
		m.input = nil
		open := m.open
		return m, func() tea.Msg {
			return actionMsg{msg: runAction(open, func(ctx context.Context, c *coord.Coordinator) error {
				return c.AnswerPending(ctx, coord.User, id, text)
			}, id+" answered")}
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
	if m.cursor < len(m.snap.items) {
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

func nextTask(ts []coord.Task, cur int64) int64 {
	var open []int64
	for _, t := range ts {
		if t.Status != "done" && t.Status != "cancelled" {
			open = append(open, t.ID)
		}
	}
	if len(open) == 0 {
		return cur
	}
	sort.Slice(open, func(i, j int) bool { return open[i] < open[j] })
	for _, id := range open {
		if id > cur {
			return id
		}
	}
	return open[0]
}

var (
	title  = lipgloss.NewStyle().Bold(true)
	dim    = lipgloss.NewStyle().Faint(true)
	warn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	bad    = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	good   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	cursor = lipgloss.NewStyle().Reverse(true)
)

func (m model) View() string {
	s := m.snap
	var b strings.Builder
	if s.err != nil {
		return bad.Render("error: "+s.err.Error()) + "\n\nq quit\n"
	}
	if s.task.ID == 0 {
		return "No open tasks. Create one with `agent-shiftboard run \"<task>\"`.\n\nq quit\n"
	}
	fmt.Fprintf(&b, "%s\n", title.Render(fmt.Sprintf("Task #%d %s", s.task.ID, trunc(s.task.Title, max(10, m.width-10)))))
	fmt.Fprintf(&b, "%s · lease %s · brief r%d\n\n", s.task.Status, leaseStyle(s.task.LeaseState), s.task.BriefRevision)

	for _, ss := range s.sessions {
		if ss.Status == "stopped" && ss.ID != s.task.LeadSession {
			continue
		}
		role := ss.Role
		switch ss.ID {
		case s.task.LeadSession:
			role = "lead"
		case s.task.PendingLead:
			role = "lead (pending ack)"
		}
		line := fmt.Sprintf("%-6s %-18s %s", ss.Provider, role, statusStyle(ss.Status))
		if ss.Reason != "" {
			line += " " + dim.Render(ss.Reason)
		}
		line += dim.Render(" " + ago(ss.ObservedAt) + " via " + ss.StatusSource)
		fmt.Fprintln(&b, line)
	}
	if len(s.sessions) == 0 {
		fmt.Fprintln(&b, dim.Render("no sessions yet"))
	}
	fmt.Fprintln(&b)

	if na := s.brief.Fields.NextAction; na != "" {
		fmt.Fprintf(&b, "%s %s\n", title.Render("Next:"), trunc(na, max(20, m.width-7)))
	}
	if bl := s.brief.Fields.Blockers; len(bl) > 0 {
		fmt.Fprintf(&b, "%s %s\n", warn.Render("Blocked:"), trunc(strings.Join(bl, "; "), max(20, m.width-10)))
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, title.Render(fmt.Sprintf("Needs you (%d)", len(s.items))))
	for i, it := range s.items {
		line := fmt.Sprintf("%-10s %-6s %s", it.kind, it.id, trunc(it.text, max(20, m.width-20)))
		if i == m.cursor {
			line = cursor.Render(line)
		}
		fmt.Fprintln(&b, line)
	}
	if len(s.items) == 0 {
		fmt.Fprintln(&b, dim.Render("nothing"))
	}
	if it, ok := m.selected(); ok && it.keys != "" {
		fmt.Fprintln(&b, dim.Render("  "+it.keys))
	}
	if m.input != nil {
		fmt.Fprintf(&b, "\nAnswer %s: %s█  (enter send · esc cancel)\n", m.input.id, m.input.text)
	}
	if m.msg != "" {
		fmt.Fprintf(&b, "\n%s\n", m.msg)
	}
	fmt.Fprintf(&b, "\n%s\n", dim.Render("click here, then ↑↓ select · t next task · updated "+s.at.Format("15:04:05")))
	return b.String()
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
