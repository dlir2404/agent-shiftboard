# Phase 0 spike — findings

Date: 2026-11-11. Machine: macOS (darwin/arm64), Go 1.27.1.

Scope of this pass: installed versions and documented flags only (`--help`, `--version`, feature lists). No model session was started, so nothing here consumed provider quota. Live checks (hooks firing, resume behaviour, approval surfacing, tmux lifetime) are listed under [Still to validate live](#still-to-validate-live).

## Installed tools

| Tool | Version | Notes |
|---|---|---|
| `claude` | 2.1.322 (Claude Code) | `~/.local/bin/claude` |
| `codex` | codex-cli 0.167.1 | via mise shim |
| `agy` | 1.4.10 | `~/.local/bin/agy` |
| `hermes` | v0.21.2 | `~/.local/bin/hermes` |
| `tmux` | **not installed** | Homebrew offers 3.8. Required by D-14/D-15; install with `brew install tmux` (user action). |
| `git`, `sqlite3` | system | |

## Claude Code (2.1.322)

Supports what D-14 needs, from `claude --help`:

- `--session-id <uuid>` — pre-assigned session ID (must be a valid UUID).
- `-r, --resume [id]` — resume by ID or name. `--fork-session` creates a new ID when resuming.
- `--settings <file-or-json>` — **per-session settings**, so AgentShiftboard can inject its hooks without editing the user's settings files.
- `--mcp-config <configs...>` — per-session MCP servers, so the AgentShiftboard MCP server can be added per session.
- `--append-system-prompt <prompt>` — a channel for the startup brief and resource index.
- `--permission-mode` choices: `acceptEdits`, `auto`, `bypassPermissions`, `manual`, `dontAsk`, `plan`. AgentShiftboard never passes `bypassPermissions` or `--dangerously-skip-permissions` by default.
- Headless: `-p/--print`, `--output-format stream-json`, `--input-format stream-json`, `--include-hook-events`, `--permission-prompts host|none`.
- `--bare` skips hooks; AgentShiftboard must not use it for managed sessions, because status and intent enforcement depend on hooks.
- `--bg` and `claude attach` exist (a background-session feature of the CLI itself). Not used; tmux stays the host (D-14).

Design consequence: launch the lead as
`claude --session-id <uuid> --settings <generated hooks json> --mcp-config <generated mcp json> [--append-system-prompt <launch brief>]`, inside a tmux window whose environment carries `AGENT_SHIFTBOARD_SESSION_ID`.

## Codex CLI (0.167.1)

- Interactive by default; `[PROMPT]` positional starts the session with an initial prompt.
- `codex resume [SESSION_ID] [PROMPT]` — resume by UUID or name; `codex fork` exists.
- `-c key=value` overrides any `~/.codex/config.toml` value per invocation — the channel for per-session hooks/notify and MCP server configuration without editing the user's config.
- `codex features list` reports `hooks  stable  true`. The current official hook reference documents `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PermissionRequest`, `PreCompact`, `Stop`, and `Interrupt`; hook input includes `session_id`, and `SessionStart` includes a `source` value (`startup`, `resume`, `clear`, `compact`, or `fork`).
- `SessionStart` and `UserPromptSubmit` accept `hookSpecificOutput.additionalContext`; Codex adds it as extra developer context. The handler injects the launch brief at startup and retries it on the first trusted user-prompt hook if startup trust review delayed injection, without duplicating it. Compaction re-injects the current brief; no transcript is read.
- `PreToolUse` receives `tool_name` and `tool_input`, and can deny with `hookSpecificOutput.permissionDecision="deny"`, matching the shared handler's existing response shape. `PermissionRequest` can observe normal prompts; the adapter does not decide or auto-approve them.
- Codex requires a user to review and trust each new or changed non-managed hook through `/hooks`; untrusted hooks are skipped. AgentShiftboard must not pass `--dangerously-bypass-hook-trust`.
- MCP config supports a per-server stdio `command`, `args`, `cwd`, `env`, `enabled_tools`, and `default_tools_approval_mode`. The adapter sets `default_tools_approval_mode = "approve"` (valid values: `auto`, `prompt`, `writes`, `approve`; `auto` still prompts) together with its explicit AgentShiftboard agent-operation allowlist in `enabled_tools`; Codex's other permission and sandbox settings remain native.
- Approval: `-a/--ask-for-approval <policy>`, `-s/--sandbox <mode>`. The current config reference says `on-request` is the interactive approval policy, `never` is non-interactive, and `untrusted` is unsupported. AgentShiftboard only maps the explicit `on-request` setting; it leaves the sandbox native. Dangerous flags exist (`--dangerously-bypass-approvals-and-sandbox`, `--approve-for-me`, `--dangerously-bypass-hook-trust`); AgentShiftboard never passes them by default.
- `--no-alt-screen` may help pane classification in tmux.
- There is no documented flag to pre-assign a session ID. The adapter captures it from the hook payload and resumes with `codex resume <id>`.

## Antigravity CLI `agy` (1.4.10, Phase 5)

- `--conversation <id>` resume, `-c/--continue`, `-p/--print`, `--output-format text|json|stream-json`, `--input-format stream-json`, `-i/--prompt-interactive` (interactive with initial prompt).
- `--settings` — per-session settings JSON, never saved.
- `--mode accept-edits|auto|plan`; `--dangerously-skip-permissions` exists and is never used by default.

## Hermes (v0.21.2, Phase 5)

- `--resume SESSION`, `--continue`, `-z PROMPT` (one-shot), `--pass-session-id`, `--accept-hooks`, a `hooks` subcommand, and an `approvals` subcommand.
- `--yolo` exists and is never used by default.

## Decisions taken from this pass

- Per-session injection (`--settings`/`--mcp-config` for Claude, `-c` for Codex, `--settings` for agy) means **AgentShiftboard never edits the user's global CLI configuration** to install hooks or its MCP server.
- Claude Code is the first adapter for Phase 2: it has a pre-assigned session ID and per-session settings documented today. Codex follows in Phase 3, after its session-ID capture is validated.

## Still to validate live

Needs tmux and a few short model turns, so the user should be present:

1. Claude: hooks fire with the expected payloads in an interactive tmux pane; `PermissionRequest`/`Notification` arrive when a prompt shows; `--resume` restores a session after the pane is killed.
2. Codex: validate that command-line hook overrides trigger with expected payloads, that `/hooks` trust review is clear to the user, that the launch brief arrives before the initial turn, that the MCP allowlist works, and that `codex resume <id>` restores the same native thread. No live Codex session has been started for this spike.
3. tmux: server lifetime across terminal close, sleep, and logout on macOS; `swap-pane`/`respawn-pane` behaviour with a running CLI.
4. Claude allowlist behavior and Codex's per-server MCP approval behavior in an interactive session, while confirming all non-AgentShiftboard permissions still use Codex's native settings.
