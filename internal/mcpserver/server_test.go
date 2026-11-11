package mcpserver

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dlir2404/agent-shiftboard/internal/adapter"
	"github.com/dlir2404/agent-shiftboard/internal/coord"
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
