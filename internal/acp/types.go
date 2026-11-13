package acp

import (
	"context"
	"encoding/json"
	"strings"
)

// Method names (ACP schema).
const (
	MethodInitialize        = "initialize"
	MethodSessionNew        = "session/new"
	MethodSessionResume     = "session/resume"
	MethodSessionPrompt     = "session/prompt"
	MethodSessionCancel     = "session/cancel"
	MethodSessionUpdate     = "session/update"
	MethodRequestPermission = "session/request_permission"
)

// NameValue is an HTTP header or environment entry.
type NameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MCPServer is an MCP server the agent should connect to for one session.
// AgentShiftboard passes only "http" servers.
type MCPServer struct {
	Type    string      `json:"type"`
	Name    string      `json:"name"`
	URL     string      `json:"url"`
	Headers []NameValue `json:"headers"`
}

type initializeParams struct {
	ProtocolVersion    int            `json:"protocolVersion"`
	ClientCapabilities map[string]any `json:"clientCapabilities"`
	ClientInfo         map[string]any `json:"clientInfo"`
}

// AgentInfo is what initialize reports about the agent.
type AgentInfo struct {
	ProtocolVersion   int `json:"protocolVersion"`
	AgentCapabilities struct {
		LoadSession         bool                       `json:"loadSession"`
		SessionCapabilities map[string]json.RawMessage `json:"sessionCapabilities"`
	} `json:"agentCapabilities"`
	AgentInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"agentInfo"`
}

// CanResume reports whether the agent supports session/resume.
func (a AgentInfo) CanResume() bool {
	_, ok := a.AgentCapabilities.SessionCapabilities["resume"]
	return ok
}

// Initialize negotiates the protocol. The client offers no file system or
// terminal access: the agent uses its own tools, under its own approvals.
func (c *Conn) Initialize(ctx context.Context, name, version string) (AgentInfo, error) {
	var out AgentInfo
	err := c.Call(ctx, MethodInitialize, initializeParams{
		ProtocolVersion:    ProtocolVersion,
		ClientCapabilities: map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		ClientInfo:         map[string]any{"name": name, "version": version},
	}, &out)
	return out, err
}

// NewSession creates a native session and returns its ID.
func (c *Conn) NewSession(ctx context.Context, cwd string, servers []MCPServer) (string, error) {
	var out struct {
		SessionID string `json:"sessionId"`
	}
	err := c.Call(ctx, MethodSessionNew, map[string]any{"cwd": cwd, "mcpServers": nonNil(servers)}, &out)
	return out.SessionID, err
}

// ResumeSession reattaches to a native session by ID.
func (c *Conn) ResumeSession(ctx context.Context, id, cwd string, servers []MCPServer) error {
	return c.Call(ctx, MethodSessionResume, map[string]any{"sessionId": id, "cwd": cwd, "mcpServers": nonNil(servers)}, nil)
}

// Prompt sends one user turn and waits for the turn to end. It returns the
// stop reason: end_turn, max_tokens, max_turn_requests, refusal, cancelled.
func (c *Conn) Prompt(ctx context.Context, id, text string) (string, error) {
	var out struct {
		StopReason string `json:"stopReason"`
	}
	err := c.Call(ctx, MethodSessionPrompt, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "text", "text": text}},
	}, &out)
	return out.StopReason, err
}

// Cancel asks the agent to stop the running turn.
func (c *Conn) Cancel(id string) error {
	return c.Notify(MethodSessionCancel, map[string]string{"sessionId": id})
}

func nonNil(s []MCPServer) []MCPServer {
	if s == nil {
		return []MCPServer{}
	}
	return s
}

// Update is one session/update notification, decoded loosely: only the
// fields the pane client shows or records.
type Update struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		Kind       string          `json:"sessionUpdate"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"toolCallId"`
		Title      string          `json:"title"`
		ToolKind   string          `json:"kind"`
		Status     string          `json:"status"`
		Entries    []PlanEntry     `json:"entries"`
	} `json:"update"`
}

// PlanEntry is one entry of the agent's plan.
type PlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"`
	Status   string `json:"status"` // pending | in_progress | completed
}

// Text returns the text of a content block or of a list of content blocks
// (tool call content wraps blocks as {"type":"content","content":{...}}).
func Text(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var block struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &block) == nil && (block.Type != "" || block.Text != "") {
		if block.Type == "text" {
			return block.Text
		}
		if block.Type == "content" {
			return Text(block.Content)
		}
		return ""
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		var parts []string
		for _, x := range list {
			if t := Text(x); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// PermissionRequest is a session/request_permission request.
type PermissionRequest struct {
	SessionID string `json:"sessionId"`
	ToolCall  struct {
		ToolCallID string          `json:"toolCallId"`
		Title      string          `json:"title"`
		Kind       string          `json:"kind"`
		Content    json.RawMessage `json:"content"`
	} `json:"toolCall"`
	Options []PermissionOption `json:"options"`
}

// PermissionOption is one answer the agent offers.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // allow_once | allow_always | reject_once | reject_always
}

// Selected answers a permission request with the user's chosen option.
func Selected(optionID string) any {
	return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": optionID}}
}

// Dismissed answers a permission request without choosing (the turn was
// cancelled or the client is closing).
func Dismissed() any {
	return map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}
}
