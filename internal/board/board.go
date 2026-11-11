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
	// OpenWorker resumes a worker session whose CLI exited.
	OpenWorker func(sessionID string) error
	// Passive skips side effects (reconcile, waking leads, closing
	// workers) when loading; the action panel sets it.
	Passive bool
	// OpenPanel opens the actions panel of a session.
	OpenPanel func(taskID int64) error
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
				if p := nativePrompt(Capture(ag.pane, 40)); p != "" {
					ag.status, ag.reason = "waiting_for_approval", p
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
			role := x.Role
			if x.ID == t.LeadSession || x.ID == t.PendingLead {
				role = "lead"
			}
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
			case gone:
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
		row := item{kind: "task", id: fmt.Sprintf("#%d", t.ID), text: t.Title, task: t, brief: b,
			agents: strings.Join(chips, " · "), agentList: agentRows, keys: "space actions · →/enter open the lead"}
		if lead != nil {
			row.provider, row.status, row.reason, row.pane, row.observed = lead.s.Provider, lead.status, lead.reason, lead.pane, lead.s.ObservedAt
			if lead.pane == "" {
				row.keys = "space actions · →/enter resume the lead"
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
			role := x.Role
			if x.ID == t.LeadSession || x.ID == t.PendingLead {
				role = "lead"
			}
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
		case " ":
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
						if err := OpenPanel(id); err != nil {
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
		w, h = 38, 40
	}
	// Everything must fit in h lines, or the terminal scrolls the top of
	// the list away: header, rows, separator, detail (capped), footer.
	var head []string
	head = append(head, accent.Render("AgentShiftboard"),
		good.Render(fmt.Sprintf("● %d running", s.running))+"  "+warn.Render(fmt.Sprintf("◐ %d need you", s.waiting)), "")
	if len(s.items) == 0 {
		head = append(head, dim.Render("No open sessions."), dim.Render("Press n to start one."))
	}
	if m.filter != "" {
		head = append(head, accent.Render("/"+m.filter)+dim.Render("  esc clears"))
	}

	foot := dim.Render("↑↓ · space actions · →/enter open lead\n/ find · n new session")
	if m.input != nil {
		prompt := "Answer " + m.input.id
		if m.input.purpose == "provider" {
			prompt = "New lead CLI (" + strings.Join(Providers, ", ") + ")"
		}
		if m.input.purpose == "filter" {
			prompt = "Find"
		}
		foot = accent.Render(prompt+": ") + m.input.text + "█"
	} else if m.msg != "" {
		foot = m.msg
	}
	footLines := strings.Split(lipgloss.NewStyle().Width(w).Render(foot), "\n")

	var detail []string
	if it, ok := m.selected(); ok {
		detail = strings.Split(strings.TrimRight(compactDetail(it, w), "\n"), "\n")
	}

	var rows []string
	for i, it := range m.visible() {
		line := rowText(it, w-3)
		if it.pane != "" && it.pane == s.shown {
			line = strings.TrimRight(line, " ") + " " + accent.Render("▸")
		}
		if i == m.cursor {
			line = cursor.Render(padRight("▶"+strings.TrimPrefix(line, " "), w))
		}
		rows = append(rows, line)
	}
	// The list gets what it needs (up to two thirds of the space); the
	// detail uses the rest and is cut with … when it does not fit.
	free := h - len(head) - 1 - len(footLines)
	room := min(len(rows), max(3, free*2/3))
	if free-room < len(detail) {
		keep := max(2, free-room)
		if keep < len(detail) {
			detail = append(detail[:keep-1], dim.Render("…"))
		}
	}
	room = max(1, min(len(rows), free-len(detail)))
	if len(rows) > room {
		start := min(max(0, m.cursor-room/2), len(rows)-room)
		rows = rows[start : start+room]
	}
	out := append([]string{}, head...)
	out = append(out, rows...)
	out = append(out, dim.Render(strings.Repeat("─", w)))
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

// compactDetail is a short description of the selected row for the
// narrow list; the session itself is shown on the right.
func compactDetail(it item, w int) string {
	wrap := lipgloss.NewStyle().Width(w)
	var b strings.Builder
	switch it.kind {
	case "task":
		f := it.brief.Fields
		if it.agents != "" {
			b.WriteString(wrap.Render(title.Render("Agents: ")+it.agents) + "\n")
		}
		if it.reason != "" {
			b.WriteString(wrap.Render(warn.Render("Lead: "+it.reason)) + "\n")
		}
		if it.needCount > 0 {
			b.WriteString(warn.Render(fmt.Sprintf("%d thing(s) need you", it.needCount)) + "\n")
		}
		if f.NextAction != "" {
			next := strings.Split(wrap.Render(title.Render("Next: ")+f.NextAction), "\n")
			if len(next) > 3 {
				next = append(next[:2], dim.Render("…"))
			}
			b.WriteString(strings.Join(next, "\n") + "\n")
		}
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
	if lead.PaneID == "" || lead.Status != "idle" || lead.PaneID == os.Getenv("TMUX_PANE") {
		return
	}
	if Capture != nil && nativePrompt(Capture(lead.PaneID, 40)) != "" {
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
