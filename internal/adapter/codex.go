package adapter

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type codex struct{}

func (codex) Name() string                                   { return "codex" }
func (codex) PreassignsID() bool                             { return false }
func (codex) Launch(s LaunchSpec) ([]string, error)          { return CodexLaunch(s) }
func (codex) Resume(s LaunchSpec) ([]string, error)          { return CodexResume(s) }
func (codex) Observe(h HookInput, now time.Time) Observation { return CodexObservation(h, now) }
func (codex) LaunchContextEvent(h HookInput) bool {
	return (h.HookEventName == "SessionStart" && h.Source == "startup") || h.HookEventName == "UserPromptSubmit"
}

func init() { register(codex{}) }

var codexAgentShiftboardTools = []string{
	"shiftboard_task_list", "shiftboard_brief_show", "shiftboard_rule_list", "shiftboard_pending_list",
	"shiftboard_effect_list", "shiftboard_assignment_list", "shiftboard_resources_get", "shiftboard_handoff_readback",
	"shiftboard_lock_list", "shiftboard_handoff_ack", "shiftboard_brief_update", "shiftboard_rule_add",
	"shiftboard_pending_add", "shiftboard_pending_record_answer", "shiftboard_pending_record_approval", "shiftboard_pending_withdraw",
	"shiftboard_delegation_propose", "shiftboard_delegation_log", "shiftboard_effect_plan", "shiftboard_effect_start",
	"shiftboard_effect_finish", "shiftboard_effect_reconcile", "shiftboard_milestone_record", "shiftboard_delegate",
	"shiftboard_assignment_submit", "shiftboard_assignment_review", "shiftboard_assignment_cancel", "shiftboard_task_status",
	"shiftboard_task_create", "shiftboard_task_describe", "shiftboard_config_set", "shiftboard_secret_ref_add",
	"shiftboard_tool_register", "shiftboard_lock_acquire", "shiftboard_lock_release",
}

var codexHookEvents = []struct {
	event   string
	matcher string
}{
	{event: "SessionStart"},
	{event: "UserPromptSubmit"},
	{event: "PreToolUse", matcher: ".*"},
	{event: "PostToolUse", matcher: ".*"},
	{event: "PermissionRequest", matcher: ".*"},
	{event: "PreCompact"},
	{event: "Stop"},
	{event: "Interrupt"},
}

// CodexConfigOverrides builds invocation-scoped config overrides. Codex
// merges hook sources across config layers, while these entries add only
// AgentShiftboard's named MCP server and hook handlers for this process.
func CodexConfigOverrides(s LaunchSpec) []string {
	args := []string{
		"-C", s.Root,
		"-c", "mcp_servers.agent_shiftboard.command=" + tomlString(s.Self),
		"-c", "mcp_servers.agent_shiftboard.args=[\"mcp\"]",
		"-c", "mcp_servers.agent_shiftboard.cwd=" + tomlString(s.Root),
		"-c", "mcp_servers.agent_shiftboard.env={\"AGENT_SHIFTBOARD_SESSION_ID\"=" + tomlString(s.SessionID) + ",\"AGENT_SHIFTBOARD_PROJECT\"=" + tomlString(s.Root) + "}",
		"-c", "mcp_servers.agent_shiftboard.default_tools_approval_mode=\"auto\"",
		"-c", "mcp_servers.agent_shiftboard.enabled_tools=" + tomlStringArray(codexAgentShiftboardTools),
	}
	for _, hook := range codexHookEvents {
		command := shellQuote(s.Self) + " hook codex " + hook.event
		handler := "{type=\"command\",command=" + tomlString(command) + ",timeout=10"
		if hook.event == "SessionStart" || hook.event == "UserPromptSubmit" {
			handler += ",additionalContextLimit=5000"
		}
		handler += "}"
		group := "{hooks=[" + handler + "]}"
		if hook.matcher != "" {
			group = "{matcher=" + tomlString(hook.matcher) + ",hooks=[" + handler + "]}"
		}
		args = append(args, "-c", "hooks."+hook.event+"=["+group+"]")
	}
	return args
}

// tomlString uses JSON's quoted string syntax, which is also a valid TOML
// basic string and safely handles paths and hook commands with punctuation.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func tomlStringArray(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = tomlString(value)
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

// CodexLaunch starts the official interactive TUI. Its initial prompt is
// passed as a normal CLI argument; the startup hook supplies the launch brief
// as extra developer context before Codex handles that prompt.
func CodexLaunch(s LaunchSpec) ([]string, error) {
	if s.Self == "" || s.Root == "" {
		return nil, fmt.Errorf("Codex needs the project root and agent-shiftboard executable path")
	}
	cmd := []string{"codex"}
	cmd = append(cmd, CodexConfigOverrides(s)...)
	if s.PermissionMode != "" {
		switch s.PermissionMode {
		case "on-request":
			cmd = append(cmd, "--ask-for-approval", s.PermissionMode)
		default:
			return nil, fmt.Errorf("unsupported Codex permission_mode %q (use on-request; leave it unset to preserve Codex settings)", s.PermissionMode)
		}
	}
	if s.InitialPrompt != "" {
		prompt := s.InitialPrompt
		if s.LaunchContext != "" {
			prompt = fmt.Sprintf("Before doing any work, read the AgentShiftboard launch context at %q and follow its instructions. Complete any required handoff readback first. Then continue this startup request: %s", s.LaunchContext, prompt)
		}
		cmd = append(cmd, prompt)
	}
	return cmd, nil
}

// CodexResume resumes the same native Codex thread in the interactive TUI.
func CodexResume(s LaunchSpec) ([]string, error) {
	if s.NativeID == "" {
		return nil, fmt.Errorf("no native Codex session ID recorded; start a new session instead")
	}
	if s.Self == "" || s.Root == "" {
		return nil, fmt.Errorf("Codex needs the project root and agent-shiftboard executable path")
	}
	cmd := []string{"codex", "resume"}
	cmd = append(cmd, CodexConfigOverrides(s)...)
	if s.PermissionMode != "" {
		switch s.PermissionMode {
		case "on-request":
			cmd = append(cmd, "--ask-for-approval", s.PermissionMode)
		default:
			return nil, fmt.Errorf("unsupported Codex permission_mode %q (use on-request; leave it unset to preserve Codex settings)", s.PermissionMode)
		}
	}
	return append(cmd, s.NativeID), nil
}

// CodexObservation maps documented Codex hook events to coordinator status.
// SessionEnd is intentionally omitted: Codex currently reports reason=other
// for every SessionEnd and that hook is not a reliable process-exit signal.
func CodexObservation(h HookInput, _ time.Time) Observation {
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
	case "Stop", "Interrupt":
		return Observation{Status: "idle"}
	}
	return Observation{}
}

// CodexAllowedTools lists the AgentShiftboard MCP tools Codex may call
// without a prompt (read and agent operations only).
func CodexAllowedTools() []string { return append([]string(nil), codexAgentShiftboardTools...) }
