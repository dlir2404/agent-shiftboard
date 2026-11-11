# Instructions for coding agents

## Project objective

Build AgentShiftboard, a local-first terminal control plane that runs official coding-agent CLIs as independent sessions and manages tasks, leadership, and compact cross-session handoff state.

Read `README.md`, `docs/PLAN.md`, `docs/STATE-CONTRACT.md`, and `docs/DECISIONS.md` before making architecture or persistence changes.

## Non-negotiable constraints

- Use installed vendor CLIs and documented local session protocols. Do not implement an inference API proxy, credential extraction, or provider-auth impersonation.
- Agents managed by this application are independent CLI processes/sessions. Do not implement the product around vendor subagents.
- The lead agent owns the semantic task brief and updates it only through the coordinator's state operations (CLI subcommands or the MCP server); the rendered Markdown brief is read-only. The coordinator stores task/session metadata, state revisions, the structured records defined in `docs/STATE-CONTRACT.md`, and minimal lifecycle events.
- Never ingest provider transcript files as AgentShiftboard task memory, including as an automatic recovery fallback. A provider adapter may invoke that CLI's documented native resume operation using a session ID. Do not persist hidden reasoning, credentials, raw environment variables, or large command output as task memory.
- The task brief must be concise and useful to a fresh session. Prefer verified facts, decisions, current status, blockers, and the next action.
- Capture durable user rules with their source and scope (`user`, `project`, `task`); keep approval requests pending until the user explicitly approves the exact action and target. A delegation becomes active only after the user confirms it and never answers a provider CLI's permission prompt.
- Store secret references (location and handling rule), never secret values. Keep shared config and tools provider-neutral so every provider's sessions can use them.
- Before a potentially external or non-idempotent action, record its intent. Record the observed outcome afterward; use `unknown` when the process dies before confirming it.
- Do not silently switch accounts, fabricate provider quota status, or bypass provider limits. Use configured providers and explicit local CLI authentication.
- Do not transfer a permission mode between providers. Start a successor under its own native policy, using a stricter mode when configuration is unknown.
- Preserve each provider CLI's permission and approval controls. Never enable a skip-permissions mode by default.
- Keep provider-specific behavior behind an adapter. Do not pretend provider sessions share one native session format.

## Implementation habits

- Treat `docs/STATE-CONTRACT.md` as the source of truth for handoff semantics.
- Treat `docs/DECISIONS.md` as the rationale log. Update it when a product boundary or persistence invariant changes.
- Keep user-visible statuses explicit and use only the vocabularies in `docs/STATE-CONTRACT.md` (agent status: starting, running, idle, waiting for approval, retrying, cooling down, quota unavailable, authentication required, stopped, failed, unknown).
- Make mutations to task ownership and lead assignment transactional and recoverable.
- Keep the terminal UI as a view/controller for the local coordinator; do not make TUI screen state the durable source of task state. Never send keystrokes to a pane that is waiting for approval.
- Do not add unrelated feature scope or change the provider policy assumptions without updating the plan first.

