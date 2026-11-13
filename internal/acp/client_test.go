package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRec struct {
	mu       sync.Mutex
	statuses []string
	turns    int
	native   string
	plan     []PlanEntry
	inbox    []string
	used     bool
}

func (r *fakeRec) Status(s, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if reason != "" {
		s += "/" + reason
	}
	r.statuses = append(r.statuses, s)
}
func (r *fakeRec) Turn()              { r.mu.Lock(); r.turns++; r.mu.Unlock() }
func (r *fakeRec) Native(id string)   { r.mu.Lock(); r.native = id; r.mu.Unlock() }
func (r *fakeRec) Plan(e []PlanEntry) { r.mu.Lock(); r.plan = e; r.mu.Unlock() }
func (r *fakeRec) MCPUsed() bool      { r.mu.Lock(); defer r.mu.Unlock(); return r.used }
func (r *fakeRec) Deliver(send func(string) error) bool {
	r.mu.Lock()
	if len(r.inbox) == 0 {
		r.mu.Unlock()
		return false
	}
	m := r.inbox[0]
	r.inbox = r.inbox[1:]
	r.mu.Unlock()
	return send(m) == nil
}
func (r *fakeRec) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.statuses...)
}

// fakeAgent answers the client's requests from a script.
type fakeAgent struct {
	t   *testing.T
	in  *bufio.Scanner // client -> agent
	out io.Writer      // agent -> client
}

func (a *fakeAgent) next() map[string]json.RawMessage {
	a.t.Helper()
	if !a.in.Scan() {
		a.t.Fatal("client closed")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(a.in.Bytes(), &m); err != nil {
		a.t.Fatalf("bad line %q", a.in.Text())
	}
	return m
}

func (a *fakeAgent) expect(method string) map[string]json.RawMessage {
	a.t.Helper()
	m := a.next()
	var got string
	_ = json.Unmarshal(m["method"], &got)
	if got != method {
		a.t.Fatalf("want %s, got %s", method, a.in.Text())
	}
	return m
}

func (a *fakeAgent) send(v any) {
	b, _ := json.Marshal(v)
	_, _ = a.out.Write(append(b, '\n'))
}

func (a *fakeAgent) result(m map[string]json.RawMessage, result any) {
	a.send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": result})
}

func (a *fakeAgent) update(u map[string]any) {
	a.send(map[string]any{"jsonrpc": "2.0", "method": MethodSessionUpdate, "params": map[string]any{"sessionId": "s1", "update": u}})
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestClientSession(t *testing.T) {
	toAgentR, toAgentW := io.Pipe()
	toClientR, toClientW := io.Pipe()
	keysR, keysW := io.Pipe()
	screen := &syncBuf{}
	rec := &fakeRec{inbox: []string{"AgentShiftboard M-1 from claude lead: status?"}}
	c := &Client{Rec: rec, In: keysR, Out: screen, Cwd: "/repo", Name: "hermes", DeliverEvery: 20 * time.Millisecond,
		Servers: []MCPServer{{Type: "http", Name: "agent_shiftboard", URL: "http://127.0.0.1:1/mcp", Headers: []NameValue{{Name: "Authorization", Value: "Bearer t"}}}}}
	c.Connect(toClientR, toAgentW)
	type runResult struct {
		reason string
		err    error
	}
	ran := make(chan runResult, 1)
	go func() {
		r, err := c.Run(context.Background(), "", "Begin A-1")
		ran <- runResult{r, err}
	}()

	a := &fakeAgent{t: t, in: bufio.NewScanner(toAgentR), out: toClientW}
	init := a.expect(MethodInitialize)
	if !strings.Contains(string(init["params"]), `"terminal":false`) {
		t.Fatalf("client offered terminal access: %s", init["params"])
	}
	a.result(init, map[string]any{"protocolVersion": 1, "agentInfo": map[string]string{"name": "hermes-agent", "version": "0.23.1"},
		"agentCapabilities": map[string]any{"sessionCapabilities": map[string]any{"resume": map[string]any{}}}})
	nw := a.expect(MethodSessionNew)
	if !strings.Contains(string(nw["params"]), `"headers":[{"name":"Authorization","value":"Bearer t"}]`) || !strings.Contains(string(nw["params"]), `"type":"http"`) {
		t.Fatalf("MCP server not passed: %s", nw["params"])
	}
	a.result(nw, map[string]string{"sessionId": "s1"})

	p := a.expect(MethodSessionPrompt)
	if !strings.Contains(string(p["params"]), "Begin A-1") {
		t.Fatalf("first prompt: %s", p["params"])
	}
	a.update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "Working on it"}})
	a.update(map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]string{"type": "text", "text": "secret thoughts"}})
	a.update(map[string]any{"sessionUpdate": "plan", "entries": []map[string]string{{"content": "read code", "status": "completed"}, {"content": "fix bug", "status": "in_progress"}}})
	a.update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "run tests", "kind": "execute", "status": "pending"})
	a.send(map[string]any{"jsonrpc": "2.0", "id": 99, "method": MethodRequestPermission, "params": map[string]any{
		"sessionId": "s1", "toolCall": map[string]any{"toolCallId": "t1", "title": "go test ./..."},
		"options": []map[string]string{{"optionId": "allow_once", "name": "Allow once", "kind": "allow_once"}, {"optionId": "deny", "name": "Reject", "kind": "reject_once"}}}})
	waitFor(t, "permission prompt", func() bool { return strings.Contains(screen.String(), "Answer 1-2") })
	if strings.Contains(screen.String(), "secret thoughts") {
		t.Fatal("thoughts were shown")
	}
	_, _ = keysW.Write([]byte("9\r")) // out of range: ignored
	_, _ = keysW.Write([]byte("1\r"))
	ans := a.next()
	if string(ans["id"]) != "99" || !strings.Contains(string(ans["result"]), `"optionId":"allow_once"`) {
		t.Fatalf("permission answer: %s", a.in.Text())
	}
	a.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": "failed"})
	a.result(p, map[string]string{"stopReason": "end_turn"})

	// Idle with an empty input: the queued message is delivered as a turn.
	p2 := a.expect(MethodSessionPrompt)
	if !strings.Contains(string(p2["params"]), "M-1 from claude lead") {
		t.Fatalf("message turn: %s", p2["params"])
	}
	a.result(p2, map[string]string{"stopReason": "end_turn"})

	waitFor(t, "idle", func() bool { s := rec.got(); return len(s) > 0 && s[len(s)-1] == "idle" && rec.turns == 2 })
	_, _ = keysW.Write([]byte{4}) // Ctrl+D quits when idle
	r := <-ran
	if r.reason != "user_stopped" || r.err != nil {
		t.Fatalf("run ended with %q %v", r.reason, r.err)
	}
	want := []string{"running", "waiting_for_approval", "running", "idle", "running", "idle"}
	if got := rec.got(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statuses %v, want %v", got, want)
	}
	if rec.native != "s1" || len(rec.plan) != 2 || rec.plan[1].Status != "in_progress" {
		t.Fatalf("native %q plan %+v", rec.native, rec.plan)
	}
	out := screen.String()
	for _, w := range []string{"Working on it", "• run tests", "✗ run tests failed", "(answered: Allow once)", "▸ fix bug"} {
		if !strings.Contains(out, w) {
			t.Errorf("screen lacks %q:\n%s", w, out)
		}
	}
}

// A prompt error leaves the client waiting, with the status unknown: no
// quota or authentication status is inferred from text.
func TestClientPromptErrorIsUnknown(t *testing.T) {
	toAgentR, toAgentW := io.Pipe()
	toClientR, toClientW := io.Pipe()
	keysR, keysW := io.Pipe()
	rec := &fakeRec{}
	c := &Client{Rec: rec, In: keysR, Out: &syncBuf{}, Cwd: "/repo", Name: "hermes"}
	c.Connect(toClientR, toAgentW)
	ran := make(chan string, 1)
	go func() { r, _ := c.Run(context.Background(), "s9", "hi"); ran <- r }()
	a := &fakeAgent{t: t, in: bufio.NewScanner(toAgentR), out: toClientW}
	a.result(a.expect(MethodInitialize), map[string]any{"agentCapabilities": map[string]any{"sessionCapabilities": map[string]any{"resume": map[string]any{}}}})
	res := a.expect(MethodSessionResume)
	if !strings.Contains(string(res["params"]), `"sessionId":"s9"`) {
		t.Fatalf("resume params %s", res["params"])
	}
	a.update(map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]string{"type": "text", "text": "earlier"}})
	a.result(res, nil)
	p := a.expect(MethodSessionPrompt)
	a.send(map[string]any{"jsonrpc": "2.0", "id": p["id"], "error": map[string]any{"code": -32603, "message": "HTTP 429 rate limited"}})
	waitFor(t, "unknown", func() bool { s := rec.got(); return len(s) > 0 && s[len(s)-1] == "unknown/unknown" })
	_, _ = keysW.Write([]byte{4})
	if r := <-ran; r != "user_stopped" {
		t.Fatalf("run ended with %q", r)
	}
	if rec.native != "s9" {
		t.Fatalf("native %q", rec.native)
	}
}

// The agent going away ends the client as a crash.
func TestClientAgentExit(t *testing.T) {
	_, toAgentW := io.Pipe()
	toClientR, toClientW := io.Pipe()
	c := &Client{Rec: &fakeRec{}, In: strings.NewReader(""), Out: &syncBuf{}}
	c.Connect(toClientR, toAgentW)
	_ = toClientW.Close()
	if r, err := c.Run(context.Background(), "", ""); r != "process_crash" || err == nil {
		t.Fatalf("got %q %v", r, err)
	}
}
