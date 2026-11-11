# Landscape and differentiation

This is a working landscape scan, not an exhaustive market survey. The repositories below show that several individual parts—and a few broad combinations—already exist. The project must not be described as the first multi-agent CLI manager or the first compact handoff store.

## Closest overlaps

| Project | Documented overlap | Documented boundary or distinction |
|---|---|---|
| [CarryCtx](https://github.com/Xuepoo/carryctx) | Local SQLite state for projects, tasks, sessions, checkpoints, teams, decisions, worktrees, and audited handoffs. It can render concise context for an agent. | Its README describes it as a persistence/control layer, not an agent runtime: an external harness launches processes, schedules work, routes prompts, retries, and selects models. Its documented Commander skill dispatches subagents. |
| [Agent Deck](https://github.com/asheshgoplani/agent-deck) | Terminal session manager and TUI for Claude Code, Gemini, OpenCode, Codex, Hermes, and other coding agents. Native CLIs run in tmux; status comes from hooks with a pane-content fallback; session IDs are pre-assigned. Parent/child sessions, `session send`, and "conductor" sessions with their own `state.json`, `task-log.md`, and inbox. | Conductors overlap with a lead-maintained task state, but the docs do not describe a scoped state contract (rules, approvals, side-effect receipts), reconciliation, or cross-provider lead succession. AgentShiftboard builds its own runtime and borrows its tmux/status patterns (D-13). |
| [ReevesAgents](https://github.com/mertkayacs/reevesagents) | Runs real coding-agent CLIs side by side in tmux; an attached agent can spawn, read, steer, and stop others. Provider logins remain in the provider CLIs. | The public description emphasizes a local CLI workspace/session manager. A compact semantic task-state contract and automatic lead succession are not documented as its central workflow. |
| [mu](https://github.com/mu-crew/mu) | Local control plane for parallel agents with a task DAG, isolated workspaces, agent status, handoffs, and a terminal dashboard. | Its described core loop is spawning and coordinating a crew in parallel. The requirements here emphasize a user-facing lead session and replacing that lead with a different native CLI when it becomes unavailable. |
| [Agent Office](https://github.com/Mr-AK-AI/agent-office) | Launches and messages real local CLI agents in isolated tmux rooms, explicitly without an API proxy. | It focuses on terminal rooms and messaging; the durable agent-authored task brief and lead failover contract remain separate concerns. |

The Gemini mention in the Agent Deck row describes that project's provider support only. It is not an AgentShiftboard adapter target; this project's initial Google CLI target is Antigravity (`agy`).

## Adjacent, narrower components

- [A2CR](https://github.com/a2cr/a2cr) stores compact local handoff checkpoints and temporary notes, explicitly separating handoff state from optional supporting notes and raw transcripts. It is an MCP workspace rather than a full native-CLI supervisor.
- [Carryover](https://github.com/carryover-dev/carryover) creates structured handoffs across sessions and tools, but its documented approach captures context from agent transcript files. This project instead makes the lead write the semantic brief and does not use transcript ingestion as its memory system.
- [Switchyard](https://github.com/MohammedAlkindi/Switchyard) coordinates coding agents with isolated worktrees and reports file collisions before merge.
- [TaskCapsule](https://github.com/vtino17/taskcapsule) focuses on pausing and resuming coding tasks with isolated Git worktrees and managed local services.

## Potential product wedge

The most defensible initial position is the intersection of these requirements:

1. One local CLI/TUI controls separate, official provider CLI sessions; it does not proxy inference.
2. One session is the human-facing lead in one fixed window. The other sessions are independent peer CLIs of the same or other providers, not subagents.
3. The lead owns one compact, agent-authored task brief, backed by scoped rules, approvals, delegations, side-effect receipts, and provider-neutral shared resources. Transcripts are never used as task memory.
4. The same contract first carries work across sessions of one provider (compaction, cooldown, closed terminal), then lets the coordinator suspend an unavailable lead, classify why it stopped, select an explicitly configured successor of another provider, and seed its native session from the brief (D-21).
5. Task state, session status, provider-native session IDs, and worktree state stay distinct and visible.

These are a product hypothesis, not a uniqueness claim. Before implementation grows beyond the first vertical slice, compare behavior against CarryCtx, Agent Deck, ReevesAgents, and mu directly. Prefer integration with an existing durable task-state layer over duplicating it if a prototype shows it already meets the state contract.

