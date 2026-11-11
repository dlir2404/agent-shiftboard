package board

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
)

// The actions panel is a modal for one session (shown in a tmux popup):
// a menu on the left, the selected view on the right.

var panelMenu = []string{"Overview", "Checklist", "Agents", "Needs you", "Open the lead", "Switch lead CLI"}

type panel struct {
	open          Opener
	taskID        int64
	row           item
	kids          []item
	menu          int
	inContent     bool
	cursor        int
	scroll        int
	width, height int
	msg           string
	input         *input
	missing       bool
}

type panelMsg struct {
	row     item
	kids    []item
	missing bool
}

// RunPanel shows the actions panel for one session, optionally opened on
// a view ("Needs you", "Checklist", ...).
func RunPanel(open Opener, taskID int64, view string) error {
	if !coord.ActorFromEnv().IsUser() {
		return coord.ErrUserOnly
	}
	Passive = true
	p := panel{open: open, taskID: taskID}
	for i, n := range panelMenu {
		if strings.EqualFold(n, view) {
			p.menu, p.inContent = i, true
		}
	}
	_, err := tea.NewProgram(p, tea.WithAltScreen()).Run()
	return err
}

func (p panel) Init() tea.Cmd { return tea.Batch(p.load(), tick()) }

func (p panel) load() tea.Cmd {
	open, id := p.open, p.taskID
	return func() tea.Msg {
		s := loadSnapshot(open)
		for i, it := range s.items {
			if it.kind == "task" && it.task.ID == id {
				j := i + 1
				for j < len(s.items) && s.items[j].kind != "task" {
					j++
				}
				return panelMsg{row: it, kids: append([]item(nil), s.items[i+1:j]...)}
			}
		}
		return panelMsg{missing: true}
	}
}

// rows returns the selectable rows of a list view.
func (p panel) rows() []item {
	var out []item
	switch panelMenu[p.menu] {
	case "Checklist":
		for _, k := range p.kids {
			if k.kind == "item" {
				out = append(out, k)
			}
		}
	case "Agents":
		out = p.row.agentList
	case "Needs you":
		for _, k := range p.kids {
			if (k.kind != "item" && k.kind != "more" && k.kind != "agent") || (k.kind == "agent" && k.status == "waiting_for_approval") {
				out = append(out, k)
			}
		}
	}
	return out
}

func (p panel) count(name string) int {
	q := p
	for i, n := range panelMenu {
		if n == name {
			q.menu = i
		}
	}
	return len(q.rows())
}

func (p panel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.width, p.height = msg.Width, msg.Height
	case tickMsg:
		return p, tea.Batch(p.load(), tick())
	case panelMsg:
		p.row, p.kids, p.missing = msg.row, msg.kids, msg.missing
		if n := len(p.rows()); p.cursor >= n {
			p.cursor = max(0, n-1)
		}
	case actionMsg:
		if msg.msg == "quit" {
			return p, tea.Quit
		}
		p.msg = msg.msg
		return p, p.load()
	case tea.KeyMsg:
		if p.input != nil {
			return p.updateInput(msg)
		}
		return p.onKey(msg.String())
	}
	return p, nil
}

func (p panel) onKey(key string) (tea.Model, tea.Cmd) {
	p.msg = ""
	name := panelMenu[p.menu]
	list := name == "Checklist" || name == "Agents" || name == "Needs you"
	switch key {
	case "ctrl+c", "q":
		return p, tea.Quit
	case "esc", "left", "h":
		if p.inContent {
			p.inContent = false
			return p, nil
		}
		if key == "esc" {
			return p, tea.Quit
		}
	case "up", "k":
		switch {
		case !p.inContent:
			p.menu = (p.menu + len(panelMenu) - 1) % len(panelMenu)
			p.cursor, p.scroll = 0, 0
		case list:
			p.cursor = max(0, p.cursor-1)
		default:
			p.scroll = max(0, p.scroll-1)
		}
	case "down", "j":
		switch {
		case !p.inContent:
			p.menu = (p.menu + 1) % len(panelMenu)
			p.cursor, p.scroll = 0, 0
		case list:
			p.cursor = min(len(p.rows())-1, p.cursor+1)
		default:
			p.scroll++
		}
	case "enter", "right", "l":
		switch name {
		case "Open the lead":
			return p, p.do(func() error { return callOpenLead(p.taskID, "auto", "") })
		case "Switch lead CLI":
			p.input = &input{purpose: "provider", taskID: p.taskID}
			return p, nil
		}
		if !p.inContent {
			p.inContent = true
			return p, nil
		}
		if list {
			rows := p.rows()
			if p.cursor < len(rows) {
				return p.openRow(rows[p.cursor])
			}
		}
	default:
		if p.inContent && name == "Needs you" {
			rows := p.rows()
			if p.cursor < len(rows) {
				if fn, done := actFn(rows[p.cursor], key); fn != nil {
					open := p.open
					return p, func() tea.Msg { return actionMsg{msg: runAction(open, fn, done)} }
				}
			}
		}
	}
	return p, nil
}

// openRow opens what a row points to: an agent's window (or resumes it), or
// an answer prompt for a question.
func (p panel) openRow(it item) (tea.Model, tea.Cmd) {
	switch {
	case it.kind == "question":
		p.input = &input{purpose: "answer", id: it.id}
		return p, nil
	case it.pane != "" && Focus != nil:
		pane := it.pane
		return p, p.do(func() error { return Focus(pane) })
	case it.kind == "agent" && it.role == "lead":
		return p, p.do(func() error { return callOpenLead(p.taskID, "auto", "") })
	case it.worker != "" && OpenWorker != nil && it.status != "done":
		id := it.worker
		return p, p.do(func() error { return OpenWorker(id) })
	}
	p.msg = "nothing to open for this row"
	return p, nil
}

func callOpenLead(taskID int64, mode, provider string) error {
	if OpenLead == nil {
		return fmt.Errorf("opening sessions needs the tmux workspace")
	}
	return OpenLead(taskID, mode, provider)
}

// do runs a workspace action and closes the panel when it succeeds.
func (p panel) do(fn func() error) tea.Cmd {
	return func() tea.Msg {
		if err := fn(); err != nil {
			return actionMsg{msg: "error: " + err.Error()}
		}
		return actionMsg{msg: "quit"}
	}
}

func (p panel) updateInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		p.input = nil
	case tea.KeyEnter:
		in := *p.input
		p.input = nil
		text := strings.TrimSpace(in.text)
		if in.purpose == "provider" {
			return p, p.do(func() error { return callOpenLead(in.taskID, "new", text) })
		}
		open := p.open
		return p, func() tea.Msg {
			return actionMsg{msg: runAction(open, func(ctx context.Context, c *coord.Coordinator) error {
				return c.AnswerPending(ctx, coord.User, in.id, text)
			}, in.id+" answered")}
		}
	case tea.KeyBackspace:
		if r := []rune(p.input.text); len(r) > 0 {
			p.input.text = string(r[:len(r)-1])
		}
	default:
		p.input.text += msg.String()
	}
	return p, nil
}

func (p panel) View() string {
	w, h := p.width, p.height
	if w == 0 {
		w, h = 100, 30
	}
	if p.missing {
		return "This session is no longer open.\n\nesc close"
	}
	r := p.row
	head := title.Render(r.id+" "+r.text) + "  " + dim.Render(r.agents)
	if r.progress != "" {
		head += dim.Render(" · " + r.progress + " done")
	}
	menuW := 22
	contentW := max(20, w-menuW-3)
	bodyH := max(5, h-4)

	var menu []string
	for i, name := range panelMenu {
		label := name
		switch name {
		case "Checklist", "Agents", "Needs you":
			if n := p.count(name); n > 0 {
				label = fmt.Sprintf("%-15s %2d", name, n)
			}
		}
		line := padRight(" "+label, menuW)
		if i == p.menu {
			if p.inContent {
				line = accent.Render(line)
			} else {
				line = highlight(line, menuW)
			}
		}
		if name == "Needs you" && p.count(name) > 0 && i != p.menu {
			line = warn.Render(line)
		}
		menu = append(menu, line)
	}

	content := p.content(contentW)
	if len(content) > bodyH {
		start := min(p.scroll, len(content)-bodyH)
		if p.inContent && (panelMenu[p.menu] != "Overview") {
			start = min(max(0, p.cursor-bodyH/2), len(content)-bodyH)
		}
		content = content[start : start+bodyH]
	}
	left := lipgloss.NewStyle().Width(menuW).Height(bodyH).Render(strings.Join(menu, "\n"))
	right := lipgloss.NewStyle().Width(contentW).Height(bodyH).MaxHeight(bodyH).Render(strings.Join(content, "\n"))
	sep := dim.Render(strings.TrimRight(strings.Repeat("│\n", bodyH), "\n"))
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, " ", sep, " ", right)

	foot := dim.Render("↑↓ move · →/enter open · ← back · esc close")
	switch {
	case p.input != nil:
		prompt := "Answer " + p.input.id
		if p.input.purpose == "provider" {
			prompt = "New lead CLI (" + strings.Join(Providers, ", ") + ")"
		}
		foot = accent.Render(prompt+": ") + p.input.text + "█" + dim.Render("  enter ok · esc cancel")
	case p.msg != "":
		foot = p.msg
	case p.inContent:
		if rows := p.rows(); p.cursor < len(rows) && rows[p.cursor].keys != "" {
			foot = keyHint.Render(rows[p.cursor].keys) + dim.Render("  · ← back · esc close")
		}
	}
	return head + "\n\n" + body + "\n" + foot
}

func (p panel) content(w int) []string {
	wrap := lipgloss.NewStyle().Width(w)
	lines := func(s string) []string { return strings.Split(wrap.Render(s), "\n") }
	var out []string
	add := func(s string) { out = append(out, lines(s)...) }
	r := p.row
	switch panelMenu[p.menu] {
	case "Overview":
		f := r.brief.Fields
		add(fmt.Sprintf("%s · lead %s · brief r%d", r.task.Status, leaseStyle(r.task.LeaseState), r.task.BriefRevision))
		if r.reason != "" {
			add(warn.Render("Lead: " + r.reason))
		}
		section := func(name string, xs []string) {
			if len(xs) == 0 {
				return
			}
			out = append(out, "", title.Render(name))
			for _, x := range xs {
				add("• " + x)
			}
		}
		if f.Goal != "" {
			out = append(out, "")
			add(title.Render("Goal: ") + f.Goal)
		}
		if f.NextAction != "" {
			add(title.Render("Next: ") + f.NextAction)
		}
		for _, x := range f.Blockers {
			add(warn.Render("Blocked: ") + x)
		}
		section("Done", f.Completed)
		section("Decisions", f.Decisions)
		section("Verification", f.Verification)
		section("References", f.RelevantRefs)
	case "Checklist", "Agents", "Needs you":
		rows := p.rows()
		if len(rows) == 0 {
			return []string{dim.Render(map[string]string{
				"Checklist": "No work items yet. The lead's own todo list appears here as it works.",
				"Agents":    "No agents.",
				"Needs you": "Nothing needs you.",
			}[panelMenu[p.menu]])}
		}
		for i, it := range rows {
			line := " " + p.rowLine(it, w-2)
			if p.inContent && i == p.cursor {
				line = highlight(line, w)
			}
			out = append(out, line)
		}
		if p.inContent && p.cursor < len(rows) {
			it := rows[p.cursor]
			out = append(out, "", dim.Render(strings.Repeat("─", w)))
			switch {
			case it.assignment != nil && it.assignment.Result != "":
				add(title.Render("Result: ") + it.assignment.Result)
			case it.kind != "item" && it.kind != "agent":
				add(it.text)
			}
		}
	case "Open the lead":
		add("Show this session's lead on the right of the workspace (resume it if its CLI exited).")
		add(dim.Render("enter"))
	case "Switch lead CLI":
		add("Hand this session to a new lead in another CLI (" + strings.Join(Providers, ", ") + "). The new lead reads the brief, rules, and open items first.")
		add(dim.Render("enter, then type the CLI name"))
	}
	return out
}

func (p panel) rowLine(it item, w int) string {
	switch it.kind {
	case "item":
		owner := ""
		if it.role != "" && !strings.HasSuffix(it.role, "lead") {
			owner = " " + dim.Render(it.role)
		}
		return itemIcon(it.status) + " " + trunc(it.text, w-lipgloss.Width(owner)-2) + owner
	case "agent":
		state := it.status
		if it.reason != "" {
			state += " · " + it.reason
		}
		if it.assignment != nil {
			state += " · " + it.assignment.Status
		}
		return statusIcon(it.status) + " " + trunc(it.text+"  "+dim.Render(state+" "+ago(it.observed)), w+20)
	default:
		return warn.Render("!") + " " + trunc(it.kind+" "+it.id+" "+it.text, w-2)
	}
}
