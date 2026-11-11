// Package policy loads user-edited policy settings. Agents cannot change
// them: they live in TOML files, not in agent-writable state
// (docs/STATE-CONTRACT.md, "Coordinator and write path").
package policy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/BurntSushi/toml"
)

// Policy holds settings from ~/.config/agent-shiftboard/config.toml,
// overridden by <project>/.agent-shiftboard/config.toml.
type Policy struct {
	// "allow" (default) or "board_only".
	ConversationApprovals string `toml:"conversation_approvals"`
	BriefTokenBudget      int    `toml:"brief_token_budget"`
	// Turns since the last brief revision before the brief is flagged stale.
	StaleTurns int `toml:"stale_turns"`
	// A running/starting/retrying session without an observation for this
	// long becomes unknown.
	ObservationWindowSeconds int `toml:"observation_window_seconds"`
	// An unknown session suspends its lease and opens effects become unknown
	// after this long.
	UnknownGraceSeconds int `toml:"unknown_grace_seconds"`
	// A new session must acknowledge within this long.
	HandoffAckTimeoutSeconds int      `toml:"handoff_ack_timeout_seconds"`
	LeadPriority             []string `toml:"lead_priority"`
	// Per-provider settings, e.g. [providers.claude] permission_mode = "manual".
	Providers map[string]Provider `toml:"providers"`
	// External-action patterns (regular expressions) matched against shell
	// commands and MCP tool names.
	ExternalActions ExternalActions `toml:"external_actions"`
}

// Provider holds per-provider policy.
type Provider struct {
	PermissionMode string `toml:"permission_mode"`
	Enabled        *bool  `toml:"enabled"`
}

// ExternalActions lists patterns that mark an action as a side effect.
type ExternalActions struct {
	Shell    []string `toml:"shell"`
	MCPTools []string `toml:"mcp_tools"`
}

// Default returns the built-in defaults.
func Default() Policy {
	return Policy{
		ConversationApprovals:    "allow",
		BriefTokenBudget:         1500,
		StaleTurns:               8,
		ObservationWindowSeconds: 600,
		UnknownGraceSeconds:      300,
		HandoffAckTimeoutSeconds: 300,
		ExternalActions: ExternalActions{
			Shell: []string{
				`^git\s+push\b`,
				`^(glab|gh)\s+(mr|pr)\s+(create|merge|close|comment|note)\b`,
				`^kubectl\s+(apply|delete|rollout|scale|patch|edit|exec)\b`,
				`^(psql|mysql)\b.*\b(insert|update|delete|alter|drop|truncate)\b`,
			},
		},
	}
}

// Load reads user then project policy over the defaults.
func Load(userDir, projectStateDir string) (Policy, error) {
	p := Default()
	for _, path := range []string{
		filepath.Join(userDir, "config.toml"),
		filepath.Join(projectStateDir, "config.toml"),
	} {
		if path == "config.toml" {
			continue
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if _, err := toml.DecodeFile(path, &p); err != nil {
			return p, fmt.Errorf("%s: %w", path, err)
		}
	}
	return p, p.Validate()
}

// Validate checks enumerated values and patterns.
func (p Policy) Validate() error {
	switch p.ConversationApprovals {
	case "allow", "board_only":
	default:
		return fmt.Errorf("conversation_approvals must be \"allow\" or \"board_only\", got %q", p.ConversationApprovals)
	}
	for _, list := range [][]string{p.ExternalActions.Shell, p.ExternalActions.MCPTools} {
		for _, pat := range list {
			if _, err := regexp.Compile(pat); err != nil {
				return fmt.Errorf("external_actions pattern %q: %w", pat, err)
			}
		}
	}
	for name, pr := range p.Providers {
		if isDangerousMode(pr.PermissionMode) {
			return fmt.Errorf("providers.%s.permission_mode %q skips permission checks; AgentShiftboard does not launch sessions in that mode", name, pr.PermissionMode)
		}
	}
	return nil
}

func isDangerousMode(m string) bool {
	switch m {
	case "bypassPermissions", "dangerously-skip-permissions", "yolo", "never":
		return true
	}
	return false
}

// SampleProjectConfig is written by `agent-shiftboard init`.
const SampleProjectConfig = `# AgentShiftboard project policy. Only you edit this file; agents cannot.
# Values here override ~/.config/agent-shiftboard/config.toml.

# "allow": the lead may record an approval you give in the conversation
# (exact action and target, with a quote). "board_only": approve on the board.
# conversation_approvals = "allow"

# brief_token_budget = 1500
# stale_turns = 8
# observation_window_seconds = 600
# unknown_grace_seconds = 300
# handoff_ack_timeout_seconds = 300
# lead_priority = ["claude", "codex"]

# [providers.claude]
# permission_mode = "manual"

# Commands and MCP tools that count as external side effects.
# Setting a list here replaces the built-in defaults.
# [external_actions]
# shell = ['^git\s+push\b']
# mcp_tools = ['^mcp__.*__(send|post|publish|create|update|delete)']
`
