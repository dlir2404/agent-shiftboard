// Package hooks handles provider hook events: status observations,
// side-effect intent enforcement, stale-brief nudges, and re-injecting the
// brief after compaction. Hooks are best-effort aids; a hook failure must
// never break the provider CLI, so errors are reported on stderr only.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/dlir2404/agent-shiftboard/internal/adapter"
	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/render"
)

// Result is what the hook process writes back to the provider CLI.
type Result struct {
	Stdout   []byte
	ExitCode int
}

// HandleClaude processes one Claude Code hook event.
func HandleClaude(ctx context.Context, c *coord.Coordinator, actor coord.Actor, event string, stdin io.Reader, stderr io.Writer, now time.Time) Result {
	if actor.IsUser() {
		return Result{} // not a managed session
	}
	var in adapter.HookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		fmt.Fprintln(stderr, "agent-shiftboard hook: bad payload:", err)
		return Result{}
	}
	if in.HookEventName == "" {
		in.HookEventName = event
	}
	sid := actor.SessionID

	if in.HookEventName == "SessionStart" && in.SessionID != "" {
		report(stderr, c.SetSessionRuntime(ctx, sid, "", 0, in.SessionID))
	}
	obs := adapter.ClaudeObservation(in, now)
	if obs.Turn {
		report(stderr, c.ObserveTurn(ctx, sid))
	}
	if obs.Status != "" {
		report(stderr, c.Observe(ctx, actor, sid, obs.Status, obs.Reason, "hook"))
	}

	switch in.HookEventName {
	case "PreToolUse":
		if reason := enforceIntent(ctx, c, sid, in); reason != "" {
			return jsonResult(map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName":            "PreToolUse",
				"permissionDecision":       "deny",
				"permissionDecisionReason": reason,
			}})
		}
	case "UserPromptSubmit":
		if msg := staleNudge(ctx, c, sid); msg != "" {
			return additionalContext("UserPromptSubmit", msg)
		}
	case "SessionStart":
		// After compaction or /clear the model loses detail; give it the
		// current brief again (never transcript content).
		if in.Source == "compact" || in.Source == "clear" {
			if msg := briefContext(ctx, c, sid); msg != "" {
				return additionalContext("SessionStart", msg)
			}
		}
	}
	return Result{}
}

func report(w io.Writer, err error) {
	if err != nil {
		fmt.Fprintln(w, "agent-shiftboard hook:", err)
	}
}

func jsonResult(v any) Result {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return Result{Stdout: bytes.TrimSpace(buf.Bytes())}
}

func additionalContext(event, msg string) Result {
	return jsonResult(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     event,
		"additionalContext": msg,
	}})
}

var splitRe = regexp.MustCompile(`&&|\|\||[;|&\n]`)
var envAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=\S*\s+`)

// segments splits a shell command into simple commands, dropping leading
// environment assignments, so patterns match each part.
func segments(cmd string) []string {
	var out []string
	for _, s := range splitRe.Split(cmd, -1) {
		s = strings.TrimSpace(s)
		for envAssign.MatchString(s) {
			s = envAssign.ReplaceAllString(s, "")
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// MatchExternal reports which external-action pattern a tool call matches.
func MatchExternal(c *coord.Coordinator, toolName, command string) string {
	if toolName == "Bash" {
		for _, seg := range segments(command) {
			if strings.HasPrefix(seg, "agent-shiftboard") {
				continue
			}
			for _, p := range c.Policy.ExternalActions.Shell {
				if re, err := regexp.Compile(p); err == nil && re.MatchString(seg) {
					return seg
				}
			}
		}
		return ""
	}
	if strings.HasPrefix(toolName, "mcp__agent-shiftboard__") {
		return ""
	}
	for _, p := range c.Policy.ExternalActions.MCPTools {
		if re, err := regexp.Compile(p); err == nil && re.MatchString(toolName) {
			return toolName
		}
	}
	return ""
}

func enforceIntent(ctx context.Context, c *coord.Coordinator, sid string, in adapter.HookInput) string {
	match := MatchExternal(c, in.ToolName, in.Command())
	if match == "" {
		return ""
	}
	n, err := c.InFlightEffects(ctx, sid)
	if err != nil || n > 0 {
		return ""
	}
	return fmt.Sprintf("AgentShiftboard: %q is an external or non-idempotent action. Record the intent first: "+
		"`agent-shiftboard effect plan --target <exact target> [--approval P-n] <action>`, then "+
		"`agent-shiftboard effect start <E-id>`, then retry. Afterwards run `agent-shiftboard effect finish <E-id> confirmed|failed --evidence ...`.", match)
}

func staleNudge(ctx context.Context, c *coord.Coordinator, sid string) string {
	t, err := c.ResolveTask(ctx, coord.Actor{SessionID: sid}, 0)
	if err != nil {
		return ""
	}
	task, err := c.GetTask(ctx, t)
	if err != nil || task.LeadSession != sid {
		return ""
	}
	reasons, err := c.StaleReasons(ctx, t)
	if err != nil || len(reasons) == 0 {
		return ""
	}
	return fmt.Sprintf("AgentShiftboard: the task brief may be stale (%s). Update it now with `agent-shiftboard brief update --base %d ...` "+
		"(current status, decisions, next_action) so a fresh session could continue.", strings.Join(reasons, "; "), task.BriefRevision)
}

func briefContext(ctx context.Context, c *coord.Coordinator, sid string) string {
	t, err := c.ResolveTask(ctx, coord.Actor{SessionID: sid}, 0)
	if err != nil {
		return ""
	}
	v, err := render.Load(ctx, c, t)
	if err != nil {
		return ""
	}
	return "AgentShiftboard task state after context reset. Rules and open items below remain in force.\n\n" + render.Markdown(v, c.Policy.BriefTokenBudget)
}
