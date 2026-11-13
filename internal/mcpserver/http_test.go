package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/paths"
)

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(r)
}

func connectHTTP(ctx context.Context, url, token string) (*mcp.ClientSession, error) {
	tr := &mcp.StreamableClientTransport{Endpoint: url, MaxRetries: -1, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: bearer{token: token, base: http.DefaultTransport}}}
	return mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, tr, nil)
}

func targetsOf(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "shiftboard_message_list", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("message_list: %v %+v", err, res)
	}
	var out struct {
		Targets []coord.MessageTarget `json:"targets"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, x := range out.Targets {
		ids = append(ids, x.Session)
	}
	return ids
}

// The shared server refuses requests without a valid, live token, and
// each token acts as its own session (D-28).
func TestHTTPHandlerActsAsTokenSession(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("AGENT_SHIFTBOARD_USER_DIR", t.TempDir())
	t.Setenv(coord.SessionEnv, "") // the shared server never uses its own environment
	if err := os.MkdirAll(paths.StateDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	open := func(context.Context) (*coord.Coordinator, error) { return coord.Open(root) }
	c, err := coord.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := c.CreateTask(ctx, coord.User, "t", "")
	lead, _ := c.StartLead(ctx, coord.User, id, coord.SessionSpec{Provider: "claude"})
	if err := c.AckHandoff(ctx, coord.Actor{SessionID: lead}, nil); err != nil {
		t.Fatal(err)
	}
	aid, _ := c.CreateAssignment(ctx, coord.Actor{SessionID: lead}, coord.AssignmentInput{TaskID: id, Scope: "x"})
	worker, err := c.RegisterWorker(ctx, coord.Actor{SessionID: lead}, aid, coord.SessionSpec{Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	leadTok, _ := c.IssueMCPToken(ctx, lead)
	workerTok, _ := c.IssueMCPToken(ctx, worker)
	c.Close()

	srv := httptest.NewServer(HTTPHandler("test", open, nil))
	defer srv.Close()

	// No token, a malformed header, or a wrong token: 401 on the raw request.
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	for _, h := range []string{"", "Bearer", "Basic " + leadTok, "Bearer nope", "Bearer " + leadTok + "x"} {
		req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(init))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("Authorization %q: status %d", h, resp.StatusCode)
		}
	}
	if _, err := connectHTTP(ctx, srv.URL, "wrong"); err == nil {
		t.Fatal("client with a wrong token connected")
	}
	if c, _ := open(ctx); c != nil {
		if used, _ := c.MCPUsed(ctx, lead); used {
			t.Fatal("refused requests marked the token used")
		}
		c.Close()
	}

	// Two tokens act as two sessions: each sees the other as its target.
	lc, err := connectHTTP(ctx, srv.URL, leadTok)
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close()
	wc, err := connectHTTP(ctx, srv.URL, workerTok)
	if err != nil {
		t.Fatal(err)
	}
	defer wc.Close()
	if got := targetsOf(t, lc); len(got) != 1 || got[0] != worker {
		t.Fatalf("lead's targets: %v", got)
	}
	if c, _ := open(ctx); c != nil {
		if used, _ := c.MCPUsed(ctx, lead); !used {
			t.Fatal("a connected token is not marked used")
		}
		c.Close()
	}
	if got := targetsOf(t, wc); len(got) != 1 || got[0] != lead {
		t.Fatalf("worker's targets: %v", got)
	}

	// An MCP session opened by one token cannot be used with another.
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"tools/list","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", wc.ID())
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	req.Header.Set("Authorization", "Bearer "+leadTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-session request: status %d", resp.StatusCode)
	}

	// Once the worker stops, its token is revoked: even its open MCP
	// session is refused, because every request is checked.
	c, _ = coord.Open(root)
	if err := c.Observe(ctx, coord.User, worker, "stopped", "user_stopped", "process"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if res, err := wc.CallTool(ctx, &mcp.CallToolParams{Name: "shiftboard_message_list", Arguments: map[string]any{}}); err == nil && !res.IsError {
		t.Fatalf("revoked token still answered: %+v", res)
	}
	if _, err := connectHTTP(ctx, srv.URL, workerTok); err == nil {
		t.Fatal("revoked token connected")
	}
	// The lead is unaffected.
	if got := targetsOf(t, lc); len(got) != 0 {
		t.Fatalf("lead's targets after the worker stopped: %v", got)
	}
}
