// Package adapter holds provider-specific launch, hook, and classification
// logic. Provider behaviour stays behind this package (AGENTS.md).
package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// LaunchSpec describes one session launch.
type LaunchSpec struct {
	SessionID      string // AgentShiftboard session ID
	NativeID       string // provider-native session ID
	Root           string // project root
	Self           string // absolute path of agent-shiftboard
	SessionDir     string // per-session directory for generated files
	LaunchContext  string // file holding the text given at launch (brief, rules, readback)
	InitialPrompt  string // first user turn for a new session
	PermissionMode string // from user policy; empty keeps the CLI's own settings
}

// Env returns the environment for the session's window and MCP server.
func (s LaunchSpec) Env() map[string]string {
	return map[string]string{
		"AGENT_SHIFTBOARD_SESSION_ID": s.SessionID,
		"AGENT_SHIFTBOARD_PROJECT":    s.Root,
	}
}

// claudeHookEvents are the events AgentShiftboard listens to (see
// docs/SPIKE-PHASE0.md and the Claude Code hooks reference).
var claudeHookEvents = []string{
	"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PermissionRequest",
	"Notification", "Stop", "StopFailure", "PreCompact", "SessionEnd",
}

// ClaudeSettings builds the per-session settings passed with --settings.
// Hooks given this way merge with the user's own hooks; the user's
// settings files are never edited.
func ClaudeSettings(self string) map[string]any {
	hooks := map[string]any{}
	for _, ev := range claudeHookEvents {
		entry := map[string]any{
			"hooks": []map[string]any{{
				"type":    "command",
				"command": shellQuote(self) + " hook claude " + ev,
				"timeout": 10,
			}},
		}
		if ev == "PreToolUse" {
			entry["matcher"] = "*"
		}
		hooks[ev] = []map[string]any{entry}
	}
	return map[string]any{
		"hooks": hooks,
		// Read and agent operations only; user operations refuse to run
		// inside a managed session anyway (D-16).
		"permissions": map[string]any{
			"allow": []string{
				"Bash(agent-shiftboard:*)",
				"Bash(" + self + ":*)",
				"mcp__agent-shiftboard__*",
			},
		},
	}
}

// ClaudeMCPConfig builds the per-session --mcp-config file content.
func ClaudeMCPConfig(s LaunchSpec) map[string]any {
	return map[string]any{
		"mcpServers": map[string]any{
			"agent-shiftboard": map[string]any{
				"type":    "stdio",
				"command": s.Self,
				"args":    []string{"mcp"},
				"env":     s.Env(),
			},
		},
	}
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func (s LaunchSpec) claudeCommon() ([]string, error) {
	settings := filepath.Join(s.SessionDir, "claude-settings.json")
	mcp := filepath.Join(s.SessionDir, "claude-mcp.json")
	if err := writeJSON(settings, ClaudeSettings(s.Self)); err != nil {
		return nil, err
	}
	if err := writeJSON(mcp, ClaudeMCPConfig(s)); err != nil {
		return nil, err
	}
	args := []string{"--settings", settings, "--mcp-config", mcp}
	if s.PermissionMode != "" {
		args = append(args, "--permission-mode", s.PermissionMode)
	}
	return args, nil
}

// ClaudeLaunch returns the command for a new interactive Claude session
// with a pre-assigned native session ID.
func ClaudeLaunch(s LaunchSpec) ([]string, error) {
	if s.NativeID == "" {
		return nil, fmt.Errorf("claude needs a pre-assigned session ID")
	}
	common, err := s.claudeCommon()
	if err != nil {
		return nil, err
	}
	cmd := append([]string{"claude", "--session-id", s.NativeID}, common...)
	if s.LaunchContext != "" {
		cmd = append(cmd, "--append-system-prompt", FileArg+s.LaunchContext)
	}
	if s.InitialPrompt != "" {
		cmd = append(cmd, s.InitialPrompt)
	}
	return cmd, nil
}

// ClaudeResume returns the command that resumes the same native session.
func ClaudeResume(s LaunchSpec) ([]string, error) {
	if s.NativeID == "" {
		return nil, fmt.Errorf("no native session ID recorded; start a new session instead")
	}
	common, err := s.claudeCommon()
	if err != nil {
		return nil, err
	}
	return append([]string{"claude", "--resume", s.NativeID}, common...), nil
}

// HookInput is the subset of the Claude hook payload AgentShiftboard reads.
// transcript_path is deliberately not decoded: AgentShiftboard never reads
// provider transcripts.
type HookInput struct {
	SessionID        string          `json:"session_id"`
	HookEventName    string          `json:"hook_event_name"`
	ToolName         string          `json:"tool_name"`
	ToolInput        json.RawMessage `json:"tool_input"`
	NotificationType string          `json:"notification_type"`
	Source           string          `json:"source"`
	Reason           string          `json:"reason"`
	Error            string          `json:"error"`
	LastMessage      string          `json:"last_assistant_message"`
}

// Command returns the Bash command of a PreToolUse payload, if any.
func (h HookInput) Command() string {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(h.ToolInput, &in)
	return strings.TrimSpace(in.Command)
}

// Observation is what a hook event means for session status.
type Observation struct {
	Status string // empty: no status change
	Reason string
	Turn   bool // a new user turn started
}

// ClaudeObservation maps a hook event to a status observation.
func ClaudeObservation(h HookInput, now time.Time) Observation {
	switch h.HookEventName {
	case "SessionStart":
		if h.Source == "compact" {
			return Observation{Status: "running"}
		}
		return Observation{Status: "idle"}
	case "UserPromptSubmit":
		return Observation{Status: "running", Turn: true}
	case "PreToolUse", "PostToolUse", "PreCompact":
		return Observation{Status: "running"}
	case "PermissionRequest":
		return Observation{Status: "waiting_for_approval"}
	case "Notification":
		switch h.NotificationType {
		case "permission_prompt":
			return Observation{Status: "waiting_for_approval"}
		case "idle_prompt":
			return Observation{Status: "idle"}
		}
	case "Stop":
		return Observation{Status: "idle"}
	case "StopFailure":
		return classifyClaudeFailure(h, now)
	case "SessionEnd":
		switch h.Reason {
		case "clear", "resume":
			return Observation{} // the CLI continues in another session
		}
		return Observation{Status: "stopped", Reason: "user_stopped"}
	}
	return Observation{}
}

var resetRe = regexp.MustCompile(`(?i)resets?\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)

// ResetTime parses "resets 3:45pm" from a usage-limit message. ok is false
// when no reset time is present.
func ResetTime(msg string, now time.Time) (time.Time, bool) {
	m := resetRe.FindStringSubmatch(msg)
	if m == nil {
		return time.Time{}, false
	}
	var h, min int
	fmt.Sscanf(m[1], "%d", &h)
	if m[2] != "" {
		fmt.Sscanf(m[2], "%d", &min)
	}
	switch strings.ToLower(m[3]) {
	case "pm":
		if h < 12 {
			h += 12
		}
	case "am":
		if h == 12 {
			h = 0
		}
	}
	if h > 23 || min > 59 {
		return time.Time{}, false
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), h, min, 0, 0, now.Location())
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t, true
}

func classifyClaudeFailure(h HookInput, now time.Time) Observation {
	switch h.Error {
	case "rate_limit":
		if _, ok := ResetTime(h.LastMessage, now); ok {
			return Observation{Status: "cooling_down", Reason: "quota"}
		}
		return Observation{Status: "quota_unavailable", Reason: "quota"}
	case "authentication_failed", "oauth_org_not_allowed", "account_on_hold", "billing_error", "cloud_credential_error":
		return Observation{Status: "authentication_required", Reason: "authentication"}
	case "overloaded", "server_error":
		return Observation{Status: "failed", Reason: "network_service"}
	}
	// invalid_request, max_output_tokens, model_not_found, unknown: do not
	// guess (never call an ambiguous error a quota failure).
	return Observation{Status: "unknown", Reason: "unknown"}
}

// FileArg marks an argument whose value is read from a file when the launch
// script runs, keeping long launch contexts out of the tmux command line.
const FileArg = "@file:"

// WriteScript writes a launch script that execs cmd and returns its path.
func WriteScript(dir, name string, cmd []string) (string, error) {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# Generated by AgentShiftboard.\nexec")
	for _, a := range cmd {
		if strings.HasPrefix(a, FileArg) {
			fmt.Fprintf(&b, ` "$(cat %s)"`, shellQuote(strings.TrimPrefix(a, FileArg)))
			continue
		}
		b.WriteString(" " + shellQuote(a))
	}
	b.WriteString("\n")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	return p, os.WriteFile(p, []byte(b.String()), 0o700)
}

var plain = regexp.MustCompile(`^[a-zA-Z0-9_./:=@%+,-]+$`)

func shellQuote(s string) string {
	if s != "" && plain.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
