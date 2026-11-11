# AgentShiftboard

> A local-first task board and control plane for independent coding-agent CLI sessions.

AgentShiftboard is planned as one terminal interface for managing tasks across Claude Code, Codex CLI, Google Antigravity CLI (`agy`), Hermes Agent CLI (`hermes`), and later other coding-agent CLIs. Each agent remains an independent native process with its own provider configuration and native session. The tool manages the work around those sessions: task assignment, lead selection, compact task state, handoff, and visibility.

The durable memory is a concise task brief authored and maintained by the lead agent, backed by structured records for rules, approvals, side effects, and shared resources. AgentShiftboard never stores or reads chat transcripts as task memory.

## Status

Early implementation (branch `feat/phase-0-1`). Phase 1 (state contract, CLI, MCP server, status board) is in place. The Claude Code adapter and tmux workspace are implemented and partly validated live (launch in the visible main slot, folder-trust prompt, exit menu); hooks and resume are still to be validated. The Codex interactive adapter is now implemented against the current documented hook and config interfaces; live hook trust, launch-context delivery, MCP approval, and resume remain unvalidated. See [Phase 0 findings](docs/SPIKE-PHASE0.md#still-to-validate-live).

## Quick start

```sh
brew install tmux
go build -o ~/.local/bin/agent-shiftboard ./cmd/agent-shiftboard   # any directory on PATH
cd your-project
agent-shiftboard
```

The first run initializes `.agent-shiftboard/` (add it to `.gitignore`) and opens straight into Claude Code as the lead of a new task: just talk to it. Later runs open the workspace with the session list on the left and the selected session's agent on the right. Each session (a line of work, such as a feature) shows its work items with live status (mirrored from the agents' own todo lists), the agents working on it, and what needs you. Each session is one line in the list (progress, ◐ for things that need you); `space` opens its actions panel (overview, checklist, agents, needs you, switch lead CLI). `↑↓` then `→` or `Enter` shows a session's lead on the right and moves the keyboard there; `Shift+←` / `Shift+→` move between the list and the session; `n` starts a new task, `s` switches a task's lead to another CLI; `Shift+↓` jumps from anywhere to what needs you (an agent's prompt, or the Needs you panel), and the status line shows how many things need you; `Ctrl+Q` leaves (sessions keep running). The workspace uses its own tmux server and config, so your tmux setup is untouched.

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

