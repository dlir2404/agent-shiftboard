package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dlir2404/agent-shiftboard/internal/adapter"
	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/paths"
)

// The Codex adapter pre-approves an explicit tool list; it must match the
// server's tools exactly, and no user operation may be exposed.
func TestToolsMatchCodexAllowlist(t *testing.T) {
	ctx := context.Background()
	s := New("test", func(context.Context) (*coord.Coordinator, error) { return nil, nil })
	ct, st := mcp.NewInMemoryTransports()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
		for _, bad := range []string{"approve", "confirm", "revoke", "break", "apply", "cancel_task", "reopen"} {
			if strings.Contains(tool.Name, bad) {
				t.Fatalf("user operation exposed: %s", tool.Name)
			}
		}
	}
	want := adapter.CodexAllowedTools()
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("server tools and Codex allowlist differ:\nserver: %v\ncodex:  %v", got, want)
	}
}

// Every tool's input schema must use the JSON names agents send (lower
// case), never Go field names of embedded structs.
func TestToolSchemasUseJSONNames(t *testing.T) {
	ctx := context.Background()
	s := New("test", func(context.Context) (*coord.Coordinator, error) { return nil, nil })
	ct, st := mcp.NewInMemoryTransports()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	needsID := map[string]bool{"shiftboard_assignment_submit": true, "shiftboard_effect_start": true, "shiftboard_pending_record_approval": true, "shiftboard_item_set": true}
	for _, tool := range res.Tools {
		b, _ := json.Marshal(tool.InputSchema)
		schema := string(b)
		if strings.Contains(schema, `"ID"`) || strings.Contains(schema, `"Task"`) {
			t.Fatalf("%s schema uses Go field names: %s", tool.Name, schema)
		}
		if needsID[tool.Name] && !strings.Contains(schema, `"id"`) {
			t.Fatalf("%s schema has no id: %s", tool.Name, schema)
		}
	}
}

// Lifecycle tools call the operations the CLI host injects, and say so
// clearly when none were configured.
func TestLifecycleToolsUseInjectedOps(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("AGENT_SHIFTBOARD_USER_DIR", t.TempDir())
	if err := os.MkdirAll(paths.StateDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	open := func(context.Context) (*coord.Coordinator, error) { return coord.Open(root) }
	call := func(ops *Ops, name string, args map[string]any) (*mcp.CallToolResult, error) {
		var s *mcp.Server
		if ops == nil {
			s = New("test", open)
		} else {
			s = New("test", open, ops)
		}
		ct, st := mcp.NewInMemoryTransports()
		if _, err := s.Connect(ctx, st, nil); err != nil {
			t.Fatal(err)
		}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		return cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	}
	var got []string
	ops := &Ops{
		WorkerStart: func(_ context.Context, _ *coord.Coordinator, _ coord.Actor, id, provider string) (string, string, error) {
			got = append(got, id+" "+provider)
			return "s1", "%9", nil
		},
		SessionClose: func(_ context.Context, _ *coord.Coordinator, _ coord.Actor, ref string) error {
			got = append(got, "close "+ref)
			return nil
		},
	}
	if res, err := call(ops, "shiftboard_worker_start", map[string]any{"id": "a-2", "provider": "codex"}); err != nil || res.IsError {
		t.Fatalf("worker_start: %v %+v", err, res)
	}
	if res, err := call(ops, "shiftboard_session_close", map[string]any{"session": "A-2"}); err != nil || res.IsError {
		t.Fatalf("session_close: %v %+v", err, res)
	}
	if strings.Join(got, "|") != "A-2 codex|close A-2" {
		t.Fatalf("ops calls: %v", got)
	}
	res, err := call(nil, "shiftboard_session_close", map[string]any{"session": "A-2"})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("missing ops should fail: %+v", res)
	}
}
