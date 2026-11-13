# Task state and handoff contract

## Purpose

The task brief is the durable, portable memory for a unit of work. It lets a fresh native CLI session continue from the project's verified state without replaying prior chat. The current lead authors semantic task updates; the coordinator owns leases, rules, approvals, delegations, side-effect receipts, shared resources, locks, and revisions.

The brief is not a chat transcript, activity dump, or model reasoning trace. AgentShiftboard never reads or ingests provider transcript files as shared task memory or as automatic recovery input. A provider adapter may ask its own CLI to resume a native session by its session ID; the provider CLI remains responsible for its own session history.

## Coordinator and write path

The **coordinator** is AgentShiftboard's core state logic over the local SQLite store. It is not a separate daemon: it runs inside whichever AgentShiftboard process is acting at the moment — a state command (CLI or MCP server), a provider hook handler, or the TUI board. Each of them uses short SQLite transactions. Interactive agent processes are owned by tmux, not by the coordinator (D-14).

All state changes go through one set of coordinator operations (D-16). This includes brief revisions, rules, pending items, delegations, assignments, side effects, shared resources, and locks. Each brief update carries the revision it was based on; an update based on an older revision is rejected and must be retried on the current one. Other operations act on individual records by ID, each in its own transaction, and check the record's current status.

Operations are split by who may perform them:

| Class | Operations | Exposed through |
|---|---|---|
| Read operations | Show tasks, briefs, records, and shared resources. | Everyone: CLI, MCP, and the board. |
| Agent operations | Create tasks, and describe (title and goal) the task the session leads; add and update work items; update the brief; add rules (`active` when user-stated, `proposed` when inferred); add pending items and record an answer or approval the user gave in the conversation; propose delegations and log decisions under an active one; plan, start, finish, and reconcile side effects; record milestones; create, submit, accept, reject, or cancel assignments; acknowledge a handoff; block, unblock, or close a task; register config, secret references, and tools; acquire and release own locks. | CLI subcommands and the MCP server. Per-provider allowlist snippets cover only read and agent operations. |
| User operations | Confirm, revoke, or supersede rules; confirm or revoke delegations; approve or deny pending items from the board; break a lock; resume a lead, start a new lead session, or confirm a pane swap; cancel or reopen a task; apply resource manifests. | The board and user CLI commands only. Not exposed through MCP, not in allowlist snippets, and refused when called from inside a managed agent session. |

A **managed agent session** is a process AgentShiftboard launched for an agent, and everything it starts. AgentShiftboard sets `AGENT_SHIFTBOARD_SESSION_ID` only in the environment of those processes (never in the tmux session's global environment, the board, or the user's own shells) and records the agent CLI's process ID. A caller counts as that session when the variable is present or when the recorded process is one of its ancestors, so removing the variable, or a user typing a command through the agent's own shell escape, does not turn it into a user operation. User operations refuse to run for a managed session. The MCP server inherits the variable from the agent that started it. With the shared MCP server (D-28), a session is identified by the per-session token in its MCP connection; the server stores only token hashes and revokes a token when its session ends. The MCP server acts only for a live managed session: started without one (for example by an agent gateway shared across sessions) or for a session that has ended, it refuses every call and never falls back to the user's authority. Where a provider CLI may move a session into a new process that keeps its arguments but not its environment, the identity also travels with the per-session files the CLI is started with: for Claude Code, the settings file sets the variable for the session's tools (its `env` block) and in each hook command, and the MCP config sets it for the MCP server.

An approval the user gives in the conversation may be recorded by the lead with `pending record-approval`. The coordinator rejects the call unless it names an existing `pending` approval of the same task and includes a short quote of the user's words; action and target are optional, and when given they must match the recorded ones (the first live test showed that requiring an exact repeat of long strings made valid approvals fail); it is stored as `via: conversation` and highlighted on the board so the user can correct it. The user may set `conversation_approvals = "board_only"` to require every approval on the board instead. This and other policy settings (permission policy, provider priority, wait thresholds, grace periods, size budgets) live in AgentShiftboard's own TOML configuration, which only the user edits; they are never shared-resource `config` entries, so agent operations cannot change them. The provider CLI's own permission prompt still applies to the action itself.

This split prevents accidental self-approval. It is not a security boundary against a deliberately hostile process running as the same OS user, which could, for example, unset the environment variable.

The Markdown brief (`agent-shiftboard brief show`; also rendered to a file in the machine-local runtime directory outside the repository) is a read-only view for people and agents. Edits to that file have no effect and are overwritten on the next render. Declarative manifests for shared resources are applied explicitly with `agent-shiftboard apply` (see [Shared resources](#shared-resources-config-secret-references-and-tools)).

## State layers

### Tier 1: portable task brief

Keep an orientation brief concise. The initial size target is about 1,500 tokens and is configurable; it is a target, not a hard cap on the task's evidence records. Coordinator-rendered sections list record IDs with one line each; when they would exceed the budget, task-scope items are rendered in full and user/project items by ID and the first line of their text.

| Field | Owner | Meaning |
|---|---|---|
| `goal` | Lead (user authoritative) | The outcome the user asked for, distilled without changing its meaning. |
| `done_when` | Lead | Observable completion conditions agreed with the user. |
| `decisions` | Lead | Settled choices and the short reason they matter. |
| `completed` | Lead | Narrative of work verified as finished, with evidence references. Task status remains the coordinator's record. |
| `in_progress` | Lead | Narrative of the current action and its owner. Ownership remains the coordinator's record. |
| `blockers` | Lead | What prevents progress and who can resolve it. |
| `next_action` | Lead | The next concrete action a new lead should take. |
| `relevant_refs` | Lead | Paths, branches, worktrees, commits, merge requests, backups, and other artifact identifiers needed to continue. Never a provider transcript. |
| `verification` | Lead | Checks performed, result, repository revision, and `verified_at`. |
| `rules_in_force` | Rendered | Active rules from user, project, and task scope, with IDs. |
| `open_items` | Rendered | Open `pending_user` items, active delegations, and side effects in `unknown` status, with IDs. |
| `delivery_milestones` | Rendered | One-line summary of the canonical Tier 2 milestone records. |
| `resources` | Rendered | Names of shared config, secret references, and tools available to the task (never secret values). |
| `updated_by` / `updated_at` / `revision` | Coordinator | Provenance and optimistic-concurrency metadata. |

### Tier 2: structured coordinator records

Store larger or operational details as typed records. The brief links to them by ID. Status values for every record type are listed under [Status vocabularies](#status-vocabularies).

- **`rules`** — durable user rules. Fields: ID, concise text, scope (`user`, `project`, or `task`), source (who, when, where), `stated` or `confirmed`, `valid_from`, `superseded_by`, status. A rule the user stated in the conversation is recorded as `active` with `stated`, and the board shows new rules so the user can correct them. A rule the lead infers is recorded as `proposed` until the user confirms it. The lead cannot change or revoke an active rule; only the user can, and a change creates a new rule that supersedes the old one.
- **`pending_user`** — unanswered questions and approvals, each with an ID, kind (`question` or `approval`), exact action and target scope, requester, creation time, optional `expires_at`, status, and how it was resolved (`via: board` or `via: conversation` with a short quote). Only an explicit user response for that scope changes an approval to approved. A successor must never infer approval from a similar earlier request.
- **`items`** — the small steps of the work, which the user follows ("I-n"). Fields: text, status, owner (for example `claude lead`, `codex worker`), source, the entry's ID in the agent's own todo tool when known, and order. Items come from three places: the agent's own todo or plan tool, mirrored from its tool calls by the provider hook (Claude Code `TodoWrite`/`TaskCreate`/`TaskUpdate`, Codex `update_plan`; only the tool call's arguments are read, never transcripts); explicit `item add`/`item set`; and assignments, whose item follows the assignment's status. An entry that disappears from a mirrored list without being done is cancelled.
- **`assignments`** — work the lead delegates to a worker session. Fields: ID, task, worker session, scope, expected output, required tools, shared constraints, worktree, status, result summary, and evidence references. The coordinator sets `running` when the worker session first reports `running`. The worker submits a result; the lead accepts it into the brief or rejects it with a reason. The lead or the user may cancel an assignment that is not yet accepted.
- **`messages`** — short notes between sessions of one task ("M-n", D-25). Fields: sender session (or the user), recipient session, body (one paragraph, at most 500 characters), status, created and delivered times. The board delivers a `queued` message by typing one line into the recipient's pane only while the recipient is `idle`, shows no native prompt, has no text the user started typing (no undimmed text on any line of its input box, and the cursor right after the input glyph), and is not `waiting_for_approval`; it then records `delivered` and clears the body. A board claims a message atomically before typing it, so overlapping boards never both type it; typing comes before the record, so a crash may deliver a message twice (once its claim expires) but never loses one; delivery is oldest first per task. The sender or the user may cancel a queued message. A message is not an approval, a delegation, or a rule, and is never rendered into the brief or launch context.
- **`delegations`** — standing authority the user grants for a period, for example "decide product questions overnight and report in the morning". Fields: ID, allowed scope (decision kinds, actions, and targets), explicit exclusions, granted by, granted at, `expires_at`, status, and a decision log. The lead may record a delegation the user stated, but it stays `proposed` until the user confirms it on the board or through the CLI. Each decision taken under a delegation is logged with time, session, and `needs_review: true` until the user reviews it. A delegation satisfies an approval only when its scope explicitly names that action and target. It never changes rules, widens permission modes, or answers a provider CLI's own permission prompt.
- **`side_effects`** — an intent recorded before a non-idempotent or external action, followed by a minimal outcome receipt. Actions include shell commands and MCP tool calls that match the project's external-action patterns, and every run of a registered tool with effect class `external_write`. Record action, target, timestamps, status, evidence reference, and backup/rollback reference when available. Lifecycle:
  - `effect plan` records `planned` (intent recorded; may wait for approval).
  - `effect start`, called immediately before the action, records `in_flight`.
  - `effect finish` from `in_flight` records `confirmed` (it happened), `failed` (attempted and failed), or `not_performed` (aborted before it ran). From `planned` it may only record `not_performed` (abandoned).
  - When the owning session ends a turn (`idle`), any of its effects still `in_flight` become `unknown`; `planned` effects stay open, since they may be waiting for approval.
  - When the owning session reaches `stopped`, `failed`, `cooling_down`, `quota_unavailable`, or `authentication_required`, or stays `unknown` longer than the configured grace period, every `planned` or `in_flight` effect becomes `unknown`, because the action may have run without a report. A brief `unknown` (for example during a long tool call without hooks) leaves effects unchanged.
  - `effect reconcile` resolves an `unknown` effect to `confirmed`, `failed`, or `not_performed` from a read-only check, recording `reconciled_at` and evidence. Any session holding the lease may reconcile, including the same session after a resume.
- **Delivery milestones** (canonical) — commit, push, merge request, build, deployment, and verification records, each attached to its target/environment, evidence identifier, and observation time.
- **`locks`** — named advisory locks on shared resources outside AgentShiftboard's control (D-20).
- **Machine-specific records** — provider session IDs, process and tmux pane IDs, billing/auth profile aliases, capability-preflight results, and resources AgentShiftboard started for a task or session: worktrees, temporary directories, port assignments, and background processes (D-11).

Do not store whole user messages to preserve intent. Store durable rules and task requirements as concise structured records with provenance and exact scope. Quote a short phrase only when the wording itself changes the meaning.

## Shared resources: config, secret references, and tools

Shared resources work like Kubernetes ConfigMaps and Secrets: sessions refer to them by name, and every provider sees the same registry (D-19). Each resource has scope `user` (all projects) or `project`.

- **`config`** — non-secret named values, such as environment URLs, namespaces, service names, and fixed IDs, each with a description. A value that is a credential belongs in a secret reference, never here.
- **`secret_refs`** — where a secret lives on this machine, never its value. Fields: name, location (for example an env-file path or a keychain item), key names it provides, purpose, and handling rule (for example "read-only use; never print"). Portable manifests declare only the secret names a task or tool needs; each machine binds those names to local locations.
- **`tools`** — reusable helper scripts or commands that any provider session can run. Fields: name, one-line description, command or path, usage and inputs, effect class (`read_only`, `local_write`, or `external_write`), targets, requirements (config names, secret names, executables), locks it takes, created by (provider session), `verified_at`, status, and `superseded_by`.

Tool files live in a provider-neutral directory: `.agent-shiftboard/tools/` for machine-local project tools, a repository path the user configures for team-shared tools, or `~/.config/agent-shiftboard/tools/` for user-scope tools. A tool must never live only in a provider-private directory such as one CLI's scratch or job folder.

Rules:

1. Every new session — lead or worker, any provider — receives the resource index: config names, secret names with their handling rules, and tool names with one-line descriptions. Secret values are never rendered, logged, or put in prompts.
2. Before writing a new helper, an agent checks the tool index. When a helper is reused, or a worker or successor may need it, the agent registers it as a tool and moves its file into a tools directory.
3. Tools read secrets at run time through secret references; they never embed secret values.
4. Running a tool with effect class `external_write` is a side effect and follows the intent-then-receipt rule.
5. Successor and worker preflight checks each required tool's requirements and reports `available`, `missing`, or `unknown`.
6. Agents change resources through the state operations. The user may also write a declarative manifest and run `agent-shiftboard apply -f <file>`; `agent-shiftboard get resources -o toml` exports the current registry.

## Status vocabularies

Every status observation records its source (`hook`, `notify`, `stream`, `pane`, `process`, or `user`) and `observed_at`. These are the only allowed values.

### Agent session status

| Status | Meaning |
|---|---|
| `starting` | Launched; not yet ready for input. |
| `running` | A turn is in progress. |
| `idle` | Alive and ready for input. Open questions to the user appear as `pending_user` records, not as a separate status; AgentShiftboard does not infer them from screen text. |
| `waiting_for_approval` | The provider CLI is waiting for a user permission decision: its own prompt in an interactive pane, or an approval request from a documented headless protocol (such as Codex app-server). AgentShiftboard never sends keystrokes to such a pane and never answers on the user's behalf; for a headless request the board shows it and relays only the user's decision. |
| `retrying` | The CLI is retrying a transient network or service error by itself. |
| `cooling_down` | Stopped by a provider limit with a known reset time. |
| `quota_unavailable` | Stopped by a provider limit with no known reset time. |
| `authentication_required` | The CLI requires the user to log in. |
| `stopped` | The process ended without a failure: finished, closed by the user, or its tmux host went away. |
| `failed` | The process or turn ended abnormally and cannot continue by itself. |
| `unknown` | The state cannot be determined reliably, including when a session last seen as `starting`, `running`, or `retrying` has no new observation within the configured window. |

An interrupted turn returns the session to `idle`. Compaction inside a CLI counts as `running`.

### Stop and failure reasons

`retrying`, `cooling_down`, `quota_unavailable`, `authentication_required`, `stopped`, `failed`, and `unknown` carry a reason:

| Reason | Status | Expected recovery |
|---|---|---|
| `quota` | `cooling_down` if the reset time is known, else `quota_unavailable` | Wait-and-resume, or hand off to another provider or billing/auth profile. |
| `authentication` | `authentication_required` | The user logs in to that CLI; handoff only if the user chooses it. |
| `context_limit` | `failed` | Start a fresh session of the same provider from the brief, or use the CLI's own compaction if it offers one. Never classify it as quota. |
| `network_service` | `retrying` while the CLI retries; `failed` once it gives up | Resume when the service recovers, or hand off. |
| `process_crash` | `failed` | Reconcile, then resume the native session by ID where supported. |
| `host_lost` | `stopped` | tmux pane or server, or the machine, went away; reconcile, then resume by ID. |
| `user_stopped` | `stopped` | None unless the user resumes it. |
| `completed` | `stopped` | A headless worker finished its assignment; the lead reviews the result. |
| `unknown` | `unknown` | Surface to the user; do not guess a reason. |

### Task status

| Status | Meaning |
|---|---|
| `queued` | Created; no lead has started. |
| `active` | A lead holds or is acquiring the lease and work is in progress. |
| `blocked` | Work cannot proceed until a recorded blocker is resolved; the blocker names who can resolve it (user, another task, external). Set and cleared by the lead with `task block` / `task unblock`. |
| `done` | The lead ran `task close` after recording that `done_when` conditions are met. Delivery milestones are tracked separately. The user may reopen the task, which returns it to `queued` until a lead starts. |
| `cancelled` | Stopped by the user without completion. |

### Lead lease state

| State | Meaning |
|---|---|
| `vacant` | No lead (task queued, done, or cancelled). |
| `held` | One session is the current lead. |
| `suspended` | The holder is not usable and no new session has been started. |
| `handoff_pending` | A new session — fresh session of the same provider, or a session of another provider — has been started and is awaiting readback acknowledgement. |

Lease transitions are transactional. At most one session holds a task's lease.

- **First lead.** A task's first lead, and the first lead after a reopen, is a new session: `vacant` → `handoff_pending` → `held` after readback acknowledgement. A lead started in the visible main slot acknowledges before its first reply to the user.
- **Acknowledgement timeout.** Only a new session that has not reported any status within the timeout is abandoned; one that reported (for example `idle`, waiting for the user's first message) stays pending until it acknowledges.
- **Voluntary handoff.** When the user starts a new lead session (`lead new` / `lead switch`) while the holder is still usable, the lease moves from `held` directly to `handoff_pending`. From then on the old holder's writes are rejected, and the board reminds the user that the old CLI is still running so they can interrupt it.
- **Suspension.** The lease moves from `held` to `suspended` when the holder reaches `stopped`, `failed`, `cooling_down`, `quota_unavailable`, or `authentication_required`, or stays `unknown` longer than a configured grace period. `retrying` and a brief `unknown` do not suspend it.
- **Recovery without a new session.** While the lease is `suspended` (never once it is `handoff_pending`), if the same holder is observed `idle` or `running` again — because its CLI recovered by itself or because it was resumed — the lease returns to `held` with the same holder.
- **Writes.** A `suspended` holder may still write state, so it can checkpoint after recovering. Once the lease is `handoff_pending`, writes from the old holder are rejected, and the new session's first accepted write must be its handoff acknowledgement.

There are two continuation kinds:

- **Resume** of the same native session ID (wait-and-resume after cooldown, resume after a crash or lost host): `held` → `suspended` → `held` with the same holder. The session keeps its own context, so no readback is needed; reconciliation still runs after a crash or lost host. In tmux the same slot is restarted with the native resume command.
- **New session** (fresh same-provider session after a context limit or when resume is impossible, or any cross-provider successor): `held` → `suspended` → `handoff_pending` → `held` by the new session after readback acknowledgement (see [Handoff protocol](#handoff-protocol)). The new session runs in the main slot when no live lead occupies it, otherwise in its own hidden window (D-15).

### Record statuses

| Record | Values |
|---|---|
| `rules` | `proposed`, `active`, `superseded`, `revoked` |
| `pending_user` | `pending`, `approved`, `denied`, `answered`, `withdrawn`, `expired` |
| `assignments` | `assigned`, `running`, `submitted`, `accepted`, `rejected`, `cancelled` |
| `items` | `pending`, `in_progress`, `review`, `done`, `blocked`, `cancelled` (an assignment's item: assigned → pending, running → in_progress, submitted → review, accepted → done, rejected → blocked) |
| `delegations` | `proposed`, `active`, `expired`, `revoked` |
| `messages` | `queued`, `delivered`, `cancelled` |
| `side_effects` | `planned`, `in_flight`, `confirmed`, `failed`, `not_performed`, `unknown` |
| Delivery milestones | `not_started`, `in_progress`, `succeeded`, `failed`, `unknown` |
| Capability preflight | `available`, `missing`, `unknown` |
| Verification and reconciliation checks | `passed`, `mismatch`, `unknown` |
| `tools` | `active`, `deprecated`, `superseded` |
| `locks` | `held`, `released`, `stale` |

## Checkpoint rules

1. The lead updates the brief when the goal, a decision, task status, blocker, owner, or next action changes.
2. Before delegating, record the scoped assignment, expected output, relevant references, required tools, and shared constraints.
3. Before a non-idempotent or external action, record its intent and any required approval. After the tool returns, record the observed outcome and a compact evidence reference.
4. Checkpoint after meaningful state changes such as a completed edit, commit, push, merge request, deployment, or worker result. Use provider-native hooks or events where available; do not depend on one provider's hooks being available in every CLI.
5. If the session ends before the outcome is observed, the side effect becomes `unknown`. Do not retry it until a relevant read-only reconciliation establishes whether it happened.
6. Before ending a normal turn or handing off while still able, leave a verified `next_action` and current worktree/branch state. This is an extra checkpoint, not the only checkpoint.
7. If a fact is uncertain, label it unverified instead of writing it as settled.
8. Register reusable helpers as shared tools (see [Shared resources](#shared-resources-config-secret-references-and-tools)).

### Stale-brief detection

The coordinator flags a brief as stale using metadata only (D-17):

- Git HEAD in the task's worktree differs from the revision the brief last recorded.
- More than a configured number of turns (counted from hook or stream turn boundaries) since the last brief revision.
- An action matched an external-action pattern without a corresponding side-effect intent.

When stale, the coordinator shows it on the board and nudges the lead through a documented hook output channel where the provider offers one. It never types nudges into a pane. It may use hook metadata such as tool name, matched pattern name, exit status, and timestamps; it does not store command output or conversation content.

Hooks are best-effort, provider-specific aids. For example, Claude Code has separate hooks for successful tool use, compaction, normal turn completion, API failure, and session end; those events have different behavior. The coordinator and the state operations remain authoritative. A hook that needs another model call must not be required for checkpointing, since that call can fail for the same quota or service reason as the lead.

## Reconciliation before takeover

Before a successor acknowledges a handoff, reconcile the relevant current state using read-only checks where available:

1. Inspect Git status, current revision, branch, and uncommitted diff in the assigned worktree.
2. Check coordinator-owned processes, panes, and ports for that task; do not infer that unrelated machine processes belong to it.
3. Check external delivery state such as a merge request or deployment only when the task uses that system and a configured, non-mutating check is available.
4. Record the revision and `verified_at` for each check. If a check is unavailable or its result is ambiguous, record `unknown` and surface that fact.
5. On a mismatch, inspect evidence and update the state; do not repeat the action blindly.

Network checks, remote refreshes, and provider-specific integrations must be visible to the user. Do not silently fetch, mutate, deploy, or authenticate as part of reconciliation.

## Handoff protocol

1. The coordinator keeps one current lead lease per task and prevents two sessions from owning it concurrently.
2. Continuous checkpoints are the recovery basis. A handoff does not require the old lead to be alive or to produce a final message.
3. Prefer resuming the same native session (see [Lead lease state](#lead-lease-state)): offer wait-and-resume after a known cooldown according to the user's configured threshold. This protocol covers new sessions. Choose an explicitly enabled successor by provider and, when known, billing/auth profile. Multiple sessions in one exhausted billing pool do not count as independent quota capacity.
4. The lease is `suspended` (failure) or still `held` by a usable session (voluntary handoff); see [Lead lease state](#lead-lease-state). Retain the old provider/session reference plus its stop reason, or a note that the user started a voluntary handoff. Never switch identities to evade a provider limit.
5. Reconcile relevant state and preflight the successor's task-required tools, shared-tool requirements, secret references, and capabilities. Report `available`, `missing`, or `unknown`; do not copy credentials or assume provider-specific memory, MCP servers, skills, or permission settings carry over.
6. Start a new provider-native session with the latest portable brief, approved task artifacts, the resource index, and reconciled workspace state, passed as launch arguments or a referenced handoff file. Never use transcript-file ingestion to reconstruct the brief. The lease is now `handoff_pending`.
7. Start the successor under its own CLI permission policy. Permission modes do not transfer; use the stricter configured mode when the successor's mode is unknown.
8. Require acknowledgement by readback within a configured timeout: the successor's first state write lists the IDs of the active rules, active delegations, open `pending_user` items, and `unknown` side effects it has read. A missing ID means the handoff is not acknowledged. Bound successor retries; if no eligible successor can proceed, stop and ask the user instead of cycling indefinitely.
9. After acknowledgement the lease becomes `held` by the new session. Its pane moves into the main slot only on the user's confirmation. When the user started the new session from the board or with `lead new` / `lead switch`, that choice is the confirmation and the swap happens right after acknowledgement. Under opt-in automatic failover the successor works from its hidden window and the board shows a banner until the user confirms (D-15).
10. If the successor detects stale or conflicting state, it records a blocker and asks before overwriting state or repeating an external action.

Same-provider continuation may use the provider's documented native resume operation with its session ID. Cross-provider handoff starts the other CLI's native session; only the portable brief, Tier 2 records, shared resources, selected artifacts, and actual workspace state transfer.

## Excluded data

The task state store must not retain:

- Full user-agent or agent-agent transcripts, including transcript excerpts imported for recovery.
- Message bodies after delivery; a queued `messages` body is held only until the board delivers or the sender cancels it.
- Hidden reasoning or chain-of-thought.
- Entire terminal output streams, pane contents, source files, or tool traces as task memory.
- API keys, cookies, auth tokens, environment variable values, or provider credential files — including inside config values, tool files, or manifests.

The coordinator rejects a write whose text matches configured secret patterns (known token prefixes, private-key headers, long high-entropy strings) and tells the writer to use a secret reference instead. Pattern matching is a safety net, not a guarantee.

The live TUI may display process output while it runs. A separate, explicit diagnostic option may save bounded logs for troubleshooting, but those logs are not canonical context and require their own retention controls.

## Ownership and edits

The coordinator is authoritative for task IDs, session IDs, leases, task status, rule and approval status, delegations, side-effect receipts, shared resources, locks, process status, and ownership transitions. User operations are listed in [Coordinator and write path](#coordinator-and-write-path); an approval given in the conversation is recorded by the lead as described there. The current lead is authoritative for the lead-owned brief fields, subject to the user's rules and approval state. The current lead starts workers for its assignments and may close an idle worker or former lead of its task through the coordinator (D-26); agents never end panes or processes directly. Workers — of the same or a different provider — may submit scoped updates or findings; the current lead accepts and incorporates useful facts into the canonical brief. Evidence references remain separate from transcript history.
