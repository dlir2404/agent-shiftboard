<div align="center">

# AgentShiftboard

**Run Claude Code, Codex, and other coding agents as one team — in one terminal, with shared memory that outlives any single session.**

![status: alpha](https://img.shields.io/badge/status-alpha-orange)
![platform: macOS](https://img.shields.io/badge/platform-macOS-lightgrey)
![go 1.27+](https://img.shields.io/badge/go-1.27%2B-00ADD8)
![agents: Claude Code · Codex](https://img.shields.io/badge/agents-Claude%20Code%20%C2%B7%20Codex-blueviolet)
[![license: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Support on Ko-fi](https://img.shields.io/badge/Ko--fi-support-FF5E5B?logo=kofi&logoColor=white)](https://ko-fi.com/dlir2404)

[About](#about) · [Why](#why-agentshiftboard) · [Install](#platforms-and-installation) · [Quick start](#quick-start) · [How it works](#how-it-works) · [Docs](docs/)

</div>

```text
┌ AgentShiftboard · my-app ─────────┬──────────────────────────────────────────────┐
│ ● 1 running   ◐ 1 need you        │                                              │
│                                   │   Claude Code, the lead of #4, runs here.    │
│ SESSIONS                          │   You talk to it as usual.                   │
│ ● #4 Add OAuth login              │                                              │
│   ▰▰▰▰▱▱ 4/6 · claude ● codex ◐ ◐1│                                              │
│ ○ #2 Fix flaky cache test         │                                              │
│   ▰▰▰▰▰▰ 3/3 · claude ○           │                                              │
│ ───────────────────────────────── │                                              │
│ #4 Add OAuth login                │                                              │
│ NEXT  Review Codex's provider     │                                              │
│       config, then run the tests  │                                              │
│ AGENTS                            │                                              │
│   ● claude lead      running      │                                              │
│   ◐ codex worker A-2 asks approval│                                              │
└───────────────────────────────────┴──────────────────────────────────────────────┘
 ◐ 1 need you · ⇧↓ open · ⇧↑ actions · ⇧← list · ⇧→ session · Ctrl+Q leave
```

## About

AgentShiftboard is what I wanted from working with several coding agents at once: keep using the real CLIs, let them hand work to each other, and never lose track of what was decided, what is done, and what is waiting for me.

It runs the official CLIs you already have — Claude Code and Codex today — as native interactive sessions in a tmux workspace. Around them it keeps one small local store per project: the sessions you are working on, each one's checklist and brief, the rules you set, what needs your approval, and every action that touched the outside world. A lead agent can delegate to a worker in another CLI; when a lead stops, a fresh session or a different CLI continues from that store instead of from scratch.

## Why AgentShiftboard

Tools for working with coding agents today fall into three groups:

- **Session managers** — Agent Deck, ReevesAgents, Claude Code's own agent view. Many sessions in one place, but each session's knowledge stays inside its own CLI and conversation.
- **Context stores** — CarryCtx, Carryover. Persistent task state or hand-off notes, but no runtime that drives the agents, or memory rebuilt from transcripts.
- **Built-in subagents** — delegation inside one vendor's CLI only, gone when the parent session ends.

Long sessions then fail the same ways: a quota runs out mid-push and nobody knows whether it happened; a compaction forgets *"the dev database is read-only"*; a successor treats an old "ok" as approval for something else; a helper script one agent wrote is invisible to the next.

AgentShiftboard is the missing fourth: native CLIs from different vendors working as one team, on state that outlives any single session — structured, local, and never read from transcripts.

## Platforms and installation

| Platform | Status | Install |
|---|---|---|
| macOS | Alpha | `go install github.com/dlir2404/agent-shiftboard/cmd/agent-shiftboard@latest` |
| Linux | Untested | Build from source |
| Windows | Not supported | — |

| Agent CLI | Status | Roles |
|---|---|---|
| [Claude Code](https://code.claude.com) | Supported | lead, worker |
| [Codex CLI](https://developers.openai.com/codex) | Supported | lead, worker |
| Google Antigravity (`agy`) | Planned | — |
| [Hermes Agent](https://github.com/NousResearch/hermes-agent) (`hermes acp`) | Alpha | worker |

**Requirements:** [tmux](https://github.com/tmux/tmux) 3.3+, Go 1.27+ to build, and at least one supported CLI installed and logged in.

From source:

```sh
git clone https://github.com/dlir2404/agent-shiftboard.git
cd agent-shiftboard
go build -o ~/.local/bin/agent-shiftboard ./cmd/agent-shiftboard   # any directory on your PATH
agent-shiftboard doctor                                            # checks CLIs and tmux; no model calls
```

## Quick start

```sh
cd your-project
agent-shiftboard
```

The first run creates `.agent-shiftboard/` (add it to `.gitignore`) and opens straight into Claude Code as the lead of a new session. Tell it what you want. It names the session, keeps the checklist moving, and records your rules and approvals as you go — *"always ask me before pushing"*, *"give the README to Codex"*, *"you can decide small product questions until tomorrow morning"*.

Run `agent-shiftboard` again to come back. Sessions keep running when you leave.

## Features

- **Native CLIs, side by side** — slash commands, skills, and each CLI's own permission prompts work as usual.
- **Sessions with live checklists** — steps mirrored from the agents' own todo and plan tools, plus anything they record explicitly.
- **Delegation across CLIs** — the lead hands an assignment to a Claude or Codex worker, is notified when the result is in, reviews it; finished workers close themselves.
- **Messages between sessions** — any session can send a short note to the lead, a worker, or a former lead still open (`message send`, or MCP `shiftboard_message_send`); the board types it in only when that session is idle and you are not typing there.
- **Lead hand-off** — switch a session's lead to another CLI; the new lead must read back the rules and open items before it can act.
- **Rules, approvals, delegations** — durable rules by scope, approvals bound to an exact action and target, time-limited delegations you confirm.
- **Side-effect journal** — pushes, deploys, and other external actions are recorded before and after; unrecorded ones are blocked; outcomes cut off by a crash become `unknown` instead of being repeated.
- **Shared resources** — config, secret *references*, and reusable tools in one registry every CLI can use.
- **One key to what needs you** — the status line counts it, `Shift+↓` takes you there.

## How it works

```text
 you ── tmux workspace (own server) ──┬── session list
                                      ├── claude (lead)    ─┐ hooks + MCP
                                      └── codex  (worker)  ─┤
                                                            ▼
                    agent-shiftboard CLI / MCP server ── coordinator ── SQLite
```

- **Coordinator** — state logic over a per-project SQLite database. Not a daemon: it runs inside whichever process acts.
- **Adapters** — launch, resume, hooks, and status per CLI, injected per session (`claude --settings`, `codex -c`). Your CLI config files are never edited.
- **Brief and records** — what a fresh session reads: goal, decisions, done, next action, rules, open items. Never a transcript.
- **tmux** — a dedicated server with its own config hosts the sessions, so they survive closing the terminal and leave your tmux setup alone.

Design documents: [plan](docs/PLAN.md) · [state and hand-off contract](docs/STATE-CONTRACT.md) · [decision log](docs/DECISIONS.md) · [landscape](docs/LANDSCAPE.md).

## Keys

| Where | Key | Action |
|---|---|---|
| anywhere | `Shift+↓` | Jump to what needs you |
| anywhere | `Shift+←` / `Shift+→` | Session list / the session on the right |
| anywhere | `Ctrl+Q` | Leave (sessions keep running) |
| list | `↑` `↓`, `→` / `Enter` | Select; show the session's lead (resume it if it exited) |
| list | `Space` | Actions: overview, checklist, agents, needs you, switch lead CLI |
| list | `n` · `/` | New session · find |
| needs you | `a` `x` `c` `v` | Approve · deny · confirm · revoke |

## Configuration

Policy lives in `.agent-shiftboard/config.toml` and `~/.config/agent-shiftboard/config.toml`. Only you edit these files; agents cannot.

```toml
conversation_approvals = "allow"   # or "board_only": approvals only from the session list
lead_priority = ["claude", "codex"]
wake_lead_on_results = true        # tell an idle lead when a worker finishes
close_workers_when_done = true     # close a worker once its result is accepted

[external_actions]                 # what counts as a side effect
shell = ['^git\s+push\b', '^kubectl\s+(apply|delete|rollout)\b']
```

All data stays on your machine: `.agent-shiftboard/` in the project, `~/.config/agent-shiftboard/`, and generated files in `~/.local/state/agent-shiftboard/`.

## Safety

- **Official CLIs only.** No API proxy, no credential handling, no account switching around limits.
- **Your permission settings stay in charge.** AgentShiftboard never answers a CLI's permission prompt, never types into a pane waiting for approval, and never enables a skip-permissions mode.
- **User decisions stay with the user.** Confirming rules, approving from the list, and switching leads are refused inside agent sessions and are not exposed to agents.
- **No transcripts, no secret values.** Memory is the brief and structured records; secrets are stored only as references.

Details: [AGENTS.md](AGENTS.md) and the [state contract](docs/STATE-CONTRACT.md).

## Roadmap

- [x] Claude Code and Codex as lead and worker
- [x] Session list, actions panel, live checklists
- [x] Delegation with result notification; lead switch between CLIs
- [ ] Opt-in automatic failover when a lead hits its limit
- [x] Hermes as a worker over ACP
- [ ] Hermes as lead; Antigravity adapter
- [ ] Retention for old brief revisions and events
- [ ] Homebrew tap and release binaries; Linux validation

## Contributing

Issues and pull requests are welcome. Before changing architecture or persistence, read [AGENTS.md](AGENTS.md) and the [decision log](docs/DECISIONS.md), and record new product decisions there.

```sh
go build ./... && go vet ./... && go test ./...
```

## Support

AgentShiftboard is free and open source. If it saves you time, you can support its development:

[![Support me on Ko-fi](https://ko-fi.com/img/githubbutton_sm.svg)](https://ko-fi.com/dlir2404)

Bug reports, ideas, and pull requests help just as much.

## Security

Please report vulnerabilities privately through [GitHub security advisories](https://github.com/dlir2404/agent-shiftboard/security/advisories/new) rather than public issues.

## License

Copyright 2026 dlir2404.

Licensed under the [Apache License, Version 2.0](LICENSE). Contributions are accepted under the same license.
