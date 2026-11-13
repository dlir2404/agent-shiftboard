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
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	ansiwidth "github.com/charmbracelet/x/ansi"

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
	// InputView returns a pane's styled visible tail, the plain text of the
	// cursor's row, and the cursor column; used only to tell whether the
	// CLI's input line is empty (never stored).
	InputView func(pane string) (screen, cursorLine string, cursorX int, err error)
	// Providers lists the CLIs a lead can be switched to.
	Providers []string
	// OpenWorker resumes a worker session whose CLI exited.
	OpenWorker func(sessionID string) error
	// Passive skips side effects (reconcile, waking leads, closing
	// workers) when loading; the action panel sets it.
	Passive bool
	// OpenPanel opens the actions panel of a session.
	OpenPanel func(taskID int64, view string) error
	// SetNeed publishes how many things need the user (status line).
	SetNeed func(n int)
	// Shown returns the pane on the right of the workspace.
	Shown func() string
	// Wake types one line into a session's pane and submits it.
	Wake func(pane, text string) error
	// ClosePane ends the CLI in a pane (finished workers).
	ClosePane func(pane string) error
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
		if Leave == nil {
			return err
		}
		// In the workspace the list is the left pane: if it crashed, the
		// pane would close and the layout would lose its list. Restart.
		fmt.Fprintf(os.Stderr, "\nThe session list stopped (%v); restarting…\n", err)
		time.Sleep(2 * time.Second)
		if exe, xerr := os.Executable(); xerr == nil {
			return syscall.Exec(exe, os.Args, os.Environ())
		}
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
	task   coord.Task
	brief  coord.Brief
	active bool // has running sessions, sessions waiting, or items that need the user
	needs  bool // something in it waits for the user: always shown expanded
	old    bool // not updated for a while and nothing active
	// session rows
	pane, provider, role, status, reason, observed string
	assignment                                     *coord.Assignment
	output                                         string
	// session (task) rows
	agents    string // e.g. "claude lead ● idle · codex worker ■ exited"
	agentList []item // every relevant agent of the session
	needCount int    // rows that wait for the user
	progress  string // e.g. "3/5"
	// work-item rows
	worker string // worker session ID for an assignment item
}

type snapshot struct {
	shown   string
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
	selectedID    string         // keeps the selection across refreshes
	toggled       map[int64]bool // tasks the user expanded or collapsed with space
	filter        string
	showOlder     bool
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
	if Shown != nil {
		s.shown = Shown()
	}
	if Refresh != nil && !Passive {
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
			continue
		}
		// A completed task can still have a native CLI pane open. Keep it
		// reachable until the session actually exits.
		if t.Status == "done" {
			ss, err := c.ListSessions(ctx, t.ID)
			if err != nil {
				s.err = err
				return s
			}
			for _, x := range ss {
				if x.PaneID != "" && x.Status != "stopped" && x.Status != "failed" {
					openTasks = append(openTasks, t)
					break
				}
			}
		}
	}
	sort.SliceStable(openTasks, func(i, j int) bool { return openTasks[i].UpdatedAt > openTasks[j].UpdatedAt })
	seen := map[string]bool{}
	type group struct {
		rows  []item
		score int
		upd   string
	}
	var groups []group
	for _, t := range openTasks {
		start := len(s.items)
		running0, waiting0 := s.running, s.waiting
		b, _ := c.GetBrief(ctx, t.ID)
		ss, _ := c.ListSessions(ctx, t.ID)
		as, _ := c.ListAssignments(ctx, t.ID)
		items, _ := c.ListItems(ctx, t.ID)

		// Agents: status, live pane, native prompts.
		type agent struct {
			s      coord.Session
			pane   string
			status string
			reason string
		}
		agents := map[string]*agent{}
		var chips []string
		var agentRows []item
		for _, x := range ss {
			ag := &agent{s: x, pane: x.PaneID, status: x.Status, reason: x.Reason}
			gone := x.Status == "stopped" || x.Status == "failed"
			if gone || x.PaneID == os.Getenv("TMUX_PANE") {
				ag.pane = ""
			}
			if Capture != nil && ag.pane != "" {
				screen := Capture(ag.pane, 40)
				if p := nativePrompt(screen); p != "" {
					ag.status, ag.reason = "waiting_for_approval", p
				} else if limited, at := usageLimit(screen); limited && x.Provider == "codex" {
					ag.status, ag.reason = "quota_unavailable", "out of usage"
					if at != "" {
						ag.reason += ", try again at " + at
					}
					if !Passive && x.Status != "quota_unavailable" {
						_ = c.Observe(ctx, coord.User, x.ID, "quota_unavailable", "quota", "pane")
					}
				}
			}
			agents[x.ID] = ag
			if gone && x.ID != t.LeadSession && x.ID != t.PendingLead && !openAssignment(as, x.ID) {
				continue
			}
			switch ag.status {
			case "running", "retrying":
				s.running++
			case "waiting_for_approval":
				s.waiting++
			}
			role := agentRole(t, x)
			chips = append(chips, fmt.Sprintf("%s %s %s", x.Provider, role, statusIcon(ag.status)))
			ar := item{kind: "agent", id: x.ID, pane: ag.pane, provider: x.Provider, role: role, status: ag.status,
				reason: ag.reason, observed: x.ObservedAt, task: t, worker: x.ID}
			for i := range as {
				if as[i].WorkerSession == x.ID {
					ar.assignment = &as[i]
				}
			}
			label := x.Provider + " " + role
			if ar.assignment != nil {
				label += " (" + ar.assignment.ID + ")"
			}
			ar.text = label
			switch {
			case ag.pane != "":
				ar.keys = "enter open"
			case gone && role == "lead":
				ar.keys = "enter resume the lead"
			case gone && role == "worker":
				ar.keys = "enter resume this worker"
			}
			if role == "lead" {
				agentRows = append([]item{ar}, agentRows...)
			} else {
				agentRows = append(agentRows, ar)
			}
		}
		lead := agents[t.LeadSession]
		if t.PendingLead != "" {
			lead = agents[t.PendingLead]
		}
		if lead == nil && t.Status == "done" {
			for _, x := range ss {
				if x.Role == "lead" {
					if ag := agents[x.ID]; ag != nil && ag.pane != "" {
						lead = ag
					}
				}
			}
		}
		row := item{kind: "task", id: fmt.Sprintf("#%d", t.ID), text: t.Title, task: t, brief: b,
			agents: strings.Join(chips, " · "), agentList: agentRows, keys: "⇧↑ actions · →/enter open the lead"}
		if needsLead(t) {
			row.keys = "⇧↑ actions · →/enter start a new lead"
		}
		if t.Status == "done" {
			row.keys = "⇧↑ actions · →/enter open the session"
		}
		if lead != nil {
			row.provider, row.status, row.reason, row.pane, row.observed = lead.s.Provider, lead.status, lead.reason, lead.pane, lead.s.ObservedAt
			if lead.pane == "" && t.Status != "done" {
				row.keys = "⇧↑ actions · →/enter resume the lead"
			}
		}
		done, total := 0, 0
		for _, it := range items {
			if it.Status != "cancelled" {
				total++
				if it.Status == "done" {
					done++
				}
			}
		}
		if total > 0 {
			row.progress = fmt.Sprintf("%d/%d", done, total)
		}
		s.items = append(s.items, row)

		// Work items: open ones, then the last few done ones.
		var doneRows []item
		for _, it := range items {
			if it.Status == "cancelled" {
				continue
			}
			r := item{kind: "item", id: it.ID, text: it.Text, status: it.Status, role: it.Owner, depth: 1, task: t}
			if strings.HasPrefix(it.Source, "assignment:") {
				aid := strings.TrimPrefix(it.Source, "assignment:")
				for i := range as {
					if as[i].ID == aid && as[i].WorkerSession != "" {
						r.assignment, r.worker = &as[i], as[i].WorkerSession
						if ag := agents[as[i].WorkerSession]; ag != nil {
							r.pane, r.provider = ag.pane, ag.s.Provider
							if ag.status == "waiting_for_approval" {
								r.reason = "needs you: " + ag.reason
							}
							if ag.pane != "" {
								r.keys = "→/enter open the " + ag.s.Provider + " worker"
							} else if it.Status != "done" {
								r.keys = "→/enter resume the " + ag.s.Provider + " worker"
							}
						}
					}
				}
			}
			if it.Status == "done" {
				doneRows = append(doneRows, r)
				continue
			}
			s.items = append(s.items, r)
		}
		s.items = append(s.items, doneRows...)

		live := map[string]liveAgent{}
		for id, ag := range agents {
			live[id] = liveAgent{pane: ag.pane, status: ag.status}
		}
		if !Passive {
			closeDoneWorkers(ctx, c, ss, as, live)
		}
		// Agents waiting on the user, and live workers not shown through an
		// item, get their own rows so they can always be opened.
		linked := map[string]bool{}
		for _, r := range s.items[start:] {
			if r.kind == "item" && r.worker != "" && r.status != "done" {
				linked[r.worker] = true
			}
		}
		for _, x := range ss {
			ag := agents[x.ID]
			if ag == nil || ag.pane == "" {
				continue
			}
			role := agentRole(t, x)
			switch {
			case ag.status == "waiting_for_approval":
				what := ag.reason
				if what == "" {
					what = "asks for your approval"
				}
				s.items = append(s.items, item{kind: "agent", id: x.ID, text: fmt.Sprintf("%s %s %s", x.Provider, role, what),
					pane: ag.pane, provider: x.Provider, role: role, status: ag.status, depth: 1, task: t,
					keys: "→/enter open it and answer · Shift+← comes back"})
			case role == "worker" && !linked[x.ID]:
				s.items = append(s.items, item{kind: "agent", id: x.ID, text: fmt.Sprintf("%s worker · %s", x.Provider, ag.status),
					pane: ag.pane, provider: x.Provider, role: role, status: ag.status, depth: 1, task: t,
					keys: "→/enter open this worker"})
			}
		}
		if !Passive {
			wakeLead(ctx, c, t, ss, as)
			relayMessages(ctx, c, t, ss)
		}
		for _, n := range needsYou(ctx, c, t.ID) {
			if seen[n.kind+n.id] {
				continue
			}
			seen[n.kind+n.id] = true
			n.depth, n.task = 1, t
			s.items = append(s.items, n)
			s.waiting++
		}
		g := group{rows: append([]item(nil), s.items[start:]...), upd: t.UpdatedAt, score: 2}
		for _, r := range g.rows[1:] {
			if (r.kind != "item" && r.kind != "more" && r.kind != "agent") || (r.kind == "agent" && r.status == "waiting_for_approval") {
				g.rows[0].needCount++
			}
		}
		s.items = s.items[:start]
		shown := false
		for _, r := range g.rows {
			shown = shown || (r.pane != "" && r.pane == s.shown)
		}
		switch {
		case s.waiting > waiting0:
			g.score = 0
		case s.running > running0:
			g.score = 1
		}
		doing := false
		for _, r := range g.rows {
			doing = doing || (r.kind == "item" && (r.status == "in_progress" || r.status == "review" || r.status == "blocked"))
		}
		g.rows[0].active = g.score < 2 || shown || doing || len(openTasks) == 1
		g.rows[0].needs = g.score == 0
		if !g.rows[0].active {
			if up, err := time.Parse(time.RFC3339, t.UpdatedAt); err == nil && time.Since(up) > 72*time.Hour {
				g.rows[0].old = true
			}
		}
		groups = append(groups, g)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].score != groups[j].score {
			return groups[i].score < groups[j].score
		}
		return groups[i].upd > groups[j].upd
	})
	for _, g := range groups {
		s.items = append(s.items, g.rows...)
	}
	if SetNeed != nil && !Passive {
		SetNeed(s.waiting)
	}

	return s
}

// needsLead reports an open session whose lead seat is empty (for example
// after it was reopened). A completed session needs no lead.
func needsLead(t coord.Task) bool {
	return t.LeaseState == "vacant" && t.Status != "done" && t.Status != "cancelled"
}

// agentRole names one session's part in the task. During a voluntary
// handoff the incoming session is the lead and the holder is only handing
// off: its writes are already rejected (STATE-CONTRACT), so the board must
// not show two leads.
func agentRole(t coord.Task, s coord.Session) string {
	switch {
	case t.PendingLead != "" && s.ID == t.LeadSession && s.ID != t.PendingLead:
		return "outgoing lead"
	case s.ID == t.LeadSession || s.ID == t.PendingLead:
		return "lead"
	}
	if s.Role == "lead" {
		return "former lead"
	}
	return s.Role
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
		out = append(out, item{kind: "effect", id: e.ID, text: fmt.Sprintf("outcome UNKNOWN: %s on %s (lead must reconcile)", e.Action, e.Target), keys: "enter open the lead to reconcile"})
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
		for i, it := range m.visible() {
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
			if m.cursor < len(m.visible())-1 {
				m.cursor++
			}
		case "/":
			m.input = &input{purpose: "filter", text: m.filter}
			return m, nil
		case "esc":
			m.filter, m.cursor = "", 0
		case " ", "shift+up": // tmux passes Shift+Up through to the list
			if it, ok := m.selected(); ok {
				switch it.kind {
				case "older":
					m.showOlder = !m.showOlder
				case "task":
					if OpenPanel == nil {
						m.msg = "the actions panel needs the tmux workspace"
						break
					}
					id := it.task.ID
					return m, func() tea.Msg {
						if err := OpenPanel(id, ""); err != nil {
							return actionMsg{msg: "error: " + err.Error()}
						}
						return actionMsg{}
					}
				}
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
	if it.kind == "older" && (key == "enter" || key == "right") {
		m.showOlder = !m.showOlder
		return m, nil
	}
	if key == "right" || key == "l" {
		if it.kind != "task" && it.kind != "session" && it.kind != "item" && it.kind != "agent" {
			return m, nil
		}
		key = "enter"
	}
	switch {
	case key == "enter" && it.kind == "task":
		if it.pane != "" && Focus != nil {
			pane := it.pane
			return m, func() tea.Msg {
				if err := Focus(pane); err != nil {
					return actionMsg{msg: "error: " + err.Error()}
				}
				return actionMsg{}
			}
		}
		if it.task.Status == "done" {
			m.msg = "this session is complete; its agent pane has closed"
			return m, nil
		}
		return m, openLead(it.task.ID, "auto", "")
	case key == "enter" && it.kind == "agent" && it.pane != "" && Focus != nil:
		pane := it.pane
		return m, func() tea.Msg {
			if err := Focus(pane); err != nil {
				return actionMsg{msg: "error: " + err.Error()}
			}
			return actionMsg{}
		}
	case key == "enter" && it.kind == "item":
		switch {
		case it.pane != "" && Focus != nil:
			pane := it.pane
			return m, func() tea.Msg {
				if err := Focus(pane); err != nil {
					return actionMsg{msg: "error: " + err.Error()}
				}
				return actionMsg{}
			}
		case it.worker != "" && OpenWorker != nil && it.status != "done":
			id := it.worker
			return m, func() tea.Msg {
				if err := OpenWorker(id); err != nil {
					return actionMsg{msg: "error: " + err.Error()}
				}
				return actionMsg{}
			}
		}
		return m, nil
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
		if OpenWorker != nil {
			id := it.id
			return m, func() tea.Msg {
				if err := OpenWorker(id); err != nil {
					return actionMsg{msg: "error: " + err.Error()}
				}
				return actionMsg{msg: ""}
			}
		}
		m.msg = "this session has no window"
	case key == "s" && (it.kind == "task" || strings.HasPrefix(it.role, "lead")):
		m.input = &input{purpose: "provider", taskID: it.task.ID}
	case key == "enter" && it.kind == "question":
		m.input = &input{purpose: "answer", id: it.id}
	case key == "enter" && it.kind == "effect":
		// Only the lead can check what happened and reconcile it.
		return m, openLead(it.task.ID, "auto", "")
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
	if m.input.purpose == "filter" {
		switch msg.Type {
		case tea.KeyEsc:
			m.input, m.filter = nil, ""
		case tea.KeyEnter:
			m.input = nil
		case tea.KeyBackspace:
			if r := []rune(m.input.text); len(r) > 0 {
				m.input.text = string(r[:len(r)-1])
			}
		default:
			m.input.text += msg.String()
		}
		if m.input != nil {
			m.filter = m.input.text
		}
		m.cursor = 0
		return m, nil
	}
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
	rows := m.visible()
	if m.cursor >= 0 && m.cursor < len(rows) {
		return rows[m.cursor], true
	}
	return item{}, false
}

func (m model) act(it item, key string) tea.Cmd {
	fn, done := actFn(it, key)
	if fn == nil {
		return nil
	}
	open := m.open
	return func() tea.Msg { return actionMsg{msg: runAction(open, fn, done)} }
}

// actFn maps a key on a row that needs the user to a user operation.
func actFn(it item, key string) (func(ctx context.Context, c *coord.Coordinator) error, string) {
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
	}
	return fn, done
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
	title        = lipgloss.NewStyle().Bold(true)
	dim          = lipgloss.NewStyle().Faint(true)
	warn         = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	bad          = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	good         = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	accent       = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	cursor       = lipgloss.NewStyle().Background(lipgloss.Color("238")).Foreground(lipgloss.Color("255")).Bold(true)
	keyHint      = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	header       = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	sectionTitle = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Bold(true)
)

func (m model) View() string {
	s := m.snap
	if s.err != nil {
		return bad.Render("error: "+s.err.Error()) + "\n"
	}
	w, h := m.width, m.height
	if w == 0 {
		w, h = 38, 40
	}
	// Everything must fit in h lines, or the terminal scrolls the top of
	// the list away: header, session cards, separator, detail, footer.
	project := ""
	if s.root != "" {
		project = dim.Render(" · " + filepath.Base(s.root))
	}
	head := []string{
		" " + accent.Render("AgentShiftboard") + project,
		" " + good.Render(fmt.Sprintf("● %d running", s.running)) + "   " + warn.Render(fmt.Sprintf("◐ %d need you", s.waiting)),
		"",
		" " + sectionTitle.Render("SESSIONS"),
	}
	if len(s.items) == 0 {
		head = append(head, dim.Render(" No open sessions. Press n to start one."))
	}
	if m.filter != "" {
		head = append(head, " "+accent.Render("/"+m.filter)+dim.Render("  esc clears"))
	}

	foot := " " + keyHint.Render("⇧↑") + dim.Render(" actions  ") + keyHint.Render("⏎") + dim.Render(" open lead") + "\n " +
		keyHint.Render("n") + dim.Render(" new  ") + keyHint.Render("/") + dim.Render(" find  ") + keyHint.Render("⇧←/⇧→") + dim.Render(" switch pane")
	if m.input != nil {
		prompt := "Answer " + m.input.id
		if m.input.purpose == "provider" {
			prompt = "New lead CLI (" + strings.Join(Providers, ", ") + ")"
		}
		if m.input.purpose == "filter" {
			prompt = "Find"
		}
		foot = " " + accent.Render(prompt+": ") + m.input.text + "█"
	} else if m.msg != "" {
		foot = " " + m.msg
	}
	footLines := strings.Split(lipgloss.NewStyle().Width(w).Render(foot), "\n")

	var detail []string
	if it, ok := m.selected(); ok {
		detail = strings.Split(strings.TrimRight(compactDetail(it, w), "\n"), "\n")
	}

	// Session cards: two lines each (an "older" row is one line). Track
	// the line where the selected card starts for scrolling.
	var rows []string
	selStart, selLen := 0, 0
	for i, it := range m.visible() {
		card := cardLines(it, w, it.pane != "" && it.pane == s.shown)
		if i == m.cursor {
			selStart, selLen = len(rows), len(card)
			for k := range card {
				card[k] = highlight(card[k], w)
			}
		}
		rows = append(rows, card...)
	}
	free := h - len(head) - 2 - len(footLines)
	room := min(len(rows), max(4, free*3/5))
	if free-room < len(detail) {
		keep := max(2, free-room)
		if keep < len(detail) {
			detail = append(detail[:keep-1], dim.Render(" …"))
		}
	}
	room = max(2, min(len(rows), free-len(detail)))
	if len(rows) > room {
		start := min(max(0, selStart+selLen-room+1, selStart-room/3), len(rows)-room)
		rows = rows[start : start+room]
	}
	out := append([]string{}, head...)
	out = append(out, rows...)
	out = append(out, "", dim.Render(" "+strings.Repeat("─", max(1, w-2))))
	out = append(out, detail...)
	if pad := h - len(out) - len(footLines); pad > 0 {
		out = append(out, make([]string, pad)...)
	}
	out = append(out, footLines...)
	if len(out) > h {
		out = out[:h]
	}
	return strings.Join(out, "\n")
}

// cardLines renders one session as two lines: status and title, then a
// progress bar, the agents, and what needs the user.
func cardLines(it item, w int, shown bool) []string {
	if it.kind != "task" {
		return []string{" " + rowText(it, w-2)}
	}
	icon := dim.Render("·")
	if it.provider != "" {
		icon = statusIcon(it.status)
	}
	mark := ""
	if shown {
		mark = " " + accent.Render("▸")
	}
	first := " " + icon + " " + title.Render(it.id) + " " + trunc(it.text, w-5-len([]rune(it.id))-lipgloss.Width(mark)) + mark
	second := "   " + progressBar(it.progress)
	if chips := agentChips(it); chips != "" {
		second += dim.Render(" · ") + chips
	}
	if it.needCount > 0 {
		second += "  " + warn.Render(fmt.Sprintf("◐ %d", it.needCount))
	}
	return []string{first, ansiwidth.Truncate(second, w, "…")}
}

// progressBar draws "▰▰▰▱▱▱ 3/6" from "done/total".
func progressBar(progress string) string {
	var done, total int
	if _, err := fmt.Sscanf(progress, "%d/%d", &done, &total); err != nil || total == 0 {
		return dim.Render("no checklist yet")
	}
	const cells = 6
	full := done * cells / total
	return good.Render(strings.Repeat("▰", full)) + dim.Render(strings.Repeat("▱", cells-full)) + " " + dim.Render(progress)
}

// agentLabel keeps role styling consistent across the list and actions panel.
// Text labels also distinguish roles when color is unavailable or selected.
func agentLabel(a item) string {
	var label string
	switch a.role {
	case "lead":
		label = title.Render(a.provider) + " " + accent.Render("[LEAD]")
	case "former lead":
		label = dim.Render(a.provider + " [previous]")
	case "outgoing lead":
		label = dim.Render(a.provider + " [handing off]")
	default:
		label = a.provider
		if a.role != "" {
			label += " " + dim.Render("["+a.role+"]")
		}
	}
	if a.assignment != nil {
		label += " " + dim.Render(a.assignment.ID)
	}
	return label
}

// agentChips shows the current lead first, with explicit role labels.
func agentChips(it item) string {
	var out []string
	if needsLead(it.task) {
		out = append(out, warn.Render("no lead"))
	}
	for _, a := range it.agentList {
		out = append(out, agentLabel(a)+" "+statusIcon(a.status))
	}
	return strings.Join(out, dim.Render(" · "))
}

// compactDetail is a short description of the selected row for the
// narrow list; the session itself is shown on the right.
func compactDetail(it item, w int) string {
	wrap := lipgloss.NewStyle().Width(w)
	var b strings.Builder
	switch it.kind {
	case "task":
		f := it.brief.Fields
		pad := lipgloss.NewStyle().Width(w - 3)
		indent := func(text string) string {
			lines := strings.Split(pad.Render(text), "\n")
			for k := range lines {
				lines[k] = "   " + lines[k]
			}
			return strings.Join(lines, "\n")
		}
		b.WriteString(" " + title.Render(trunc(it.id+" "+it.text, w-2)) + "\n")
		if needsLead(it.task) {
			b.WriteString(" " + warn.Render("NO CURRENT LEAD · Enter to start one") + "\n")
		}
		if it.reason != "" {
			b.WriteString(" " + warn.Render(trunc("lead "+it.reason, w-2)) + "\n")
		}
		if f.NextAction != "" {
			b.WriteString(" " + sectionTitle.Render("NEXT") + "\n")
			next := strings.Split(indent(f.NextAction), "\n")
			if len(next) > 3 {
				next = append(next[:2], "   "+dim.Render("…"))
			}
			b.WriteString(strings.Join(next, "\n") + "\n")
		}
		if len(it.agentList) > 0 {
			b.WriteString(" " + sectionTitle.Render("AGENTS") + "\n")
			for _, a := range it.agentList {
				state := a.status
				if a.reason != "" {
					state = a.reason
				}
				label := agentLabel(a)
				line := "   " + statusIcon(a.status) + " " + label + "  " + dim.Render(state)
				b.WriteString(ansiwidth.Truncate(line, w, "…") + "\n")
			}
		}
		b.WriteString(" " + sectionTitle.Render("NEEDS YOU") + "\n")
		if it.needCount > 0 {
			b.WriteString("   " + warn.Render(fmt.Sprintf("%d thing(s) · space to see", it.needCount)) + "\n")
		} else {
			b.WriteString("   " + dim.Render("nothing") + "\n")
		}
		return b.String()
	case "session":
		st := it.status
		if it.reason != "" {
			st += " · " + it.reason
		}
		b.WriteString(wrap.Render(fmt.Sprintf("%s %s: %s %s", it.provider, it.role, st, dim.Render(ago(it.observed)))) + "\n")
		if a := it.assignment; a != nil {
			b.WriteString(wrap.Render(title.Render("Assignment "+a.ID+": ")+a.Scope) + "\n")
			if a.Result != "" {
				b.WriteString(wrap.Render(title.Render("Result: ")+a.Result) + "\n")
			}
		}
	case "item":
		st := map[string]string{"pending": "to do", "in_progress": "in progress", "review": "done by the worker, lead reviews", "done": "done", "blocked": "blocked"}[it.status]
		b.WriteString(wrap.Render(it.text) + "\n")
		line := st
		if it.role != "" {
			line += " · " + it.role
		}
		b.WriteString(dim.Render(wrap.Render(line)) + "\n")
		if it.reason != "" {
			b.WriteString(warn.Render(wrap.Render(it.reason)) + "\n")
		}
		if a := it.assignment; a != nil && a.Result != "" {
			b.WriteString(wrap.Render(title.Render("Result: ")+a.Result) + "\n")
		}
	default:
		b.WriteString(wrap.Render(it.text) + "\n")
	}
	if it.keys != "" {
		b.WriteString(keyHint.Render(wrap.Render(it.keys)) + "\n")
	}
	return b.String()
}

func rowText(it item, w int) string {
	indent := strings.Repeat(" ", 1+it.depth*2)
	switch it.kind {
	case "older":
		return " " + dim.Render(it.text)
	case "task":
		icon := dim.Render("·")
		if it.provider != "" {
			icon = statusIcon(it.status)
		}
		suffix := ""
		if it.progress != "" {
			suffix = " " + dim.Render(it.progress)
		}
		if it.needCount > 0 {
			suffix += " " + warn.Render(fmt.Sprintf("◐%d", it.needCount))
		} else if it.reason != "" && it.status == "waiting_for_approval" {
			suffix += " " + warn.Render("◐")
		}
		return icon + " " + trunc(title.Render(it.id)+" "+it.text, w-lipgloss.Width(suffix)+18) + suffix
	case "item":
		owner := ""
		if it.role != "" && !strings.HasSuffix(it.role, "lead") {
			owner = " " + dim.Render(strings.TrimSuffix(it.role, " worker"))
		}
		if it.reason != "" {
			owner += " " + warn.Render("needs you")
		}
		text := it.text
		if it.status == "done" {
			text = dim.Render(text)
		}
		return indent + itemIcon(it.status) + " " + trunc(text, w-len(indent)-lipgloss.Width(owner)+8) + owner
	case "more":
		return indent + dim.Render(it.text)
	case "agent":
		text := it.text
		if it.status == "waiting_for_approval" {
			text = warn.Render(text)
		}
		return indent + statusIcon(it.status) + " " + trunc(text, w-len(indent)+8)
	case "session":
		label := it.provider + " " + sessionLabel(it)
		return indent + statusIcon(it.status) + " " + trunc(label, w-len(indent)+20)
	default:
		return indent + warn.Render("!") + " " + trunc(it.kind+" "+it.id+" "+it.text, w-len(indent)-2)
	}
}

// sessionLabel says in words what a session is doing.
func sessionLabel(it item) string {
	role := "lead"
	if it.assignment != nil {
		role = "worker: " + it.assignment.Scope
	} else if !strings.HasPrefix(it.role, "lead") {
		role = it.role
	}
	var state string
	switch {
	case it.assignment != nil && it.assignment.Status == "submitted":
		state = good.Render("done, lead reviews")
	case it.reason != "" && it.status == "waiting_for_approval":
		state = warn.Render("needs you")
	case it.status == "stopped" || it.status == "failed":
		state = dim.Render("exited")
	case it.status == "waiting_for_approval":
		state = warn.Render("needs you")
	default:
		state = dim.Render(it.status)
	}
	return role + " · " + state
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
	case "quota_unavailable", "authentication_required", "unknown":
		return bad.Render("✕")
	case "stopped", "failed":
		return dim.Render("■")
	}
	return dim.Render("·")
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
	if n <= 0 {
		return "" // a pane narrower than the layout (for example while tmux resizes)
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// nativePrompt recognizes a provider CLI's own blocking prompt in the
// visible pane (display only). AgentShiftboard never answers it; it only
// tells the user that the session waits for them.
func nativePrompt(screen string) string {
	// capture-pane includes old conversation. Only the bottom of the pane can
	// represent a prompt the CLI is waiting on now.
	var recent []string
	for _, line := range strings.Split(screen, "\n") {
		if strings.TrimSpace(line) != "" {
			recent = append(recent, line)
		}
	}
	if len(recent) > 12 {
		recent = recent[len(recent)-12:]
	}
	low := strings.ToLower(strings.Join(recent, "\n"))
	if strings.Contains(low, "ask codex to do anything") {
		return ""
	}
	if optionDialog(recent) {
		if strings.Contains(low, "rate limit") {
			return "asks whether to switch models (rate limits)"
		}
		return "asks you to choose an option"
	}
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

var optionLine = regexp.MustCompile(`^\s*(?:[›❯]\s*)?\d+\.\s`)

// optionDialog recognizes a CLI's numbered selection dialog at the bottom
// of the pane: a selected option ("› 1. …") and another numbered option.
func optionDialog(recent []string) bool {
	selected, options := false, 0
	for _, l := range lastN(recent, 8) {
		t := strings.TrimLeft(l, " \u00a0")
		if !optionLine.MatchString(t) {
			continue
		}
		options++
		if strings.HasPrefix(t, "›") || strings.HasPrefix(t, "❯") {
			selected = true
		}
	}
	return selected && options >= 2
}

var tryAgain = regexp.MustCompile(`(?i)try again at ([^.]+)`)

// usageLimit recognizes Codex's out-of-usage notice ("You've hit your
// usage limit … try again at 6:49 PM.") when it is the last output above
// the composer, and returns the time it names. Codex reports no hook for
// it, so the session would otherwise keep its last status.
func usageLimit(screen string) (bool, string) {
	var lines []string
	for _, l := range strings.Split(screen, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	composer := -1
	for i := len(lines) - 1; i >= 0 && composer < 0; i-- {
		if hasInputGlyph(lines[i]) {
			composer = i
		}
	}
	if composer < 0 {
		return false, ""
	}
	// The notice starts with "■" and may wrap onto a few lines; any other
	// output or message between it and the composer means it is old.
	for i := composer - 1; i >= 0 && i >= composer-4; i-- {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "•") || hasInputGlyph(lines[i]) {
			return false, ""
		}
		if !strings.HasPrefix(t, "■") {
			continue
		}
		text := strings.Join(lines[i:composer], " ")
		if !strings.Contains(strings.ToLower(text), "hit your usage limit") {
			return false, ""
		}
		if m := tryAgain.FindStringSubmatch(text); m != nil {
			return true, strings.TrimSpace(m[1])
		}
		return true, ""
	}
	return false, ""
}

// openAssignment reports whether a worker's assignment still awaits the
// lead (so the worker stays listed with its result after it exits).
func openAssignment(as []coord.Assignment, worker string) bool {
	for _, a := range as {
		if a.WorkerSession == worker && (a.Status == "assigned" || a.Status == "running" || a.Status == "submitted") {
			return true
		}
	}
	return false
}

func lastN(xs []string, n int) []string {
	if len(xs) > n {
		return xs[len(xs)-n:]
	}
	return xs
}

// wakeLead tells an idle lead that a worker submitted a result, once per
// assignment. It never types into a session that is busy, waiting for an
// approval, or showing a native prompt.
func wakeLead(ctx context.Context, c *coord.Coordinator, t coord.Task, ss []coord.Session, as []coord.Assignment) {
	if Wake == nil || !c.Policy.WakeLead() {
		return
	}
	var lead coord.Session
	for _, s := range ss {
		if s.ID == t.LeadSession {
			lead = s
		}
	}
	if !readyForTyping(lead) {
		return
	}
	for _, a := range as {
		if a.Status != "submitted" {
			continue
		}
		first, err := c.Note(ctx, t.ID, "lead_woken", a.ID)
		if err != nil || !first {
			continue
		}
		msg := fmt.Sprintf("AgentShiftboard: %s worker finished %s (%s). Review the result (assignment list) and accept or reject it.", a.Provider, a.ID, trunc(a.Scope, 60))
		_ = Wake(lead.PaneID, msg)
		return // one notice per refresh
	}
}

// readyForTyping reports whether AgentShiftboard may type one line into a
// session's pane: it is idle (so never waiting for approval), shows no
// native prompt, and holds no text the user has started typing.
func readyForTyping(s coord.Session) bool {
	if s.PaneID == "" || s.Status != "idle" || s.PaneID == os.Getenv("TMUX_PANE") {
		return false
	}
	// Fail closed: a screen that cannot be read, or one without the CLI's
	// input line (a dialog, a menu), is not ready.
	if Capture != nil {
		screen := Capture(s.PaneID, 40)
		if strings.TrimSpace(screen) == "" || nativePrompt(screen) != "" {
			return false
		}
	}
	if InputView != nil {
		screen, line, x, err := InputView(s.PaneID)
		if err != nil || !cursorAtEmptyInput(line, x) {
			return false
		}
		if found, typed := inputState(screen); !found || typed {
			return false
		}
	}
	return true
}

// RestWindow is how long PaneAtRest watches a pane for activity. Claude
// Code and Codex redraw an elapsed-time counter every second while they
// work (thinking, streaming, or waiting on a tool), so a working CLI's
// screen changes within it.
var RestWindow = 3 * time.Second

// PaneAtRest checks that a CLI whose status is unknown waits rather than
// works, before it is closed (D-26): its screen shows the CLI's empty input
// line, no native prompt (approval or selection dialog), and does not
// change for RestWindow. It fails closed: anything it cannot read counts
// as not at rest. Screens are compared, never stored.
func PaneAtRest(pane string, capture func(pane string, lines int) (string, error),
	inputView func(pane string) (screen, cursorLine string, cursorX int, err error)) error {
	look := func() (string, error) {
		screen, err := capture(pane, 40)
		if err != nil || strings.TrimSpace(screen) == "" {
			return "", fmt.Errorf("cannot read the pane")
		}
		if p := nativePrompt(screen); p != "" {
			return "", fmt.Errorf("it shows a prompt that needs the user (%s)", p)
		}
		view, line, x, err := inputView(pane)
		if err != nil {
			return "", fmt.Errorf("cannot read the input line")
		}
		if found, typed := inputState(view); !found || typed || !cursorAtEmptyInput(line, x) {
			return "", fmt.Errorf("its input line is not empty or not visible")
		}
		return screen, nil
	}
	first, err := look()
	if err != nil {
		return err
	}
	time.Sleep(RestWindow)
	second, err := look()
	if err != nil {
		return err
	}
	if first != second {
		return fmt.Errorf("its screen changed within %s, so it may be working", RestWindow)
	}
	return nil
}

// cursorAtEmptyInput reports whether the cursor sits right after the
// input glyph and its space, where an empty input line keeps it. Any text
// the user typed, on this line or on further lines, moves it.
func cursorAtEmptyInput(line string, x int) bool {
	plain := strings.TrimLeft(line, " \u00a0")
	if !strings.HasPrefix(plain, "❯") && !strings.HasPrefix(plain, "›") {
		return false
	}
	return x == ansiwidth.StringWidth(line)-ansiwidth.StringWidth(plain)+2
}

// relayMessages delivers queued messages (D-25): at most one per recipient
// per refresh, oldest first, only into a pane that is ready for typing.
func relayMessages(ctx context.Context, c *coord.Coordinator, t coord.Task, ss []coord.Session) {
	if Wake == nil {
		return
	}
	ms, err := c.QueuedMessages(ctx, t.ID)
	if err != nil || len(ms) == 0 {
		return
	}
	byID := map[string]coord.Session{}
	for _, s := range ss {
		byID[s.ID] = s
	}
	tried := map[string]bool{}
	for _, m := range ms {
		s, ok := byID[m.To]
		if !ok || tried[m.To] {
			continue
		}
		tried[m.To] = true
		if !readyForTyping(s) {
			continue
		}
		pane := s.PaneID
		_, _ = c.DeliverMessage(ctx, m.ID, func(x coord.Message) error { return Wake(pane, messageLine(x)) })
	}
}

// messageLine is the one line typed into the recipient's CLI.
func messageLine(m coord.Message) string {
	if m.From == "" {
		return fmt.Sprintf("AgentShiftboard %s from the user: %s", m.ID, m.Body)
	}
	from := m.From
	if len(from) > 8 {
		from = from[:8]
	}
	return fmt.Sprintf("AgentShiftboard %s from %s: %s (reply: agent-shiftboard message send %s \"<text>\", or MCP shiftboard_message_send)", m.ID, m.FromLabel, m.Body, from)
}

// inputState finds the CLI's input box from the provider's own boundaries,
// not from the draft's text, and reports whether any row of it holds text
// the user typed, wherever the cursor is. Claude Code draws the box between
// two border rows; Codex draws its composer on its own background. Text
// drawn dim (placeholders, suggestions) does not count. Whatever cannot be
// bounded counts as typed, so the caller does not type into it.
func inputState(screen string) (found, typed bool) {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	g := -1
	for i := len(lines) - 1; i >= 0 && g < 0; i-- {
		if hasInputGlyph(lines[i]) {
			g = i
		}
	}
	if g < 0 {
		return false, false
	}
	// The background at each row's first cell; SGR state carries across
	// rows in a styled capture.
	rowBG := make([]string, len(lines))
	var st sgrState
	var boxBG string
	for i, l := range lines {
		if i == g {
			cut := glyphEnd(l)
			_, rowBG[i] = scanLine(l[:cut], &st)
			_, boxBG = scanLine(l[cut:], &st)
			continue
		}
		_, rowBG[i] = scanLine(l, &st)
	}
	start, end := -1, -1 // the box is rows [start, end)
	for i := g - 1; i >= 0 && start < 0; i-- {
		if claudeBorder(lines[i]) {
			start = i + 1
		}
	}
	for i := g + 1; i < len(lines) && end < 0; i++ {
		if claudeBorder(lines[i]) {
			end = i
		}
	}
	if start < 0 || end < 0 {
		if boxBG == "" {
			return true, true // no boundary found
		}
		start, end = g, g+1
		for start > 0 && rowBG[start-1] == boxBG {
			start--
		}
		for end < len(lines) && rowBG[end] == boxBG {
			end++
		}
		if start == 0 || end == len(lines) {
			return true, true // the composer runs off the capture
		}
	}
	// The real prompt is the box's first glyph row; a glyph pasted into the
	// draft further down is text like any other.
	p := -1
	for i := start; i < end && p < 0; i++ {
		if hasInputGlyph(lines[i]) {
			p = i
		}
	}
	if p < 0 {
		return true, true
	}
	st = sgrState{}
	for i, l := range lines {
		if i < start || i >= end {
			scanLine(l, &st)
			continue
		}
		if i == p {
			cut := glyphEnd(l)
			scanLine(l[:cut], &st)
			l = l[cut:]
		}
		if t, _ := scanLine(l, &st); t {
			typed = true
		}
	}
	return true, typed
}

func hasInputGlyph(line string) bool {
	plain := strings.TrimLeft(stripANSI(line), " \u00a0")
	return strings.HasPrefix(plain, "❯") || strings.HasPrefix(plain, "›")
}

// glyphEnd is the byte offset just past a row's input glyph.
func glyphEnd(line string) int {
	at := strings.IndexAny(line, "❯›")
	_, size := utf8.DecodeRuneInString(line[at:])
	return at + size
}

// claudeBorder reports a Claude Code input-box border: a row that starts
// at column 0 with a run of "─" (the top border may end in a title). Draft
// rows never start at column 0: the first follows "❯", the rest are
// indented, so a typed line of "─" is not mistaken for the border.
func claudeBorder(line string) bool {
	return strings.HasPrefix(stripANSI(line), strings.Repeat("─", 8))
}

// sgrState is the part of the terminal style inputState needs.
type sgrState struct {
	dim bool
	bg  string
}

// scanLine walks one styled line, updating st. It reports whether a
// visible character is drawn without dim, and the background in effect at
// the line's first cell.
func scanLine(s string, st *sgrState) (typed bool, firstBG string) {
	first := true
	firstBG = st.bg
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				st.apply(s[i+2 : j])
			}
			i = j + 1
			if first {
				firstBG = st.bg
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if first {
			firstBG, first = st.bg, false
		}
		if r != ' ' && r != '\u00a0' && r != '\t' && !st.dim {
			typed = true
		}
	}
	return typed, firstBG
}

// apply applies one SGR parameter list, reading extended colors
// (38/48/58;5;n and 38/48/58;2;r;g;b) as a unit.
func (st *sgrState) apply(params string) {
	ps := strings.Split(params, ";")
	for k := 0; k < len(ps); k++ {
		switch p := ps[k]; p {
		case "", "0":
			st.dim, st.bg = false, ""
		case "2":
			st.dim = true
		case "22":
			st.dim = false
		case "49":
			st.bg = ""
		case "38", "48", "58":
			n := 0
			if k+1 < len(ps) && ps[k+1] == "5" {
				n = 2
			} else if k+1 < len(ps) && ps[k+1] == "2" {
				n = 4
			}
			if p == "48" && k+n < len(ps) {
				st.bg = strings.Join(ps[k:k+n+1], ";")
			}
			k += n
		default:
			if len(p) == 2 && p[0] == '4' && p[1] >= '0' && p[1] <= '7' || len(p) == 3 && strings.HasPrefix(p, "10") {
				st.bg = p
			}
		}
	}
}

func stripANSI(s string) string { return ansiwidth.Strip(s) }

// expanded reports whether a task row shows its sessions and items: the
// user's space toggle wins; otherwise tasks with activity, and every task
// while filtering, are expanded.
func (m model) expanded(task item) bool {
	if task.needs {
		return true
	}
	if v, ok := m.toggled[task.task.ID]; ok {
		return v
	}
	return task.active || m.filter != ""
}

// visible lists the rows to show: tasks ranked by attention, children of
// expanded tasks, matching rows while filtering, and older tasks folded
// into one row.
func (m model) visible() []item {
	var out []item
	var older []item
	f := strings.ToLower(m.filter)
	items := m.snap.items
	for i := 0; i < len(items); i++ {
		if items[i].kind != "task" {
			continue
		}
		task := items[i]
		j := i + 1
		for j < len(items) && items[j].kind != "task" {
			j++
		}
		children := items[i+1 : j]
		if f != "" {
			match := strings.Contains(strings.ToLower(task.text+" "+task.id), f)
			for _, c := range children {
				match = match || strings.Contains(strings.ToLower(c.provider+" "+c.text), f)
			}
			if !match {
				continue
			}
		}
		group := []item{task} // details live in the actions panel
		if task.old && f == "" {
			older = append(older, group...)
			continue
		}
		out = append(out, group...)
	}
	if len(older) > 0 {
		n := 0
		for _, o := range older {
			if o.kind == "task" {
				n++
			}
		}
		label := fmt.Sprintf("▸ Older tasks (%d) · space to show", n)
		if m.showOlder {
			label = fmt.Sprintf("▾ Older tasks (%d) · space to hide", n)
		}
		out = append(out, item{kind: "older", id: "older", text: label})
		if m.showOlder {
			out = append(out, older...)
		}
	}
	return out
}

func itemIcon(st string) string {
	switch st {
	case "done":
		return good.Render("✓")
	case "in_progress":
		return accent.Render("●")
	case "review":
		return warn.Render("◎")
	case "blocked":
		return bad.Render("!")
	}
	return dim.Render("○")
}

type liveAgent struct{ pane, status string }

// closeDoneWorkers ends idle workers whose assignment the lead has
// accepted, rejected, or cancelled. Their conversation stays resumable.
func closeDoneWorkers(ctx context.Context, c *coord.Coordinator, ss []coord.Session, as []coord.Assignment, live map[string]liveAgent) {
	if ClosePane == nil || !c.Policy.CloseDoneWorkers() {
		return
	}
	for _, a := range as {
		if a.WorkerSession == "" || (a.Status != "accepted" && a.Status != "rejected" && a.Status != "cancelled") {
			continue
		}
		ag, ok := live[a.WorkerSession]
		if !ok || ag.pane == "" || ag.status != "idle" {
			continue
		}
		if err := ClosePane(ag.pane); err == nil {
			_ = c.Observe(ctx, coord.User, a.WorkerSession, "stopped", "completed", "user")
		}
	}
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// highlight marks the selected row with a full-width bar. The text keeps its
// exact position (no marker is inserted), so moving the cursor never shifts
// the layout; inner colors are dropped so the bar stays even.
func highlight(line string, w int) string {
	return cursor.Render(padRight(ansi.ReplaceAllString(line, ""), w))
}

// Attention finds the most urgent thing that needs the user: an agent
// waiting at a prompt (returns its pane), or a session with records that
// need the user (returns its task ID).
func Attention(open Opener) (pane string, taskID int64, ok bool) {
	s := loadSnapshot(open)
	for _, it := range s.items {
		if it.kind == "task" && it.status == "waiting_for_approval" && it.pane != "" {
			return it.pane, it.task.ID, true
		}
		if it.kind == "agent" && it.status == "waiting_for_approval" && it.pane != "" {
			return it.pane, it.task.ID, true
		}
	}
	for _, it := range s.items {
		if it.kind == "task" && it.needCount > 0 {
			return "", it.task.ID, true
		}
	}
	return "", 0, false
}
