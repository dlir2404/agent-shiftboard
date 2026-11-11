# Key decisions

These decisions define AgentShiftboard's product boundaries and explain the main choices in `PLAN.md` and `STATE-CONTRACT.md`. Revisit them when new provider behavior or user requirements give a concrete reason to change course.

## D-01 — Run the selected native CLIs directly

**Decision:** AgentShiftboard launches provider-native CLIs and uses their documented local session interfaces. It does not proxy inference requests, impersonate provider authentication, or copy credentials.

**Why:** The user explicitly requires native CLI usage and wants each provider's own login, billing, session, and permission behavior to remain authoritative.

## D-02 — Initial CLI targets are Claude Code, Codex, Antigravity, and Hermes

**Decision:** The initial adapter target list is Claude Code (`claude`), Codex CLI (`codex`), Google Antigravity CLI (`agy`), and Hermes Agent CLI (`hermes`). Gemini CLI is not an initial target; the project owner asked to use Antigravity CLI and Hermes CLI instead. Implementation order: Claude Code and Codex first, because their hooks and session protocols are the best documented — both are spiked in Phase 0, one is implemented in Phase 2 and the other in Phase 3, and both are part of the MVP. Antigravity and Hermes follow in Phase 5, once the adapter contract is proven.

**Why:** This is the provider set the project owner wants to use. Google's current Antigravity documentation describes a native `agy` CLI with headless execution, structured output, and conversation resume; Nous documents Hermes as a native terminal CLI with single-query mode and session resume. Verify exact behavior against installed versions in the integration spike before claiming adapter parity. Codex adapter implementation is now present, but live hook trust, launch-context delivery, MCP approval, and resume behavior remain unvalidated.

**References:** [Antigravity CLI headless mode](https://antigravity.google/docs/cli/headless/), [Hermes CLI interface](https://hermes-agent.nousresearch.com/docs/user-guide/cli).

## D-03 — The task brief, not conversation history, is shared memory

**Decision:** The lead writes and maintains a concise semantic task brief. AgentShiftboard never reads or ingests provider transcript files as task memory or automatic recovery input. A provider adapter may invoke that CLI's own documented resume operation using its native session ID.

**Why:** Full transcripts are noisy, duplicate tool output, can be large, and are not portable between providers. Native resume preserves a provider's own session where available; cross-provider continuity comes from the brief, approved artifacts, and verified workspace state.

## D-04 — Checkpoint continuously and journal side effects

**Decision:** Update task state at meaningful transitions. Record intent before a non-idempotent or external action, then record a minimal outcome receipt after it. If the process ends before the outcome is known, mark it `unknown`.

**Why:** A lead can hit a quota or process failure between performing an action and reporting it. No final handoff turn can be assumed. Provider hooks can help, but the state contract must not depend on hooks or another model call being available.

## D-05 — Preserve user rules and approval scope as structured state

**Decision:** Track durable user rules with provenance and scope, and keep questions/approvals in explicit pending records. Approval applies only to the exact action and target the user approved. Do not store every user message verbatim.

**Why:** A successor must retain active constraints and know which actions still require consent without turning the task store into a transcript archive. The user remains the authority for rules and approvals.

## D-06 — Reconcile before a successor continues

**Decision:** Before a new lead acknowledges takeover, check relevant Git/worktree state, coordinator-owned processes, and task-relevant delivery state using non-mutating checks where available. Store revision and `verified_at`; unresolved checks remain `unknown`.

**Why:** A brief can become stale if an action completed just before a crash. Reconciliation reduces duplicate writes and false completion claims. It is task-scoped; it does not trigger silent remote updates, deployments, or authentication.

## D-07 — Use a short brief plus evidence references

**Decision:** Aim for a configurable Tier 1 brief around 1,500 tokens, with a separate Tier 2 of structured records and references to files, commits, merge requests, backups, and delivery evidence. The target is not a hard limit on useful references.

**Why:** The next lead needs a quick orientation and exact handles for deeper inspection. Keeping references separate avoids bloating every new session while retaining operational detail.

## D-08 — Model failure by billing/auth profile, not just session

**Decision:** Classify provider failures separately from session state. When known, group sessions under a user-configured opaque billing/auth profile. Offer wait-and-resume for a documented native session when a cooldown is short enough; otherwise use an explicitly enabled successor. Never rotate identities to evade a limit.

**Why:** Multiple sessions can consume the same provider quota. Anthropic documents shared usage limits between Claude and Claude Code for Pro and Max plans, so opening another session under that same plan does not create a fresh allowance. Do not infer a profile from credentials; use `unknown` when the CLI does not expose enough information.

**Reference:** [Anthropic: Claude Pro/Max usage with Claude Code](https://support.anthropic.com/en/articles/11145838-using-claude-code-with-your-pro-or-max-plan).

## D-09 — Preflight successor capabilities and keep permissions local

**Decision:** Tasks may declare required tools and capabilities. Before handoff, report each as `available`, `missing`, or `unknown` using non-secret checks. The successor starts with its own native permissions; permission modes and credentials do not transfer.

**Why:** A new provider may lack an MCP server, skill, executable, or access that the old lead used. Blind handoff causes failed work; copying credentials or widening permissions would violate the local-provider boundary.

## D-10 — Keep task lifecycle separate from delivery milestones

**Decision:** Task status answers whether work is queued, active, blocked, or complete. Separate evidence-backed milestones record commit, push, review, build, deployment, and verification state per target/environment.

**Why:** “Task complete” does not say whether a change was merged or deployed, and real delivery can branch across environments. Milestones are more accurate than a single linear `delivery_state` enum.

## D-11 — Track resources AgentShiftboard owns; defer cross-instance coordination

**Decision:** Record worktrees and processes that AgentShiftboard starts; use per-task temporary directories where supported and record port assignments. Defer a shared ledger across unrelated agent sessions/instances. Named advisory locks on external resources are the one exception (D-20).

**Why:** Worktree, temp-directory, port, and orphan-process collisions are real local risks. Automatic port allocation cannot control every app, and cross-instance coordination expands the local control plane into a shared service problem. Delivery references still belong on the task they serve.

## D-12 — Bound handoff retries

**Decision:** Successor acknowledgement has a timeout and a bounded retry policy. If the selected successors are unavailable or fail their capability checks, stop and ask the user rather than cycling indefinitely.

**Why:** Automatic failover without a stopping rule can repeatedly launch unusable sessions or obscure a task that needs human input.

## D-13 — Build AgentShiftboard's own session layer; borrow patterns from Agent Deck

**Decision:** AgentShiftboard implements its own session runtime instead of depending on or forking Agent Deck. It reuses proven patterns: native CLIs hosted in tmux, hook-first status detection with a pane-content fallback, and pre-assigned native session IDs.

**Why:** The project owner chose to keep the runtime and the state contract under one design rather than inherit another tool's CLI surface, permission defaults (for example its optional dangerous mode for conductors), and provider list. Agent Deck shows in practice that tmux-hosted native CLIs with hook-based status work across several providers. See [LANDSCAPE.md](LANDSCAPE.md) and [REVIEW-2.md](REVIEW-2.md) §1a–1b.

**Reference:** [Agent Deck](https://github.com/asheshgoplani/agent-deck) (MIT): `docs/architecture.md`, `docs/status-detection.md`, `docs/session-id-lifecycle.md`.

## D-14 — The lead runs its native interactive CLI in tmux; workers may run headless

**Decision:**
- The lead runs as the provider's native interactive CLI inside a tmux pane. The user types into that CLI directly, so native permission prompts, slash commands, and skills keep working. AgentShiftboard does not relay the lead conversation through its own composer.
- Bounded worker tasks may run headless through a documented stream (`claude -p --output-format stream-json`, `codex exec`, `codex app-server`, `agy -p --output-format stream-json`). A worker that needs interactive approval runs in tmux like the lead.
- The tmux server owns interactive session processes, so they survive closing the terminal or the TUI. The coordinator keeps durable state in SQLite. Lease liveness comes from the last hook or status observation; no separate daemon is required for the first release.
- Status comes from provider hooks or notify mechanisms first, then from classifying the visible pane, otherwise `unknown`. Pane content is used only for live classification and is never persisted as task memory.
- Where the CLI supports it, AgentShiftboard assigns the native session ID at launch (for example `claude --session-id <uuid>`) instead of discovering it afterwards.
- AgentShiftboard never sends keystrokes into a pane whose status is `waiting_for_approval`. Startup prompts are passed as CLI arguments or a referenced handoff file, not typed in.

**Amendment (2026-11-11):** the workspace runs on AgentShiftboard's own tmux server (`tmux -L agent-shiftboard`) with a generated config (`~/.config/agent-shiftboard/tmux.conf`): mouse on to focus panes, `Ctrl+Q` to leave (sessions keep running), keys shown in the status line. The user's own tmux server, sessions, and `~/.tmux.conf` are never touched. Agent Deck, by contrast, shares the default server and appends to `~/.tmux.conf`. From inside the user's own tmux, attaching is a nested attach.

**Why:** In headless mode Claude Code has no interactive permission prompt and loses slash commands and skills, so the lead would no longer be the CLI the user relies on. Keeping the native CLI also keeps each provider's approval controls authoritative. The cost is less structured failure detection for the lead; classifiers must fall back to `unknown` when unsure.

## D-15 — One fixed workspace window; the lead pane is swapped on handoff after confirmation

**Decision:**
- `agent-shiftboard` creates or attaches to one tmux session per project. A fixed main slot shows the current lead's CLI pane, beside a Bubble Tea status board and above a status line naming the lead's provider, permission mode, and brief revision.
- Every agent session lives in its own hidden tmux window. On a lead change the successor's pane is moved into the main slot (`swap-pane`). The previous lead's pane moves to a background window and stays alive for viewing and possible same-provider resume until the user closes it.
- Resuming the same native session restarts the same slot with the provider's native resume command (`respawn-pane`).
- When no live lead occupies the main slot (first lead, a fresh session after the lead exited, or a resume), the session runs directly in the main slot. `agent-shiftboard start` runs there: it asks for a task, runs the lead's CLI as its child, and offers resume, new session, or another task when the CLI exits. The user therefore sees every native prompt (folder trust, login, permissions) and answers it.
- A new session that would replace a live lead in the main slot starts in a hidden window and moves into the main slot after readback acknowledgement. When the user chose that action, the choice is the confirmation.
- A cross-provider successor starts in a hidden window and is swapped into the main slot only after its acknowledgement and the user's confirmation. Under opt-in automatic failover the successor holds the lease after acknowledgement and works from its hidden window; the board shows a banner and the pane swap still waits for the user.
- Worker panes are viewed in a popup and do not take over the main slot.

**Amendment (2026-11-11, first live test):** the first version started every new session in a hidden window. Claude Code then waited on its folder-trust dialog where nobody could see it, and the handoff stalled. Sessions now run in the visible main slot whenever it is free. The user's whole entry point is `agent-shiftboard`: it initializes the project if needed and starts (or resumes) the lead immediately; the lead describes the task and records further tasks from the conversation, with no task form and no automatic first message.

**Amendment (2026-11-11, third live test):** the side board was hard to use. The workspace now follows the session-list model the user preferred (as in Agent Deck): the first window, `home`, is a full-screen list of open tasks with their lead and worker sessions and what needs the user, with a live preview (status and the visible tail of the selected session's pane, display only, never stored). Enter opens a session's own window full screen; `Ctrl+Q` returns to the list; `q` on the list leaves. Every session runs in its own window, so lead changes no longer swap panes: a new lead opens in a new window. The list also flags sessions stopped at a native prompt (folder trust, hook review, login) so the user answers them; AgentShiftboard never does.

**Why:** The user wants one continuous window across days and lead changes. Swapping panes inside one tmux session keeps that window stable while each CLI stays native. Swapping only after confirmation avoids replacing a pane mid-input. The new lead starts without the previous chat history; the old pane stays viewable on screen, but AgentShiftboard does not read it.

## D-16 — One write path for task state: coordinator operations via CLI and MCP

**Decision:** Agents change task state only through the coordinator's agent operations, exposed identically as `agent-shiftboard` CLI subcommands and as an MCP server. The Markdown brief is a rendered, read-only view. Writes carry a base revision and are rejected when stale. AgentShiftboard ships per-provider allowlist snippets covering read and agent operations only, so checkpoints do not trigger a permission prompt. Policy settings live in a user-edited TOML file, not in agent-writable state. User operations — confirming, revoking, or superseding rules; confirming or revoking delegations; approving from the board; breaking locks; resuming or starting lead sessions and confirming pane swaps; cancelling or reopening tasks; applying manifests — are available only through the board and user CLI commands, never through MCP or allowlists, and are refused from inside a managed agent session (detected by an environment variable set only for agent processes). Conversation approvals recorded by the lead must name an existing pending approval of the task and quote the user; a project can require board-only approvals. See [STATE-CONTRACT.md](STATE-CONTRACT.md#coordinator-and-write-path).

**Why:** Two write paths (file edits and commands) race with revision assignment. Permission prompts on every checkpoint make agents skip checkpoints. CLI plus MCP covers every target provider with the same semantics. Exposing user operations to agents without a prompt would let a lead approve its own request; the split prevents that by accident, though it is not a security boundary against a hostile process running as the same OS user.

## D-17 — Detect a stale brief from metadata and nudge the lead

**Decision:** The coordinator flags a stale brief when Git HEAD moved past the recorded revision, too many turns passed since the last revision, or an external-action pattern matched without an intent record. It nudges through a documented hook output channel where available and always shows the flag on the board. It uses only hook/stream metadata and never stores command output or conversation content.

**Why:** Instructions alone do not keep agents checkpointing; every context loss observed in long multi-session work came from an update not written in time. Metadata signals are enough to detect staleness without reading transcripts.

## D-18 — Scoped rules and user-confirmed delegations

**Decision:** Rules carry a scope (`user`, `project`, `task`), provenance, `valid_from`, and `superseded_by`; a successor receives all three scopes. Delegations record standing authority the user grants for a period, with explicit scope, exclusions, expiry, and a decision log marked for review. A delegation becomes active only after the user confirms it, never answers a provider CLI's permission prompt, and satisfies an approval only when its scope names that exact action and target.

**Why:** Most durable rules in real use applied to every task in a project, or to every project (reply language, read-only databases, ask before push), and some changed over time. Users also grant bounded autonomy ("decide overnight, report in the morning"), which per-action approvals cannot express.

## D-19 — Shared resources: config, secret references, and tools

**Decision:** Like Kubernetes ConfigMaps and Secrets, AgentShiftboard keeps a provider-neutral registry of named config values, secret references (location and handling rule, never values), and reusable tools with a manifest (effect class, requirements, locks, status). Every session of any provider receives the index. Agents check it before writing helpers and register reusable helpers into a provider-neutral tools directory. Each project also declares external-action patterns covering shell commands and MCP tool names. Running an `external_write` tool, or any call matching a pattern, is a side effect that requires an intent record; where a provider offers a pre-tool hook, the hook enforces it, and elsewhere enforcement is reported as `unknown`.

**Why:** In practice one provider's agent wrote many helper scripts (publishing, data updates, scanners) into its own private scratch folders; a session of another provider did not know they existed and rewrote them. Successors also need to know where credentials live and how to use them without ever seeing values in task state.

## D-20 — Named advisory locks for shared external resources

**Decision:** Tasks and tools may declare named locks (for example `browser-profile:<name>` or `pod-tmp:<service>`). The coordinator grants one holder per name, shows holders on the board, marks a lock `stale` when its holder session is no longer alive, and lets the user break it. Locks are advisory and local to one machine. Phase 3.

**Why:** The costliest collisions between parallel sessions were on resources AgentShiftboard does not own: fixed temp-file names inside a shared pod, and a single-use refresh token in a shared browser state that logged the user out. A name-based lock prevents these without a cross-instance ledger.

## D-21 — Same-provider continuity first; the lead delegates to any provider

**Decision:** Build same-provider continuity first — resuming after compaction, cooldown, or a closed terminal, and coordinating sibling sessions — using the brief, rules, approvals, side effects, and shared resources (Phase 2). The lead may start workers of the same or a different provider (Phase 3). Explicit cross-provider lead handoff completes the MVP (Phase 4). Opt-in automatic cross-provider failover follows after the MVP, once failure classification is reliable.

**Why:** In long multi-session work, most handoffs happened within one provider. Serving them first makes the product useful earlier and exercises the same state contract that cross-provider handoff depends on.

## D-22 — Fixed status vocabularies with observation source and time

**Decision:** Agent session status, stop/failure reason, task status, lead lease state, and every record status use the fixed values in [STATE-CONTRACT.md](STATE-CONTRACT.md#status-vocabularies). Each observation records its source and time; a session last seen as `starting`, `running`, or `retrying` without a fresh observation within the configured window becomes `unknown`.

**Why:** Ambiguous or stale statuses cause wrong handoffs, such as treating a context limit as a quota failure or showing a dead session as running.

## D-23 — Configure Codex per invocation and inject launch context through hooks

**Decision:** Run Codex's native interactive TUI. Attach AgentShiftboard's stdio MCP server and hook handlers with invocation-scoped `-c` overrides; capture the native session ID from hook payloads and resume with `codex resume <id>`. Deliver the generated launch brief through `SessionStart.additionalContext`, which Codex treats as extra developer context, and retry it on the first trusted `UserPromptSubmit` if startup trust review delayed delivery. Keep the user's `developer_instructions`, built-in instructions, and global config intact. Leave native approval and sandbox settings alone unless the user explicitly sets `providers.codex.permission_mode`; then apply the supported interactive `on-request` policy for that invocation. Scope automatic MCP approval to the explicit AgentShiftboard read/agent-operation allowlist; user-only operations remain absent from the server. Require normal Codex `/hooks` trust review and never pass `--dangerously-bypass-hook-trust`.

**Why:** Codex's hooks provide the documented session ID and a supported context injection channel, while `-c` keeps integration state local to the managed invocation. `model_instructions_file` replaces Codex's built-in instructions, and replacing `developer_instructions` could erase user policy. Codex also intentionally requires review before a new non-managed hook can run, so its native trust boundary remains in force. See the [Codex hooks reference](https://developers.openai.com/codex/hooks) and [configuration reference](https://developers.openai.com/codex/config-reference).

**Validation still required:** A live session must confirm per-invocation hook activation and trust review, launch-context timing, MCP approval behavior, and native resume. No Codex model session has been started as part of this implementation pass.
