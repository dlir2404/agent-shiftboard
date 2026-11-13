package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Recorder is what the pane client reports to the coordinator. Every call
// acts for the session the client hosts; none stores conversation content.
type Recorder interface {
	// Status records a status observation (source "stream").
	Status(status, reason string)
	// Turn records that a user turn started.
	Turn()
	// Native records the agent's native session ID.
	Native(id string)
	// Plan mirrors the agent's plan as the session's work items.
	Plan(entries []PlanEntry)
	// Deliver hands the next queued message for this session, if any, to
	// send, which submits it as a turn (D-25). It reports whether one was
	// delivered.
	Deliver(send func(text string) error) bool
	// MCPUsed reports whether the session's MCP token has been used since
	// it was issued.
	MCPUsed() bool
}

// Client is the user's view of one ACP agent session in a terminal pane:
// it shows the streamed conversation, takes typed prompts, and asks the
// user to answer the agent's permission requests. It never answers one
// itself. In is raw terminal input; Out is the terminal.
type Client struct {
	Rec     Recorder
	In      io.Reader
	Out     io.Writer
	Cwd     string
	Servers []MCPServer
	// Name labels the prompt, e.g. "hermes".
	Name string
	// DeliverEvery is how often an idle client looks for queued messages.
	DeliverEvery time.Duration
	// MCPGrace is how long after the first turn starts the client waits
	// before warning that the MCP server was never used.
	MCPGrace time.Duration

	conn    *Conn
	session string
	q       eventQueue
}

// eventQueue is an unbounded queue, so the protocol read loop never blocks
// on the user interface (a resume replays the whole conversation before it
// answers).
type eventQueue struct {
	mu    sync.Mutex
	items []func(*ui)
	ready chan struct{}
}

func (q *eventQueue) push(f func(*ui)) {
	q.mu.Lock()
	q.items = append(q.items, f)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *eventQueue) drain() []func(*ui) {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items = nil
	return items
}

// Connect wires the client to an agent's stdio and returns the connection.
func (c *Client) Connect(agentOut io.Reader, agentIn io.Writer) *Conn {
	c.q.ready = make(chan struct{}, 1)
	c.conn = NewConn(agentOut, agentIn, clientHandler{c})
	return c.conn
}

type clientHandler struct{ c *Client }

func (h clientHandler) Notify(method string, params json.RawMessage) {
	if method != MethodSessionUpdate {
		return
	}
	var u Update
	if json.Unmarshal(params, &u) != nil {
		return
	}
	h.c.q.push(func(s *ui) { h.c.printUpdate(s, u) })
}

func (h clientHandler) Request(ctx context.Context, method string, params json.RawMessage) (any, *RPCError) {
	if method != MethodRequestPermission {
		// No file system or terminal access was offered.
		return nil, &RPCError{Code: -32601, Message: "method not supported by this client"}
	}
	var r PermissionRequest
	if err := json.Unmarshal(params, &r); err != nil || len(r.Options) == 0 {
		return nil, &RPCError{Code: -32602, Message: "invalid permission request"}
	}
	answer := make(chan any, 1)
	h.c.q.push(func(s *ui) { h.c.openAsk(s, r, answer) })
	select {
	case a := <-answer:
		return a, nil
	case <-ctx.Done():
		return Dismissed(), nil
	}
}

// ui is the terminal state, touched only on the Run goroutine.
type ui struct {
	buf       []rune
	running   bool
	queued    []string
	asks      []pendingAsk
	atBOL     bool
	promptOn  bool
	firstTurn time.Time
	mcpWarned bool
	titles    map[string]string
}

type pendingAsk struct {
	req    PermissionRequest
	answer chan any
}

func (c *Client) write(s string) {
	_, _ = io.WriteString(c.Out, strings.ReplaceAll(s, "\n", "\r\n"))
}

// Run attaches to the session (resuming resumeID when set, otherwise
// creating one), sends firstPrompt if any, and runs until the user quits
// or the agent goes away. It returns the stop reason to record (a value of
// the stop-reason vocabulary) and, for a failure, the error.
func (c *Client) Run(ctx context.Context, resumeID, firstPrompt string) (string, error) {
	if c.conn == nil {
		return "process_crash", fmt.Errorf("client not connected")
	}
	if c.Name == "" {
		c.Name = "agent"
	}
	if c.DeliverEvery == 0 {
		c.DeliverEvery = 2 * time.Second
	}
	if c.MCPGrace == 0 {
		c.MCPGrace = 2 * time.Minute
	}
	s := &ui{atBOL: true, titles: map[string]string{}}
	flush := func() {
		for _, f := range c.q.drain() {
			f(s)
		}
	}
	info, err := c.conn.Initialize(ctx, "agent-shiftboard", "1")
	if err != nil {
		return "process_crash", fmt.Errorf("initialize: %w", err)
	}
	if resumeID != "" {
		if !info.CanResume() {
			return "process_crash", fmt.Errorf("%s does not support session/resume", info.AgentInfo.Name)
		}
		c.write(fmt.Sprintf("\x1b[2mResuming %s session %s…\x1b[0m\n", c.Name, resumeID))
		if err := c.conn.ResumeSession(ctx, resumeID, c.Cwd, c.Servers); err != nil {
			return "process_crash", fmt.Errorf("resume: %w", err)
		}
		c.session = resumeID
	} else {
		id, err := c.conn.NewSession(ctx, c.Cwd, c.Servers)
		if err != nil {
			return "process_crash", fmt.Errorf("new session: %w", err)
		}
		c.session = id
	}
	flush() // the replayed conversation, on resume
	c.Rec.Native(c.session)
	if !s.atBOL {
		c.write("\n")
		s.atBOL = true
	}
	c.write(fmt.Sprintf("\x1b[2m%s %s · session %s · Ctrl+C stops a turn · Ctrl+D quits when idle\x1b[0m\n",
		info.AgentInfo.Name, info.AgentInfo.Version, c.session))

	keys := make(chan []byte, 64)
	go func() {
		b := make([]byte, 1024)
		for {
			n, err := c.In.Read(b)
			if n > 0 {
				keys <- append([]byte(nil), b[:n]...)
			}
			if err != nil {
				close(keys)
				return
			}
		}
	}()

	done := make(chan promptResult, 1)
	start := func(text string) {
		s.running = true
		if s.firstTurn.IsZero() {
			s.firstTurn = time.Now()
		}
		c.hidePrompt(s)
		c.newline(s)
		c.write("\x1b[1m› \x1b[0m" + text + "\n")
		c.Rec.Turn()
		c.Rec.Status("running", "")
		go func() {
			r, err := c.conn.Prompt(ctx, c.session, text)
			done <- promptResult{r, err}
		}()
	}
	if firstPrompt != "" {
		start(firstPrompt)
	} else {
		c.Rec.Status("idle", "")
		c.showPrompt(s)
	}
	tick := time.NewTicker(c.DeliverEvery)
	defer tick.Stop()
	defer func() {
		for _, a := range s.asks {
			a.answer <- Dismissed()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return "user_stopped", nil
		case <-c.conn.Done():
			flush()
			c.hidePrompt(s)
			c.newline(s)
			c.write("\x1b[31mThe agent process ended.\x1b[0m\n")
			return "process_crash", ErrClosed
		case <-c.q.ready:
			flush()
		case r := <-done:
			flush() // updates sent before the response
			s.running = false
			c.endTurn(s, r)
			if len(s.queued) > 0 {
				next := s.queued[0]
				s.queued = s.queued[1:]
				start(next)
			} else {
				c.showPrompt(s)
			}
		case <-tick.C:
			if !s.running && len(s.asks) == 0 && len(s.buf) == 0 {
				c.Rec.Deliver(func(text string) error { start(text); return nil })
			}
			if !s.mcpWarned && !s.firstTurn.IsZero() && time.Since(s.firstTurn) > c.MCPGrace {
				s.mcpWarned = true
				if !c.Rec.MCPUsed() {
					c.notice(s, "\x1b[33mThe agent has not connected to AgentShiftboard's MCP server, so it cannot report to the lead. Check the agent's own logs.\x1b[0m")
				}
			}
		case b, ok := <-keys:
			if !ok {
				return "user_stopped", nil
			}
			if c.keys(s, b, start) {
				c.hidePrompt(s)
				c.newline(s)
				return "user_stopped", nil
			}
		}
	}
}

type promptResult struct {
	stop string
	err  error
}

func (c *Client) newline(s *ui) {
	if !s.atBOL {
		c.write("\n")
		s.atBOL = true
	}
}

func (c *Client) endTurn(s *ui, r promptResult) {
	c.hidePrompt(s)
	c.newline(s)
	switch {
	case r.err != nil:
		c.write(fmt.Sprintf("\x1b[31m%s: %v\x1b[0m\n", c.Name, r.err))
		// No documented error codes: the client waits for input, but what
		// happened (quota, authentication, network) is not known.
		c.Rec.Status("unknown", "unknown")
	case r.stop == "end_turn":
		c.Rec.Status("idle", "")
	default:
		c.write(fmt.Sprintf("\x1b[2m(turn ended: %s)\x1b[0m\n", r.stop))
		c.Rec.Status("idle", "")
	}
}

func (c *Client) notice(s *ui, text string) {
	c.hidePrompt(s)
	c.newline(s)
	c.write(text + "\n")
	c.showPrompt(s)
}

func (c *Client) promptLine(s *ui) string {
	if len(s.asks) > 0 {
		return fmt.Sprintf("\x1b[1;33mAnswer 1-%d ❯\x1b[0m %s", len(s.asks[0].req.Options), string(s.buf))
	}
	return fmt.Sprintf("\x1b[1m%s ❯\x1b[0m %s", c.Name, string(s.buf))
}

// The prompt line shows while idle or while a permission request waits.
func (c *Client) showPrompt(s *ui) {
	if s.promptOn || (s.running && len(s.asks) == 0) {
		return
	}
	c.newline(s)
	c.write(c.promptLine(s))
	s.promptOn = true
}

func (c *Client) hidePrompt(s *ui) {
	if s.promptOn {
		c.write("\r\x1b[K")
		s.promptOn = false
	}
}

func (c *Client) redraw(s *ui) {
	c.hidePrompt(s)
	c.showPrompt(s)
}

// printUpdate shows one session update. Thoughts are not shown.
func (c *Client) printUpdate(s *ui, u Update) {
	if c.session != "" && u.SessionID != "" && u.SessionID != c.session {
		return
	}
	x := u.Update
	switch x.Kind {
	case "agent_message_chunk":
		t := Text(x.Content)
		if t == "" {
			return
		}
		c.hidePrompt(s)
		c.write(t)
		s.atBOL = strings.HasSuffix(t, "\n")
	case "user_message_chunk": // replayed on resume
		t := Text(x.Content)
		if t == "" {
			return
		}
		c.hidePrompt(s)
		c.newline(s)
		c.write("\x1b[1m› \x1b[0m" + t + "\n")
	case "tool_call":
		if x.Title != "" {
			s.titles[x.ToolCallID] = x.Title
		}
		c.hidePrompt(s)
		c.newline(s)
		c.write("\x1b[36m• " + firstNonEmpty(x.Title, x.ToolKind, "tool") + "\x1b[0m\n")
	case "tool_call_update":
		if x.Title != "" {
			s.titles[x.ToolCallID] = x.Title
		}
		if x.Status == "failed" {
			c.hidePrompt(s)
			c.newline(s)
			c.write("\x1b[31m  ✗ " + firstNonEmpty(s.titles[x.ToolCallID], "tool") + " failed\x1b[0m\n")
		}
	case "plan":
		c.Rec.Plan(x.Entries)
		c.hidePrompt(s)
		c.newline(s)
		var b strings.Builder
		b.WriteString("\x1b[2mPlan:\n")
		for _, e := range x.Entries {
			mark := "○"
			switch e.Status {
			case "completed":
				mark = "✓"
			case "in_progress":
				mark = "▸"
			}
			b.WriteString("  " + mark + " " + e.Content + "\n")
		}
		b.WriteString("\x1b[0m")
		c.write(b.String())
	default:
		return
	}
	c.showPrompt(s)
}

func (c *Client) openAsk(s *ui, r PermissionRequest, answer chan any) {
	s.asks = append(s.asks, pendingAsk{r, answer})
	if len(s.asks) > 1 {
		return // shown when the first is answered
	}
	c.printAsk(s)
}

func (c *Client) printAsk(s *ui) {
	r := s.asks[0].req
	c.hidePrompt(s)
	c.newline(s)
	title := firstNonEmpty(r.ToolCall.Title, s.titles[r.ToolCall.ToolCallID], r.ToolCall.Kind, "an action")
	c.write(fmt.Sprintf("\x1b[1;33m%s asks for permission:\x1b[0m %s\n", c.Name, title))
	if t := strings.TrimSpace(Text(r.ToolCall.Content)); t != "" {
		c.write(indent(clip(t, 1200)) + "\n")
	}
	for i, o := range r.Options {
		c.write(fmt.Sprintf("  %d. %s\n", i+1, o.Name))
	}
	c.Rec.Status("waiting_for_approval", "")
	c.showPrompt(s)
}

// answerAsk sends the user's answer to the first open request.
func (c *Client) answerAsk(s *ui, answer any, label string) {
	a := s.asks[0]
	s.asks = s.asks[1:]
	a.answer <- answer
	c.hidePrompt(s)
	c.newline(s)
	c.write(fmt.Sprintf("\x1b[2m(%s)\x1b[0m\n", label))
	if len(s.asks) > 0 {
		c.printAsk(s)
		return
	}
	if s.running {
		c.Rec.Status("running", "")
	}
	c.showPrompt(s)
}

// keys handles raw terminal input. It reports whether the user quit.
func (c *Client) keys(s *ui, b []byte, start func(string)) bool {
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		b = b[n:]
		switch {
		case r == 3: // Ctrl+C
			switch {
			case s.running:
				// The user stops the turn; ACP requires open permission
				// requests to be answered as cancelled then.
				_ = c.conn.Cancel(c.session)
				for len(s.asks) > 0 {
					c.answerAsk(s, Dismissed(), "turn stopped")
				}
				s.buf = nil
				c.notice(s, "\x1b[2m(stopping the turn…)\x1b[0m")
			default:
				s.buf = nil
				c.redraw(s)
			}
		case r == 4: // Ctrl+D
			if !s.running && len(s.asks) == 0 && len(s.buf) == 0 {
				return true
			}
		case r == '\r' || r == '\n':
			line := strings.TrimSpace(string(s.buf))
			s.buf = nil
			if len(s.asks) > 0 {
				opts := s.asks[0].req.Options
				var i int
				if _, err := fmt.Sscanf(line, "%d", &i); err != nil || i < 1 || i > len(opts) {
					c.redraw(s)
					continue
				}
				c.answerAsk(s, Selected(opts[i-1].OptionID), "answered: "+opts[i-1].Name)
				continue
			}
			if line == "" {
				c.redraw(s)
				continue
			}
			if s.running {
				s.queued = append(s.queued, line)
				continue
			}
			start(line)
		case r == 127 || r == 8: // Backspace
			if len(s.buf) > 0 {
				s.buf = s.buf[:len(s.buf)-1]
				c.redraw(s)
			}
		case r == 21: // Ctrl+U
			s.buf = nil
			c.redraw(s)
		case r == 27: // escape sequence (arrows, paste marks): ignored
			b = skipEscape(b)
		case r >= 32 || r == '\t':
			s.buf = append(s.buf, r)
			if s.promptOn {
				c.write(string(r))
			}
		}
	}
	return false
}

func skipEscape(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	if b[0] == '[' || b[0] == 'O' {
		for i := 1; i < len(b); i++ {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				return b[i+1:]
			}
		}
		return nil
	}
	return b[1:]
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func indent(s string) string { return "  " + strings.ReplaceAll(s, "\n", "\n  ") }

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
