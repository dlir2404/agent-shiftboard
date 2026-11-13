# AgentShiftboard — Product and implementation plan

## 1. Product thesis

Build a local-first command-line control plane for several independent coding-agent sessions. The user works in one fixed terminal window with one chosen lead: the lead's native CLI runs beside a status board. The lead can start workers of the same or a different provider and assign them scoped work. A small task board tracks ownership and progress. A shared state contract — concise brief, scoped rules, approvals, delegations, side-effect receipts, and shared resources — lets work continue when a session compacts, cools down, or closes, and lets a configured successor of another provider take over when the lead cannot continue.

Same-provider continuity is built first; workers of any provider and explicit cross-provider lead handoff complete the MVP; opt-in automatic failover follows after it (D-21).

The project manages official CLI processes and documented local session interfaces. It does not issue provider inference requests through a proxy.

The public landscape is already active. See [LANDSCAPE.md](LANDSCAPE.md); particularly, CarryCtx covers much of the local task/handoff layer, while Agent Deck and other projects cover CLI session management. AgentShiftboard builds its own runtime and borrows proven patterns (D-13). The implementation should still validate the state-contract and leader-succession wedge before building a general task database.

## 2. Goals

- Discover supported installed CLIs and report executable/version clearly.
- Let the user select a lead or configure an ordered successor policy.
- Start, resume, interrupt, and monitor native CLI sessions through provider adapters.
- Keep lead and workers as separate CLI processes/sessions, of the same or different providers.
- Create a task board with task status, owner, dependency, worktree, and provider session references.
- Require the lead to maintain the canonical concise task brief through one write path.
- Checkpoint task state throughout work, especially around external and non-idempotent actions, and detect a stale brief.
- Preserve scoped user rules, delegations, unresolved approvals, and side-effect status across sessions and lead changes.
- Share config, secret references, and reusable tools across all providers' sessions.
- Reconcile relevant workspace and external state before a successor repeats an action.
- Check that the successor has the task's required tools and permissions before handoff.
- Track delivery milestones separately from whether the task itself is active or complete.
- Support explicit handoff and controlled failover to another provider.
- Display task, agent, process, approval, and handoff state in one window.
- Keep provider login and permissions inside each installed CLI.

## 3. Non-goals for the first release

- Building a model API gateway, request proxy, credential manager, or account rotator.
- Replacing the vendors' agent loops, tools, or permission systems.
- Treating built-in subagents as the product's agent-to-agent model.
- Persisting full conversations as the task memory layer.
- Distributed orchestration, remote workers, team accounts, or a hosted dashboard.
- Automatically merging concurrent code changes or silently approving risky operations.
- Supporting every coding CLI before the adapter contract is proven.

## 4. Proposed stack

| Area | Choice | Reason |
|---|---|---|
| Language | Go | Produces a straightforward local binary with strong subprocess and terminal support; the coordinator runs inside short-lived CLI, hook, MCP, and TUI processes. |
| CLI commands | Cobra | Clear command tree for project, task, agent, state, resource, and handoff commands (§9). |
| Agent state interface | `agent-shiftboard` state subcommands and an MCP server exposing the same operations: one Streamable HTTP server per project on 127.0.0.1 with a per-session token (D-28), or stdio for a session the user starts by hand | One write path for every provider (D-16). |
| TUI | Bubble Tea with Lip Gloss | Status board beside the lead's native CLI pane, agent activity, and handoff prompts (D-15). |
| Session host | tmux | Hosts native interactive CLIs, keeps them alive across terminal/TUI restarts, and supports the fixed workspace layout and pane swaps (D-14, D-15). |
| Local metadata | SQLite through `modernc.org/sqlite` | Transactional local state without a service or CGO setup. Use migrations and short write transactions. |
| Task brief | Markdown rendered from SQLite records | Readable by people and agents; read-only; revisioned and size-bounded by the coordinator (D-16). |
| Provider events | Interactive sessions: provider hooks/notify mechanisms, then pane classification. Headless workers: each CLI's documented stream (JSON-RPC 2.0 for Codex app-server, stream JSON for Claude and Antigravity) | Preserve native interfaces and normalize only the small event subset the board needs. |
| Process management | tmux for interactive sessions; Go `os/exec` for headless workers, behind a runner interface | Interactive CLIs keep their native terminal; bounded workers get structured output. |
| Config | TOML | User-edited policy (provider profiles, lead priority, permission policy, conversation approvals, thresholds, budgets) and declarative resource manifests. Agents cannot change policy. |

Versions should be selected from supported stable releases when implementation begins. The first release targets macOS; Linux support follows after process and TUI behavior is stable. Windows is out of scope initially.

## 5. High-level architecture

```text
User
  │
  ▼
tmux session per project ── lead's native CLI pane │ Bubble Tea board pane
  │
  ▼
AgentShiftboard entry points (each runs the coordinator in-process)
  ├─ Cobra CLI and state subcommands
  ├─ MCP server (one per project: `mcp --serve` in the hidden window "mcp", HTTP on 127.0.0.1, per-session tokens; stdio for hand-started sessions)
  ├─ provider hook handlers
  └─ Bubble Tea board
  │
  ▼
Coordinator (core state logic) ─────── SQLite
  │                                      ├─ tasks, dependencies, leases
  │                                      ├─ brief revisions
  │                                      ├─ rules, pending items, delegations
  │                                      ├─ side effects, delivery milestones
  │                                      ├─ shared resources and locks
  │                                      └─ sessions, status observations
  │
  ├── Claude adapter ───── official `claude` (interactive / `-p` stream)
  ├── Codex adapter ────── official `codex` TUI / `codex app-server` / `codex exec`
  ├── Antigravity adapter  official Google `agy`
  └── Hermes adapter ───── official Nous `hermes`
          │
          ▼
  tmux windows (interactive lead and workers) · os/exec (headless workers)
```

The **coordinator** is AgentShiftboard's core state logic, not a separate daemon. It runs inside whichever entry point is acting — a CLI or MCP state call, a hook handler, or the board — using short SQLite transactions. tmux owns interactive agent processes, so they outlive the board and the terminal (D-14). The coordinator owns state transitions and lifecycle decisions. Provider adapters own command construction, hook and stream parsing, native session IDs, interrupts, and provider-specific approval/error mapping. The board reads coordinator state; it does not manipulate processes directly.

Task identity, task brief, app run ID, provider CLI session ID, process ID, tmux pane ID, and Git worktree are separate identifiers. Never infer one from another.

## 6. Session and task model

### Local task

- ID, title, user goal, completion criteria, status, priority, dependencies.
- Rules in force (user, project, and task scope) and task-specific required capabilities, each with provenance.
- Delegations granted by the user, with scope, expiry, and decision log.
- Pending questions and approval requests, with exact action/target scope and an explicit unresolved state.
- Side-effect intents and receipts, including target, result status, evidence reference, and rollback/backup reference where available.
- Delivery milestones such as commit, push, review, build, deploy, and verification, each with target and evidence.
- Lead lease state and holder.
- Worker assignments and concise accepted results.
- Worktree path, branch, and last observed repository revision.
- Canonical brief revision pointer.

### Agent session

- Agent instance ID, provider, executable path/version, role, cwd/worktree, tmux pane ID or headless process ID.
- Provider-native session/thread ID when known.
- User-configured opaque billing/auth profile label when available; never infer a raw account identity from credentials.
- Required tools/capabilities and their preflight state (`available`, `missing`, or `unknown`).
- Status and stop/failure reason with observation source and time, as defined in [STATE-CONTRACT.md](STATE-CONTRACT.md#status-vocabularies).
- Permission mode as selected by the user.

### Shared resources

Config values, secret references, and tools at user or project scope, as defined in [STATE-CONTRACT.md](STATE-CONTRACT.md#shared-resources-config-secret-references-and-tools) (D-19). Named advisory locks guard external resources shared by parallel sessions (D-20).

### Persistence boundary

Persist coordinator metadata, brief revisions, rules, delegations, approval state, minimal side-effect receipts, the shared resource registry, and locks. Never ingest or use native transcript files as shared task memory or automatic recovery input. A provider adapter may call that provider CLI's documented native resume operation by session ID; the provider retains and interprets its own session history. Never store secret values; only secret references. Keep diagnostic logs out of canonical context and only save them behind an explicit opt-in with bounded size and retention.

The storage location is project-scoped and local: a dedicated ignored `.agent-shiftboard/` directory created by `agent-shiftboard init`, holding the SQLite database, the user-edited policy file, and machine-local tools. Generated files (launch scripts and contexts, per-session CLI settings, rendered briefs) live in a runtime directory outside the repository (`~/.local/state/agent-shiftboard/<project>-<hash>/`), so agent CLIs do not report them as file changes. User-scope resources live under `~/.config/agent-shiftboard/`. Team-shared tools and manifests may live in a repository path the user configures. Store machine-specific native session IDs and secret-reference locations separately from portable task briefs and manifests. Keep the portable brief near 1,500 tokens as an adjustable target, with Tier 2 records for references and evidence; 1,500 tokens is not a ceiling for evidence pointers. Phase 0 checks whether CarryCtx's state CLI could serve as a backend before committing to a new persistence schema.

## 7. Agent lifecycle and leadership

### Starting a task

1. Detect supported CLI executables and versions without making model calls.
2. Ask the user to choose a lead, or use the explicitly configured provider priority.
3. Create the task record and concise initial brief from the user's request.
4. Start the lead's native CLI in the tmux main slot with a pre-assigned session ID where supported, or resume the selected native session. Pass the brief, rules in force, and resource index at launch.
5. Collect status from hooks/notify, then pane cues, and show it on the board.

### Delegating

1. The lead proposes a task with scope, expected output, shared constraints, required tools, and worktree needs.
2. The coordinator records and validates it and starts another independent CLI session, of the same or a different provider, interactive in tmux or headless for bounded work.
3. The worker receives the current brief, its assignment, rules in force, and the resource index — not the lead's transcript.
4. The worker returns a concise result and evidence. The lead accepts or corrects the task state.
5. The lead closes sessions it no longer needs (`session close`): an idle worker after its assignment is settled, or an idle former lead. Finished workers also close themselves after the lead accepts, rejects, or cancels their work (D-26).

### Messaging between sessions

1. Any session of a task sends a short message to another session of the same task (`message send <to> <text>`, MCP `shiftboard_message_send`); `<to>` is `lead`, an assignment ID, a session ID or unique prefix, or a provider name that matches exactly one other open session.
2. The coordinator queues it as `M-n`. The board delivers it as one typed line, only to an idle pane with no native prompt and never to a session waiting for approval; the body is then cleared (D-25).
3. The recipient replies the same way. A message never carries user authority.

### Same-provider continuity

1. After a cooldown with a known reset time, offer wait-and-resume of the same native session within the user's configured threshold.
2. After a closed terminal, lost tmux host, or crash, reconcile and resume the native session by ID where supported.
3. Resumes keep the same session as lease holder and need no readback ([lease states](STATE-CONTRACT.md#lead-lease-state)).
4. After a context limit, or when resume is not possible, the user can start a fresh session of the same provider (`lead new`). It is a new session: it runs in the main slot (free, since the previous CLI exited), receives the current brief, rules, open items, and resource index at launch, and must give a readback acknowledgement before it holds the lease.
5. Nothing is recovered from transcripts.

### Cross-provider handoff and failover

1. Detect a concrete stop or failure from provider-specific hooks, structured events, exit status, and process/pane state. Do not depend on a final model turn to checkpoint after failure.
2. Classify it with the stop/failure reasons in [STATE-CONTRACT.md](STATE-CONTRACT.md#stop-and-failure-reasons). Keep provider and user-configured billing/auth profile separate from session identity; if the profile is unknown, say so. Do not call an ambiguous error a quota failure.
3. Prefer same-provider continuity where it applies. If the issue is authentication, surface it to the user.
4. Use the latest continuous checkpoint. Open side effects become `unknown` as defined in the state contract; never fill the gap by reading a native transcript. The lease is `suspended`.
5. Reconcile relevant local state before takeover: Git status/revision/diff, coordinator-owned processes, panes, and ports, and task-relevant external milestones when a configured read-only check is available. Record `verified_at`; if a check cannot run, record `unknown`.
6. Preflight the successor's declared tools, shared-tool requirements, secret references, and capabilities. Surface missing or unknown requirements before assigning work. Do not copy credentials or widen permission modes.
7. Start the successor's native session in a hidden window with the brief, open items, resource index, approved artifacts, and reconciled state; the lease becomes `handoff_pending`. Require readback acknowledgement within a timeout. Bound retries and stop for user input if successors fail or are unavailable.
8. After acknowledgement the lease is `held` by the successor. Its pane moves into the main slot when the user confirms the swap; under opt-in automatic failover it works from its hidden window and the board shows a banner until then (D-15).
9. Keep the old session reference and stop reason for audit. Never rotate identities to bypass provider limits.

This is local coordinator policy, not distributed consensus. The database lease prevents two sessions from being current lead for one task.

## 8. Provider adapter plan

### Claude Code

- Detect `claude` on `PATH`; record its version.
- Run the lead interactively in tmux, launched with a pre-assigned `--session-id`; resume with `--resume <id>`. Use documented print/stream output only for headless workers, and normalize their message, tool, completion, interruption, and error events.
- Derive interactive status from hooks first (`UserPromptSubmit`/`PreToolUse` → `running`; `Stop` → `idle`; `PermissionRequest` and permission `Notification` → `waiting_for_approval`; `SessionEnd` → `stopped`), then from pane cues, otherwise `unknown`.
- Use `PreToolUse` to require a side-effect intent for shell commands and MCP tools matching the project's external-action patterns, and hook output to deliver stale-brief nudges.
- Keep existing CLI authentication and project permission policy intact.
- Treat `Stop`, `PreCompact`, and `SessionEnd` as provider-specific aids, not a guaranteed final-save mechanism; API failures and user interrupts follow different events.

### Codex CLI

- Run the native interactive `codex` TUI in tmux for leads and interactive workers. Use `-C` and per-invocation `-c` overrides to attach AgentShiftboard's MCP server and lifecycle hooks without editing `config.toml`.
- Capture Codex's native session ID from the documented `session_id` hook field; Codex does not expose a pre-assigned ID flag. Resume with `codex resume <id>` and the same scoped MCP/hook overrides.
- Use `SessionStart.additionalContext` to deliver the generated launch brief as extra developer context. Do not set `model_instructions_file`, which replaces Codex's built-in instructions, or overwrite the user's `developer_instructions` setting.
- Observe `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PermissionRequest`, `Stop`, `Interrupt`, and `PreCompact`. Enforce external-action intents through `PreToolUse`; do not infer quota failures from unstable model-request error text.
- Codex requires a user to review and trust new or changed non-managed hooks in `/hooks`. Preserve that trust flow and never pass `--dangerously-bypass-hook-trust`.
- Auto-approve only the AgentShiftboard MCP server's explicit read and agent-operation allowlist, which excludes user-only operations. Leave Codex's other native approval and sandbox settings intact; map an explicit `providers.codex.permission_mode` only to supported approval policies.
- Keep `codex app-server` and `codex exec` as future documented options for bounded headless workers. They are not the lead-session path in D-14.

### Google Antigravity CLI (Phase 5)

- Detect the official `agy` binary and version; Google documents `agy -p` headless execution, `json`/`stream-json` output, and conversation resume IDs.
- Prefer `stream-json` for headless turns and persist only the returned native conversation ID. Validate interactive use in tmux before using it as a lead.
- Use only the user's existing Antigravity configuration and permissions. Do not assume the CLI's internal subagents are AgentShiftboard peer sessions.

### Hermes Agent CLI (Phase 5)

- Detect the official Nous `hermes` binary and version.
- The documented CLI supports interactive TUI use, one-shot `hermes chat -q`, and native session resume by ID. Validate follow-up messaging, status signals, process control, and approval handling before promising equivalent support to other adapters.
- Preserve the user's Hermes provider/model configuration; AgentShiftboard does not route requests or rewrite provider credentials.

### Adapter contract

Each adapter exposes: detect/capability report, interactive launch and resume commands for tmux, headless start-or-resume where supported, status signal sources (hooks/notify, pane cues), side-effect enforcement capability (`available` or `unknown`), the state-interface allowlist snippet for that CLI, send turn (headless only), interrupt, wait/stream events, and classification of observed stops. Capability discovery must say `unknown` when the CLI does not provide reliable information. Do not probe quotas with a model request during `init`.

## 9. TUI and command surface

### Primary screen

```text
┌ AgentShiftboard ────────────────┬──────────────────────────────────────┐
│ ● 1 running  ◐ 1 need you       │                                      │
│                                 │   the selected session's lead (or a  │
│ ● #4 README + goodbye.txt  3/5  │   worker) runs here; type directly   │
│    ✓ Create goodbye.txt         │                                      │
│    ◎ Write README.md   codex    │                                      │
│    ● Commit 3 files             │                                      │
│    ○ Push to origin             │                                      │
│    ! approval P-2 git push …    │                                      │
│ ■ #1 Create hello.txt  2/2      │                                      │
│ ─────────────────────────────── │                                      │
│ Agents: claude lead ● · codex … │                                      │
└─────────────────────────────────┴──────────────────────────────────────┘
 Shift+← list · Shift+→ session · Ctrl+Q leave
```

The workspace is one tmux session per project on AgentShiftboard's own server (D-14, D-15). Its `home` window is this list; every lead and worker runs in its own window. `Enter` opens a session full screen and `Ctrl+Q` returns to the list. From any pane, `Shift+Up` opens the actions panel of the session you are in, and `Shift+Down` jumps to what needs you. The first run with no open task goes straight into a new lead. When a lead's CLI exits, its window offers resume, switching to another CLI, a new task, or another task. The list shows waiting-for-approval and native prompts, cooldown/reset information when known, stale-brief flags, proposed rules and delegations, held locks, and the lead lease state, and distinguishes task state from session status.

### Initial command surface

The split between agent and user operations is defined in [STATE-CONTRACT.md](STATE-CONTRACT.md#coordinator-and-write-path).

Read operations (everyone, including MCP):

- `task list|show`, `brief show`, `rule list`, `pending list`, `effect list`, `get resources [-o toml]`, `config get`, `tool list|show`, `lock list`.

User commands (board or terminal; refused from inside a managed agent session; not exposed through MCP):

- `agent-shiftboard init` — inspect the project and create local state.
- `agent-shiftboard` — create or attach to the project's tmux workspace.
- `agent-shiftboard run <request>` — create a task and start its lead.
- `agent-shiftboard open <task>` — show a task in the main slot; if its lead is not running, offer `lead resume` or `lead new`.
- `agent-shiftboard task cancel|reopen` — user task transitions.
- `agent-shiftboard agent list|doctor|select` — see detected CLIs and set lead order.
- `agent-shiftboard lead resume [<task>]` — resume the holder's native session; `lead new [<task>] [--provider <p>]` — start a new lead session from the brief (same provider by default); `lead switch <provider> [<task>]` — shorthand for `lead new --provider`; `handoff confirm [<task>]` — confirm a pending pane swap. Without `<task>`, these act on the task shown in the main slot.
- `agent-shiftboard rule confirm|revoke|supersede`, `delegation confirm|revoke`, `pending approve|deny`, `assignment cancel`, `lock break`.
- `agent-shiftboard apply -f <manifest>` — apply shared resources.

Agent operations (CLI subcommands and `agent-shiftboard mcp`; covered by allowlist snippets):

- `task new|describe`, `brief update`, `rule add`, `pending add|record-answer|record-approval|withdraw`, `delegation propose|log`.
- `effect plan|start|finish|reconcile`, `milestone record`.
- `delegate <task>` (creates an assignment), `assignment submit|accept|reject|cancel`, `handoff ack`.
- `message send|list|cancel` (short notes between sessions of a task, D-25).
- `worker start`, `session list|close` (the lead starts and closes its task's agents, D-26).
- `task block|unblock|close`.
- `config set`, `secret ref add`, `tool register|deprecate`, `lock acquire|release`.

Internal: `agent-shiftboard hook <provider> <event>` — hook handler entry point.

Refine the command surface during the integration spike.

## 10. Delivery phases

### Phase 0 — Product and integration spike

- Confirm the boundary against CarryCtx, Agent Deck, ReevesAgents, and mu; decide whether the state contract integrates with CarryCtx or remains a local module.
- Inspect supported versions and live behavior of Claude Code and Codex: interactive launch with session IDs, resume, interrupt, hooks/notify, approval surfacing, and side-effect enforcement, without bypassing each CLI's native policy.
- Spike the tmux runtime (D-14, D-15): fixed layout, `swap-pane`/`respawn-pane` handoff, hook-based status, pane-cue fallback, and tmux server lifetime on macOS across terminal close, sleep, and logout.
- Spike the state interface (D-16): CLI and MCP operations and per-provider allowlists so checkpoints do not prompt.

### Phase 1 — Local shell and state contract

- Create the Go CLI skeleton, project initialization, and tmux workspace.
- Detect executable/version and display a health report without making inference calls.
- Add the SQLite store and the state operations over CLI and MCP, with the agent/user operation split: brief revisions, rules, pending items, delegations, assignments, side effects, milestones, shared resources, the lead lease state machine, and status vocabularies.
- Render the brief and resource index; add the board with manual task creation and a fixed lead.

### Phase 2 — Native lead and same-provider continuity

- Implement the Claude Code or Codex adapter end-to-end, whichever the Phase 0 spike shows is more ready.
- Start/resume the lead in the main slot with a pre-assigned native session ID where supported, otherwise capture it from provider hooks; report hook-based status and interrupt it.
- Add stop/failure classification for this adapter (quota with or without reset time, authentication, context limit, network/service, crash, host loss).
- Add stale-brief detection and nudges (D-17) and side-effect intent enforcement where the provider supports it.
- Add same-provider continuity: wait-and-resume after cooldown, resume after host loss or crash, and a fresh session with readback after a context limit.

### Phase 3 — Second provider and independent workers

- Finish and validate the second Claude Code/Codex adapter behind the shared interface, including its hook payloads, native resume, and status mapping. The Codex adapter is implemented from current docs but has not had a live session validation.
- Delegate scoped assignments to separate sessions of the same or a different provider, interactive or headless.
- Add per-task worktrees for concurrent code edits; support read-only shared checkout only for explicitly read-only review tasks.
- Add named advisory locks (D-20).
- Surface worker results as submitted assignments that the lead accepts into the brief or rejects.

### Phase 4 — Explicit cross-provider lead handoff (completes the MVP)

- Validate both adapters' classification against cross-provider handoff scenarios.
- Add explicit handoff: reconciliation, preflight, launch in a hidden window with brief and resource index, readback acknowledgement, and confirmed pane swap.

### Phase 5 — More providers and automatic failover

- Add the Antigravity and Hermes adapters (D-02).
- Add opt-in automatic successor selection from a user-configured provider priority list and billing/auth profiles, once failure classification is reliable.

### Phase 6 — Usability and release readiness

- Refine board navigation, approval and delegation prompts, task filters, and worktree visibility.
- Add backup/export/import for task state and resources without transcript export.
- Define supported CLI versions, upgrade behavior, data retention, and recovery instructions.
- Revisit product differentiation based on user workflows and direct comparison with the closest repos.

## 11. Risks and controls

| Risk | Design response |
|---|---|
| Provider CLIs change output/protocols | Isolate adapters, record tested versions, fail to `unknown` rather than guessing. |
| Quota detection is inconsistent | Per-provider classifiers; separate quota from authentication, context limit, transient, and unknown; never rotate accounts. |
| A successor lacks hidden conversational context | Keep the brief authored by the lead and current, and ground handoff in files/Git state. Exact internal model context cannot move across vendors. |
| Agents forget to checkpoint | Stale-brief detection from metadata, nudges, and intent enforcement where hooks allow (D-17). |
| Agents rewrite helpers that already exist | Shared tool registry in a provider-neutral directory; resource index given to every session (D-19). |
| Secret values leak into task state | Secret references only; reject token-like strings on write; tools read secrets at run time. |
| Concurrent edits conflict | One worktree per writing task; coordinator owns task leases; lead reviews results before merge. |
| Parallel sessions collide on external resources | Named advisory locks declared by tasks and tools (D-20). |
| Agent emits overly long state | Validate schema and configurable size budget; ask the lead to condense rather than silently save a transcript. |
| Sensitive task details become durable | Local-only storage, restrictive file permissions, brief preview, explicit export; transcripts are never stored. |
| A lead approves its own request | User operations are unavailable through MCP and allowlists and refused from agent sessions; conversation approvals are quoted and shown for correction (D-16). |
| Multiple agents believe they are lead | Transactional lease and single current-lead pointer per task. |
| Native CLI requires human approval | Represent approval as a first-class waiting state and deliver it to the user; preserve CLI permission settings. |
| A delegation is read too broadly | Delegations require user confirmation, explicit scope and expiry, and never answer native permission prompts (D-18). |
| A lead fails between a side effect and its report | Record intent before the action and a minimal result receipt after it; reconcile before retrying and leave ambiguous outcomes `unknown`. |
| A successor lacks tools or authenticated access | Keep task and tool requirements explicit; preflight only non-secret metadata and ask the user to resolve unknown access. |
| Pane-text status classification breaks when a CLI changes its UI | Prefer hooks/notify; keep pane cues per adapter and CLI version; fall back to `unknown`. |
| Keystrokes land in a native approval prompt | Never send keys to a pane in `waiting_for_approval`; pass startup prompts as arguments or files. |
| A native transcript contains useful recovery details | Do not ingest it into task memory; resume its owner session where supported, otherwise reconstruct from checkpoints and artifacts or ask the user. |

## 12. Definition of MVP

The MVP is complete at the end of Phase 4. It can:

- initialize a local project and its tmux workspace, and detect Claude Code and Codex;
- run one lead in the main slot with hook-based status;
- keep the brief, scoped rules, delegations, pending approvals, side-effect outcomes, and shared resources through the CLI/MCP state operations, without transcript ingestion;
- flag a stale brief and nudge the lead;
- continue the same provider after cooldown, a closed terminal, or a context limit;
- start one independent worker of the same or the other provider;
- reconcile the worktree;
- manually hand the lead to the other provider with capability preflight, readback acknowledgement, and a confirmed pane swap.

Antigravity and Hermes adapters and opt-in automatic failover follow in Phase 5.
