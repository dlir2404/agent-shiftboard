# AgentShiftboard

> A local-first task board and control plane for independent coding-agent CLI sessions.

AgentShiftboard is planned as one terminal interface for managing tasks across Claude Code, Codex CLI, Google Antigravity CLI (`agy`), Hermes Agent CLI (`hermes`), and later other coding-agent CLIs. Each agent remains an independent native process with its own provider configuration and native session. The tool manages the work around those sessions: task assignment, lead selection, compact task state, handoff, and visibility.

The durable memory is a concise task brief authored and maintained by the lead agent, backed by structured records for rules, approvals, side effects, and shared resources. AgentShiftboard never stores or reads chat transcripts as task memory.

## Status

Early implementation (branch `feat/phase-0-1`). Phase 1 (state contract, CLI, MCP server, status board) is in place with tests. The Phase 2 Claude Code adapter and tmux workspace are implemented and partly validated live (launch in the visible main slot, folder-trust prompt, exit menu); hooks and resume are still to be validated; see [Phase 0 findings](docs/SPIKE-PHASE0.md#still-to-validate-live).

## Quick start

```sh
brew install tmux
go build -o ~/.local/bin/agent-shiftboard ./cmd/agent-shiftboard   # any directory on PATH
cd your-project
agent-shiftboard                        # or: agent-shiftboard "add an LRU cache to the API"
```

The first run initializes `.agent-shiftboard/` (add it to `.gitignore`), opens a tmux workspace with the lead on the left and the status board on the right, asks for a task, and starts Claude Code as the lead in that pane. When the CLI exits, the pane offers resume, a new session, or another task. Without tmux, the lead runs in the current terminal and `agent-shiftboard board` shows the board in another terminal.

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

