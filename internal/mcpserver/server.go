// Package mcpserver exposes AgentShiftboard's read and agent operations over
// MCP (stdio). User operations are deliberately absent (D-16).
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/render"
)

// Opener opens a coordinator for one call.
type Opener func(ctx context.Context) (*coord.Coordinator, error)

// Ops supplies lifecycle operations from the CLI host. The MCP package
// knows nothing about tmux or provider launch scripts.
type Ops struct {
	WorkerStart  func(context.Context, *coord.Coordinator, coord.Actor, string, string) (string, string, error)
	SessionClose func(context.Context, *coord.Coordinator, coord.Actor, string) error
	SessionList  func(context.Context, *coord.Coordinator, coord.Actor, int64) ([]coord.SessionView, error)
}

// New builds the server. Each tool call opens the coordinator, acts as the
// session named in the environment, and closes it again.
func New(version string, open Opener, ops ...*Ops) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "agent-shiftboard", Version: version}, nil)
	r := registrar{s: s, open: open}
	if len(ops) > 0 {
		r.ops = ops[0]
	}
	r.register()
	return s
}

type registrar struct {
	s    *mcp.Server
	open Opener
	ops  *Ops
}

// Task selects a task; 0 means the calling session's own task.
type Task struct {
	Task int64 `json:"task,omitempty" jsonschema:"task number; omit to use your session's task"`
}

type ID struct {
	ID string `json:"id" jsonschema:"record ID, e.g. P-3, E-7, A-2, G-1"`
}

func add[In any](r registrar, name, desc string, fn func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in In) (any, error)) {
	mcp.AddTool(r.s, &mcp.Tool{Name: "shiftboard_" + name, Description: desc},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			c, err := r.open(ctx)
			if err != nil {
				return nil, nil, err
			}
			defer c.Close()
			actor := coord.ActorFromEnv()
			if err := c.CheckMCPCaller(ctx, actor); err != nil {
				return nil, nil, err
			}
			out, err := fn(ctx, c, actor, in)
			if err != nil {
				return nil, nil, err
			}
			var text string
			if s, ok := out.(string); ok {
				text = s
			} else {
				b, _ := json.MarshalIndent(out, "", "  ")
				text = string(b)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		})
}

func task(ctx context.Context, c *coord.Coordinator, a coord.Actor, t int64) (int64, error) {
	return c.ResolveTask(ctx, a, t)
}

func ok(msg string) any { return msg }

func (r registrar) register() {
	add(r, "worker_start", "Start an independent worker for an assignment (lead); choose its provider for short or long work.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		ID       string `json:"id" jsonschema:"assignment ID, e.g. A-2"`
		Provider string `json:"provider" jsonschema:"provider CLI: claude or codex"`
	}) (any, error) {
		if r.ops == nil || r.ops.WorkerStart == nil {
			return nil, fmt.Errorf("worker_start is unavailable: lifecycle operations were not configured")
		}
		sid, pane, err := r.ops.WorkerStart(ctx, c, a, strings.ToUpper(in.ID), in.Provider)
		return map[string]string{"session": sid, "pane": pane}, err
	})
	add(r, "session_close", "Close an idle worker or former lead of your task (lead). Cancel assigned or running work first; the current lead and your own session cannot be closed.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Session string `json:"session" jsonschema:"session ID, assignment ID (A-n), or unique session ID prefix"`
	}) (any, error) {
		if r.ops == nil || r.ops.SessionClose == nil {
			return nil, fmt.Errorf("session_close is unavailable: lifecycle operations were not configured")
		}
		return ok("closed"), r.ops.SessionClose(ctx, c, a, in.Session)
	})
	add(r, "session_list", "List sessions with their task-relative role, status, and assignment.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in Task) (any, error) {
		if r.ops == nil || r.ops.SessionList == nil {
			return nil, fmt.Errorf("session_list is unavailable: lifecycle operations were not configured")
		}
		return r.ops.SessionList(ctx, c, a, in.Task)
	})
	// --- read operations ---
	add(r, "task_list", "List tasks with status and lead lease state.", func(ctx context.Context, c *coord.Coordinator, _ coord.Actor, _ struct{}) (any, error) {
		return c.ListTasks(ctx)
	})
	add(r, "brief_show", "Show the task brief: lead-owned fields plus rules in force, open items, milestones, and shared resources. Note the revision: brief_update needs it.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in Task) (any, error) {
		id, err := task(ctx, c, a, in.Task)
		if err != nil {
			return nil, err
		}
		v, err := render.Load(ctx, c, id)
		if err != nil {
			return nil, err
		}
		return render.Markdown(v, c.Policy.BriefTokenBudget), nil
	})
	add(r, "rule_list", "List active rules (user, project, task scope) for your task.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in Task) (any, error) {
		id, _ := task(ctx, c, a, in.Task)
		return c.ListRules(ctx, id, true)
	})
	add(r, "pending_list", "List open questions and approvals for your task.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in Task) (any, error) {
		id, err := task(ctx, c, a, in.Task)
		if err != nil {
			return nil, err
		}
		return c.ListPending(ctx, id, true)
	})
	add(r, "effect_list", "List side effects for your task.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Task   int64  `json:"task,omitempty" jsonschema:"task number; omit to use your session's task"`
		Status string `json:"status,omitempty" jsonschema:"filter: planned, in_flight, confirmed, failed, not_performed, unknown"`
	}) (any, error) {
		id, err := task(ctx, c, a, in.Task)
		if err != nil {
			return nil, err
		}
		return c.ListEffects(ctx, id, in.Status)
	})
	add(r, "assignment_list", "List worker assignments for your task.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in Task) (any, error) {
		id, err := task(ctx, c, a, in.Task)
		if err != nil {
			return nil, err
		}
		return c.ListAssignments(ctx, id)
	})
	add(r, "resources_get", "List shared config, secret references (never values), and tools. Check tools before writing a helper.", func(ctx context.Context, c *coord.Coordinator, _ coord.Actor, _ struct{}) (any, error) {
		return c.GetResources(ctx)
	})
	add(r, "handoff_readback", "List the IDs you must read back to acknowledge a handoff.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in Task) (any, error) {
		id, err := task(ctx, c, a, in.Task)
		if err != nil {
			return nil, err
		}
		return c.RequiredReadback(ctx, id)
	})
	add(r, "lock_list", "List held and stale advisory locks.", func(ctx context.Context, c *coord.Coordinator, _ coord.Actor, _ struct{}) (any, error) {
		return c.ListLocks(ctx)
	})

	// --- agent operations ---
	add(r, "handoff_ack", "Acknowledge a handoff by listing every required ID (active rules, delegations, open pending items, unknown side effects).", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		IDs []string `json:"ids" jsonschema:"all IDs from handoff_readback"`
	}) (any, error) {
		return "acknowledged; you hold the lead lease", c.AckHandoff(ctx, a, in.IDs)
	})
	add(r, "brief_update", "Update lead-owned brief fields on top of base_revision. set replaces fields (goal, in_progress, next_action are strings; done_when, decisions, completed, blockers, relevant_refs, verification are string lists); append adds to list fields.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Task         int64               `json:"task,omitempty" jsonschema:"task number; omit to use your session's task"`
		BaseRevision int64               `json:"base_revision" jsonschema:"the brief revision you read"`
		Set          map[string]any      `json:"set,omitempty"`
		Append       map[string][]string `json:"append,omitempty"`
	}) (any, error) {
		id, err := task(ctx, c, a, in.Task)
		if err != nil {
			return nil, err
		}
		p := coord.BriefPatch{Set: map[string]json.RawMessage{}, Append: in.Append}
		for k, v := range in.Set {
			b, _ := json.Marshal(v)
			p.Set[k] = b
		}
		rev, err := c.UpdateBrief(ctx, a, id, in.BaseRevision, p)
		return map[string]int64{"revision": rev}, err
	})
	add(r, "rule_add", "Record a durable rule the user stated (active) or that you inferred (inferred=true, stays proposed until the user confirms).", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Text     string `json:"text"`
		Scope    string `json:"scope" jsonschema:"user, project, or task"`
		Source   string `json:"source" jsonschema:"who/when/where, e.g. user in chat 2026-11-11"`
		Inferred bool   `json:"inferred,omitempty"`
	}) (any, error) {
		return c.AddRule(ctx, a, coord.RuleInput{Scope: in.Scope, Text: in.Text, Source: in.Source, Inferred: in.Inferred})
	})
	add(r, "pending_add", "Open a question or an approval request for the user. Approvals need an exact action and target.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Kind             string `json:"kind" jsonschema:"approval or question"`
		Action           string `json:"action" jsonschema:"exact action, or the question text"`
		Target           string `json:"target,omitempty"`
		ExpiresInMinutes int    `json:"expires_in_minutes,omitempty"`
	}) (any, error) {
		return c.AddPending(ctx, a, 0, in.Kind, in.Action, in.Target, time.Duration(in.ExpiresInMinutes)*time.Minute)
	})
	add(r, "pending_record_answer", "Record the user's answer to a question, given in the conversation.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		ID     string `json:"id" jsonschema:"record ID, e.g. A-2, P-3, E-7, I-4"`
		Answer string `json:"answer"`
	}) (any, error) {
		return ok(in.ID + " answered"), c.RecordAnswer(ctx, a, strings.ToUpper(in.ID), in.Answer)
	})
	add(r, "pending_record_approval", "Record an approval the user explicitly gave in the conversation: the P-id and a short quote of the user's words.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		ID     string `json:"id" jsonschema:"record ID, e.g. A-2, P-3, E-7, I-4"`
		Action string `json:"action,omitempty" jsonschema:"optional; must match the recorded action"`
		Target string `json:"target,omitempty" jsonschema:"optional; must match the recorded target"`
		Quote  string `json:"quote"`
	}) (any, error) {
		return ok(in.ID + " approved via conversation"), c.RecordApproval(ctx, a, strings.ToUpper(in.ID), in.Action, in.Target, in.Quote)
	})
	add(r, "pending_withdraw", "Withdraw a pending item that is no longer needed.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in ID) (any, error) {
		return ok(in.ID + " withdrawn"), c.WithdrawPending(ctx, a, strings.ToUpper(in.ID))
	})
	add(r, "delegation_propose", "Record standing authority the user stated (stays proposed until the user confirms).", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Scope      string  `json:"scope"`
		Exclusions string  `json:"exclusions,omitempty"`
		GrantedBy  string  `json:"granted_by"`
		Hours      float64 `json:"hours" jsonschema:"how long it lasts"`
	}) (any, error) {
		return c.ProposeDelegation(ctx, a, 0, in.Scope, in.Exclusions, in.GrantedBy, c.Now().Add(time.Duration(in.Hours*float64(time.Hour))))
	})
	add(r, "delegation_log", "Log a decision you took under an active delegation; it is marked for the user's review.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		ID       string `json:"id" jsonschema:"record ID, e.g. A-2, P-3, E-7, I-4"`
		Decision string `json:"decision"`
	}) (any, error) {
		return ok("logged"), c.LogDecision(ctx, a, strings.ToUpper(in.ID), in.Decision)
	})
	add(r, "effect_plan", "Record intent before an external or non-idempotent action (push, deploy, DB write, message, publish).", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Action     string `json:"action"`
		Target     string `json:"target"`
		ApprovalID string `json:"approval_id,omitempty" jsonschema:"P-id of the approval this action needs"`
		Rollback   string `json:"rollback,omitempty" jsonschema:"backup or rollback reference"`
	}) (any, error) {
		return c.PlanEffect(ctx, a, 0, in.Action, in.Target, strings.ToUpper(in.ApprovalID), in.Rollback)
	})
	add(r, "effect_start", "Mark a planned effect in flight, immediately before running the action.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in ID) (any, error) {
		return ok(in.ID + " in flight"), c.StartEffect(ctx, a, strings.ToUpper(in.ID))
	})
	add(r, "effect_finish", "Record the observed outcome: confirmed, failed, or not_performed.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		ID       string `json:"id" jsonschema:"record ID, e.g. A-2, P-3, E-7, I-4"`
		Status   string `json:"status"`
		Evidence string `json:"evidence,omitempty"`
	}) (any, error) {
		return ok(in.ID + " " + in.Status), c.FinishEffect(ctx, a, strings.ToUpper(in.ID), in.Status, in.Evidence)
	})
	add(r, "effect_reconcile", "Resolve an unknown effect from a read-only check (lead).", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		ID       string `json:"id" jsonschema:"record ID, e.g. A-2, P-3, E-7, I-4"`
		Status   string `json:"status"`
		Evidence string `json:"evidence"`
	}) (any, error) {
		return ok(in.ID + " reconciled"), c.ReconcileEffect(ctx, a, strings.ToUpper(in.ID), in.Status, in.Evidence)
	})
	add(r, "milestone_record", "Record a delivery milestone (commit, push, merge_request, build, deploy, verify).", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Kind     string `json:"kind"`
		Target   string `json:"target"`
		Status   string `json:"status" jsonschema:"not_started, in_progress, succeeded, failed, unknown"`
		Evidence string `json:"evidence,omitempty"`
	}) (any, error) {
		return c.RecordMilestone(ctx, a, 0, in.Kind, in.Target, in.Status, in.Evidence)
	})
	add(r, "delegate", "Create an assignment for a worker session (lead).", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Scope         string `json:"scope"`
		Expected      string `json:"expected_output,omitempty"`
		RequiredTools string `json:"required_tools,omitempty"`
		Constraints   string `json:"constraints,omitempty"`
		Worktree      string `json:"worktree,omitempty"`
	}) (any, error) {
		return c.CreateAssignment(ctx, a, coord.AssignmentInput{Scope: in.Scope, Expected: in.Expected, RequiredTools: in.RequiredTools, Constraints: in.Constraints, Worktree: in.Worktree})
	})
	add(r, "assignment_submit", "Submit your concise result for an assignment (worker).", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		ID       string `json:"id" jsonschema:"record ID, e.g. A-2, P-3, E-7, I-4"`
		Result   string `json:"result"`
		Evidence string `json:"evidence,omitempty"`
	}) (any, error) {
		return ok("submitted"), c.SubmitAssignment(ctx, a, strings.ToUpper(in.ID), in.Result, in.Evidence)
	})
	add(r, "assignment_review", "Accept or reject a submitted result (lead).", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		ID     string `json:"id" jsonschema:"record ID, e.g. A-2, P-3, E-7, I-4"`
		Accept bool   `json:"accept"`
		Reason string `json:"reason,omitempty"`
	}) (any, error) {
		return ok("reviewed"), c.ReviewAssignment(ctx, a, strings.ToUpper(in.ID), in.Accept, in.Reason)
	})
	add(r, "assignment_cancel", "Cancel an assignment that is not yet accepted (lead).", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		ID     string `json:"id" jsonschema:"record ID, e.g. A-2, P-3, E-7, I-4"`
		Reason string `json:"reason,omitempty"`
	}) (any, error) {
		return ok("cancelled"), c.CancelAssignment(ctx, a, strings.ToUpper(in.ID), in.Reason)
	})
	add(r, "message_send", "Send a short message (one paragraph, max 500 characters) to another session of your task: to is lead, an assignment ID (A-n), a session ID or prefix, or a provider name. The board types it in when that session is idle. Never carries user approval.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		To   string `json:"to" jsonschema:"lead, A-n, a session ID or prefix, or a provider name such as claude"`
		Text string `json:"text"`
		Task int64  `json:"task,omitempty" jsonschema:"task number; omit to use your session's task"`
	}) (any, error) {
		return c.SendMessage(ctx, a, in.Task, in.To, in.Text)
	})
	add(r, "message_list", "List the sessions of your task you can message, and recent messages (bodies are cleared once delivered).", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in Task) (any, error) {
		id, err := task(ctx, c, a, in.Task)
		if err != nil {
			return nil, err
		}
		ts, err := c.MessageTargets(ctx, a, id)
		if err != nil {
			return nil, err
		}
		ms, err := c.ListMessages(ctx, id, false)
		return map[string]any{"targets": ts, "messages": ms}, err
	})
	add(r, "message_cancel", "Cancel a queued message you sent.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in ID) (any, error) {
		return ok("cancelled"), c.CancelMessage(ctx, a, strings.ToUpper(in.ID))
	})
	add(r, "task_status", "Block, unblock, or close your task (lead). Close only after recording that done_when is met.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Task    int64  `json:"task,omitempty" jsonschema:"task number; omit to use your session's task"`
		Op      string `json:"op" jsonschema:"block, unblock, or close"`
		Blocker string `json:"blocker,omitempty" jsonschema:"for block: what blocks and who can resolve it"`
	}) (any, error) {
		id, err := task(ctx, c, a, in.Task)
		if err != nil {
			return nil, err
		}
		switch in.Op {
		case "block":
			err = c.BlockTask(ctx, a, id, in.Blocker)
		case "unblock":
			err = c.UnblockTask(ctx, a, id)
		case "close":
			err = c.CloseTask(ctx, a, id)
		default:
			err = fmt.Errorf("op must be block, unblock, or close")
		}
		return ok("task updated"), err
	})
	add(r, "task_create", "Create another task the user brought up; it has no lead until one is started.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Title string `json:"title"`
		Goal  string `json:"goal,omitempty"`
	}) (any, error) {
		return c.CreateTask(ctx, a, in.Title, in.Goal)
	})
	add(r, "task_describe", "Set your task's title and goal once the user has said what they want (lead).", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Task  int64  `json:"task,omitempty" jsonschema:"task number; omit to use your session's task"`
		Title string `json:"title"`
		Goal  string `json:"goal,omitempty"`
	}) (any, error) {
		id, err := task(ctx, c, a, in.Task)
		if err != nil {
			return nil, err
		}
		return ok("described"), c.DescribeTask(ctx, a, id, in.Title, in.Goal)
	})
	add(r, "item_list", "List the session's work items.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in Task) (any, error) {
		id, err := task(ctx, c, a, in.Task)
		if err != nil {
			return nil, err
		}
		return c.ListItems(ctx, id)
	})
	add(r, "item_add", "Record a work item (your own todo/plan list is mirrored automatically; use this for anything else).", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Text   string `json:"text"`
		Status string `json:"status,omitempty" jsonschema:"pending, in_progress, review, done, blocked, cancelled"`
		Owner  string `json:"owner,omitempty"`
	}) (any, error) {
		return c.AddItem(ctx, a, 0, in.Text, in.Status, in.Owner)
	})
	add(r, "item_set", "Change a work item's status (and optionally its text).", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		ID     string `json:"id" jsonschema:"record ID, e.g. A-2, P-3, E-7, I-4"`
		Status string `json:"status"`
		Text   string `json:"text,omitempty"`
	}) (any, error) {
		return ok(in.ID + " updated"), c.SetItem(ctx, a, strings.ToUpper(in.ID), in.Status, in.Text)
	})
	add(r, "config_set", "Set a shared non-secret config value (project scope).", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Name        string `json:"name"`
		Value       string `json:"value"`
		Description string `json:"description,omitempty"`
	}) (any, error) {
		return ok("set"), c.SetConfig(ctx, a, coord.ConfigValue{Name: in.Name, Scope: "project", Value: in.Value, Description: in.Description})
	})
	add(r, "secret_ref_add", "Record where a secret lives and how to handle it (never the value).", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Name     string   `json:"name"`
		Location string   `json:"location"`
		Keys     []string `json:"keys,omitempty"`
		Purpose  string   `json:"purpose,omitempty"`
		Handling string   `json:"handling" jsonschema:"e.g. read-only use; never print"`
	}) (any, error) {
		return ok("recorded"), c.AddSecretRef(ctx, a, coord.SecretRef{Name: in.Name, Scope: "project", Location: in.Location, Keys: in.Keys, Purpose: in.Purpose, Handling: in.Handling})
	})
	add(r, "tool_register", "Register a reusable helper so every provider's sessions can use it. Its file must not live in a provider-private or temp folder.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Name        string             `json:"name"`
		Description string             `json:"description"`
		Command     string             `json:"command"`
		Usage       string             `json:"usage,omitempty"`
		Effect      string             `json:"effect" jsonschema:"read_only, local_write, or external_write"`
		Targets     string             `json:"targets,omitempty"`
		Requires    coord.ToolRequires `json:"requires,omitempty"`
		Locks       []string           `json:"locks,omitempty"`
	}) (any, error) {
		return ok("registered"), c.RegisterTool(ctx, a, coord.Tool{Name: in.Name, Scope: "project", Description: in.Description, Command: in.Command,
			Usage: in.Usage, Effect: in.Effect, Targets: in.Targets, Requires: in.Requires, Locks: in.Locks})
	})
	add(r, "lock_acquire", "Take a named advisory lock on a shared external resource.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Name string `json:"name"`
	}) (any, error) {
		return ok("acquired"), c.AcquireLock(ctx, a, in.Name)
	})
	add(r, "lock_release", "Release a lock you hold.", func(ctx context.Context, c *coord.Coordinator, a coord.Actor, in struct {
		Name string `json:"name"`
	}) (any, error) {
		return ok("released"), c.ReleaseLock(ctx, a, in.Name)
	})
}
