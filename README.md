# AgentShiftboard

**One terminal workspace for running coding agents (Claude Code, Codex, …) as a team — with shared task memory that survives quotas, compactions, closed terminals, and hand-offs between CLIs.**

AgentShiftboard runs the official coding-agent CLIs you already use, each as its own native session, and keeps the work around them in one place: what you are building, the steps and their status, the rules you set, what is waiting for your approval, and what has already been done to the outside world. A lead agent can hand parts of the work to other agents — of the same or another CLI — and when a lead stops, a fresh session (or another CLI) picks up from the same state instead of from scratch.

> **Status: alpha.** Claude Code and Codex are supported on macOS. Expect rough edges and breaking changes. See [Status and roadmap](#status-and-roadmap).

```text
┌ AgentShiftboard · my-app ─────────┬──────────────────────────────────────────────┐
│ ● 1 running   ◐ 1 need you        │                                              │
│                                   │   Claude Code (the lead of #4) runs here:    │
│ SESSIONS                          │   you talk to it as usual                    │
│ ● #4 Add OAuth login              │                                              │
│   ▰▰▰▰▱▱ 4/6 · claude ● codex ◐ ◐1│                                              │
│ ○ #2 Fix flaky cache test         │                                              │
│   ▰▰▰▰▰▰ 3/3 · claude ○           │                                              │
│ ───────────────────────────────── │                                              │
│ #4 Add OAuth login                │                                              │
│ NEXT                              │                                              │
│   Review Codex's provider config  │                                              │
│ AGENTS                            │                                              │
│   ● claude lead     running       │                                              │
│   ◐ codex worker A-2 asks approval│                                              │
│ space actions  ⏎ open lead        │                                              │
└───────────────────────────────────┴──────────────────────────────────────────────┘
 ◐ 1 need you · ⇧↓ open · ⇧← list · ⇧→ session · Ctrl+Q leave
```

## Why

Long agent sessions fail in predictable ways:

- **A quota runs out mid-task.** Did the push already happen? What was the next step?
- **Context gets compacted.** The agent forgets "the dev database is read-only" or "always ask before pushing".
- **Approvals get lost.** A successor treats an earlier "ok" as permission for something else.
- **Work is trapped in one CLI.** Helper scripts and progress live in one tool's private folders; another tool starts over.

AgentShiftboard keeps that state outside any single agent, in a small local store every session reads and writes through the same interface.

## Features

- **Native CLIs, side by side.** Claude Code and Codex run as their own interactive sessions in a tmux workspace — slash commands, skills, and permission prompts work as usual.
- **Sessions and live checklists.** Each line of work (a *session*) shows its steps with status. Steps are mirrored automatically from the agents' own todo/plan tools (Claude `TodoWrite`/`TaskCreate`/`TaskUpdate`, Codex `update_plan`), plus anything agents record explicitly.
- **Delegation between CLIs.** The lead creates an assignment and starts a worker in any supported CLI. The worker submits a result, the lead is notified and reviews it, and the finished worker is closed.
- **Lead hand-off.** Switch a session's lead to another CLI; the new lead reads the brief, the rules, and open items, and must acknowledge them before it can act.
- **Rules and approvals that stick.** Durable user rules (user, project, or task scope), approvals scoped to an exact action and target, and time-bounded delegations — kept as structured records, not chat history.
- **Side-effect journal.** External actions (git push, deploys, DB writes, messages) are recorded before and after. A hook blocks unrecorded ones; outcomes cut off by a crash become `unknown` so nobody repeats them blindly.
- **Shared resources.** Config values, secret *references* (never values), and reusable tools in a provider-neutral registry, so a script one agent wrote is available to all.
- **One key to what needs you.** The status line counts what is waiting for you; `Shift+↓` jumps straight to it.

## How it works

```text
 you ── tmux workspace (own server) ──┬── session list (AgentShiftboard)
                                      ├── claude  (lead)      ─┐ hooks + MCP
                                      └── codex   (worker)    ─┤
                                                               ▼
                         agent-shiftboard CLI / MCP server ── coordinator ── SQLite
                                                                        (.agent-shiftboard/state.db)
```

- **Coordinator.** Core state logic over a local SQLite database. It is not a daemon: it runs inside whichever process acts — a CLI call, the MCP server, a hook, or the session list.
- **Adapters.** Provider-specific launch, resume, hook, and status mapping live behind one interface. Each session gets its hooks and MCP server through per-invocation settings (`claude --settings/--mcp-config`, `codex -c …`); your own CLI configuration files are never edited.
- **Brief.** The lead keeps a concise brief (goal, decisions, done, next action) through the same operations. It is what a fresh session reads — never a transcript.
- **tmux.** A dedicated tmux server (`-L agent-shiftboard`) with its own config hosts the sessions, so they keep running when you close the terminal. Your tmux setup is untouched.

The full design is in [docs/](docs/): the [plan](docs/PLAN.md), the [state and handoff contract](docs/STATE-CONTRACT.md), and the [decision log](docs/DECISIONS.md).

## Requirements

- macOS (Linux is expected to work but is untested; Windows is not supported)
- [tmux](https://github.com/tmux/tmux) 3.3 or later
- Go 1.27 or later (to build from source)
- At least one supported CLI, installed and logged in:
  - [Claude Code](https://code.claude.com) — `claude`
  - [Codex CLI](https://developers.openai.com/codex) — `codex`

## Installation

From source:

```sh
git clone https://github.com/dlir2404/agent-shiftboard.git
cd agent-shiftboard
go build -o ~/.local/bin/agent-shiftboard ./cmd/agent-shiftboard   # any directory on your PATH
```

Or with `go install`:

```sh
go install github.com/dlir2404/agent-shiftboard/cmd/agent-shiftboard@latest
```

Then check your setup (no model calls are made):

```sh
agent-shiftboard doctor
```

## Quick start

```sh
cd your-project
agent-shiftboard
```

The first run creates `.agent-shiftboard/` (add it to `.gitignore`) and drops you straight into Claude Code as the lead of a new session. Just tell it what you want; it names the session, keeps the checklist moving, and records rules and approvals as you go.

Run `agent-shiftboard` again any time to come back. Sessions keep running in the background.

## Using the workspace

| Where | Key | Action |
|---|---|---|
| anywhere | `Shift+↓` | Jump to what needs you (an agent's prompt, or the Needs you panel) |
| anywhere | `Shift+←` / `Shift+→` | Move between the session list and the session on the right |
| anywhere | `Ctrl+Q` | Leave the workspace (sessions keep running) |
| list | `↑` `↓` | Select a session |
| list | `→` / `Enter` | Show the session's lead on the right (resume it if it exited) |
| list | `Space` | Open the session's actions panel |
| list | `n` | Start a new session |
| list | `/` | Find a session |
| actions panel | `↑` `↓` `→` `←` `Esc` | Navigate: Overview, Checklist, Agents, Needs you, Open the lead, Switch lead CLI |
| Needs you | `a` / `x` / `c` / `v` | Approve, deny, confirm, revoke |

When a lead's CLI exits, its pane offers to resume it, switch to another CLI, start a new session, or open another one.

Talking to the lead is enough for most things — for example *"give the README to Codex"*, *"always ask me before pushing"*, or *"you can decide small product questions until tomorrow morning"* (recorded as a delegation you confirm in the list).

## Configuration

Policy lives in `.agent-shiftboard/config.toml` (project) and `~/.config/agent-shiftboard/config.toml` (user). Only you edit these files; agents cannot change them.

```toml
# Approvals you give in chat may be recorded by the lead ("allow"),
# or must be given in the list ("board_only").
conversation_approvals = "allow"

lead_priority = ["claude", "codex"]   # CLI for new leads
wake_lead_on_results = true            # tell an idle lead when a worker finishes
close_workers_when_done = true         # close a worker once its result is accepted

[providers.claude]
# permission_mode = "manual"           # passed to the CLI; skip-permission modes are refused

# Commands and MCP tools that count as external side effects.
[external_actions]
shell = ['^git\s+push\b', '^kubectl\s+(apply|delete|rollout)\b']
mcp_tools = ['^mcp__.*__(send|post|publish)']
```

See the sample written by `agent-shiftboard init` for all options.

### Files and data

| Path | Contents |
|---|---|
| `.agent-shiftboard/state.db` | Sessions, briefs, rules, approvals, checklists, side effects (per project) |
| `.agent-shiftboard/config.toml` | Project policy |
| `.agent-shiftboard/tools/` | Shared helper scripts registered by agents |
| `~/.config/agent-shiftboard/` | User-scope rules and resources, user policy, tmux config |
| `~/.local/state/agent-shiftboard/` | Generated launch scripts, per-session CLI settings, rendered briefs |

Everything is local. Nothing is sent anywhere by AgentShiftboard itself.

## Command line

Agents use these through their shell or the built-in MCP server; you rarely need them yourself.

```text
agent-shiftboard                       open the workspace
agent-shiftboard doctor                detect installed CLIs and tmux
agent-shiftboard task list|show        sessions (internally "tasks")
agent-shiftboard brief show            the session brief
agent-shiftboard item list|add|set     checklist items
agent-shiftboard rule list|add         durable rules (confirm/revoke: user only)
agent-shiftboard pending list|add      questions and approvals (approve/deny: user only)
agent-shiftboard effect plan|start|finish|reconcile
agent-shiftboard delegate / worker start <A-id> --provider codex
agent-shiftboard lead new|switch|resume <task>
agent-shiftboard tool|config|secret ref|lock …
agent-shiftboard mcp                   MCP server (stdio) with read and agent operations
```

Run `agent-shiftboard <command> --help` for details.

## Safety model

- **Official CLIs only.** No API proxy, no credential handling, no account switching to get around limits.
- **Your permission settings stay in charge.** Each CLI keeps its own approval prompts. AgentShiftboard never answers a CLI's prompt, never types into a pane that is waiting for approval, and never enables a skip-permissions mode.
- **User operations are separate.** Confirming rules and delegations, approving from the list, breaking locks, and switching leads are refused inside agent sessions (detected by environment and process ancestry) and are not exposed over MCP.
- **No transcripts.** Task memory is the brief and structured records. Provider transcript files are never read; hooks only see tool-call arguments.
- **No secret values.** Writes are scanned for credential-like strings; agents record where a secret lives, not what it is.

These rules are enforced in code and described in [AGENTS.md](AGENTS.md) and the [state contract](docs/STATE-CONTRACT.md).

## Status and roadmap

Working today:

- Claude Code and Codex adapters (lead and worker), tmux workspace, session list and actions panel
- Briefs, checklists mirrored from agent todo tools, rules, approvals, delegations, side-effect journal, shared resources, advisory locks
- Delegation with result notification, lead switch between CLIs with readback acknowledgement

Planned:

- Opt-in automatic failover to another CLI when a lead hits its limit
- Google Antigravity (`agy`) and Hermes adapters
- Retention for old brief revisions and events
- Packaging (Homebrew tap, release binaries) and Linux validation

Known limitations are tracked in [docs/SPIKE-PHASE0.md](docs/SPIKE-PHASE0.md) and the open items in [docs/REVIEW-2.md](docs/REVIEW-2.md).

## Related projects

[Agent Deck](https://github.com/asheshgoplani/agent-deck), [CarryCtx](https://github.com/Xuepoo/carryctx), [ReevesAgents](https://github.com/mertkayacs/reevesagents), and [mu](https://github.com/mu-crew/mu) cover parts of this space. See [docs/LANDSCAPE.md](docs/LANDSCAPE.md) for how AgentShiftboard differs.

## Contributing

Issues and pull requests are welcome. Before changing architecture or persistence, read [AGENTS.md](AGENTS.md), the [plan](docs/PLAN.md), the [state contract](docs/STATE-CONTRACT.md), and the [decision log](docs/DECISIONS.md); record new product decisions in the decision log.

```sh
go build ./...
go vet ./...
go test ./...
```

## License

No license has been chosen yet. Until one is added, all rights are reserved by the author.
