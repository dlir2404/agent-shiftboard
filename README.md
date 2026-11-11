# AgentShiftboard

> A local-first task board and control plane for independent coding-agent CLI sessions.

AgentShiftboard is planned as one terminal interface for managing tasks across Claude Code, Codex CLI, Google Antigravity CLI (`agy`), Hermes Agent CLI (`hermes`), and later other coding-agent CLIs. Each agent remains an independent native process with its own provider configuration and native session. The tool manages the work around those sessions: task assignment, lead selection, compact task state, handoff, and visibility.

The durable memory is a concise task brief authored and maintained by the lead agent, backed by structured records for rules, approvals, side effects, and shared resources. AgentShiftboard never stores or reads chat transcripts as task memory.

## Status

Early implementation (branch `feat/phase-0-1`). Phase 1 (state contract, CLI, MCP server, status board) is in place with tests. The Phase 2 Claude Code adapter and tmux workspace are implemented but not yet validated against a live session; see [Phase 0 findings](docs/SPIKE-PHASE0.md#still-to-validate-live).

## Quick start

```sh
go build -o bin/agent-shiftboard ./cmd/agent-shiftboard   # put bin/ on PATH
agent-shiftboard init            # in your project root; add .agent-shiftboard/ to .gitignore
agent-shiftboard doctor          # detects claude, codex, agy, hermes, tmux (no model calls)
agent-shiftboard run "implement cache" --goal "add an LRU cache to the API"
agent-shiftboard lead new 1      # needs tmux: starts Claude Code in the workspace
agent-shiftboard                 # attach to the workspace: lead CLI + status board
```

Without tmux, `agent-shiftboard board` shows the status board and all state commands work.

## Product invariants

- Launch the installed vendor CLI or its documented local session interface. Do not proxy inference requests or copy provider credentials.
- A lead or worker is an independent CLI session, not a vendor subagent.
- Store task facts, decisions, progress, blockers, and next actions. Do not store the full conversation as the project's memory.
- A new lead receives the task brief, worktree state, and relevant worker results. A different vendor cannot resume another vendor's native session.
- Treat quota, authentication, approval, network, context-window, and process failures as different states. Never rotate accounts to evade a provider limit.
- Keep the human in control of permission modes, provider priority, and automatic failover.

## Planning documents

- [Product and implementation plan](docs/PLAN.md)
- [Task state and handoff contract](docs/STATE-CONTRACT.md)
- [Key decisions and rationale](docs/DECISIONS.md)
- [Landscape and differentiation](docs/LANDSCAPE.md)
- [Review field notes](docs/REVIEW-FIELD-NOTES.md) and [follow-up review](docs/REVIEW-2.md) — review input, not decisions

