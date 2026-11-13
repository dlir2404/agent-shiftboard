package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/render"
	"github.com/dlir2404/agent-shiftboard/internal/tmux"
)

func (a *app) briefCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "brief", Short: "Read or update the task brief"}
	show := &cobra.Command{
		Use: "show", Short: "Show the rendered brief (or --json for fields)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := a.taskFor(ctx, c, actor)
				if err != nil {
					return err
				}
				v, err := render.Load(ctx, c, id)
				if err != nil {
					return err
				}
				return a.emit(v, func(w io.Writer) { fmt.Fprint(w, render.Markdown(v, c.Policy.BriefTokenBudget)) })
			})
		},
	}
	a.taskFlag(show)

	var base int64
	var sets, appends []string
	var patchFile string
	update := &cobra.Command{
		Use:   "update",
		Short: "Update lead-owned brief fields on top of --base revision (lead)",
		Long: `Fields: goal, done_when, decisions, completed, in_progress, blockers,
next_action, relevant_refs, verification.

  --set next_action="run the migration on staging"
  --set blockers='["waiting for DB access (user)"]'   (JSON list replaces the list)
  --append decisions="use LRU, bounded at 10k entries"
  --patch patch.json   ({"set": {...}, "append": {...}}; "-" reads stdin)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := buildPatch(sets, appends, patchFile)
			if err != nil {
				return err
			}
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := a.taskFor(ctx, c, actor)
				if err != nil {
					return err
				}
				rev, err := c.UpdateBrief(ctx, actor, id, base, p)
				if err != nil {
					return err
				}
				return a.done(fmt.Sprintf("Brief of task #%d is now r%d.", id, rev), map[string]any{"task": id, "revision": rev})
			})
		},
	}
	a.taskFlag(update)
	update.Flags().Int64Var(&base, "base", 0, "brief revision this update is based on (required)")
	update.Flags().StringArrayVar(&sets, "set", nil, "field=value (repeatable)")
	update.Flags().StringArrayVar(&appends, "append", nil, "list_field=item (repeatable)")
	update.Flags().StringVar(&patchFile, "patch", "", "JSON patch file, or - for stdin")
	_ = update.MarkFlagRequired("base")
	cmd.AddCommand(show, update)
	return cmd
}

func buildPatch(sets, appends []string, patchFile string) (coord.BriefPatch, error) {
	p := coord.BriefPatch{Set: map[string]json.RawMessage{}, Append: map[string][]string{}}
	if patchFile != "" {
		var data []byte
		var err error
		if patchFile == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(patchFile)
		}
		if err != nil {
			return p, err
		}
		if err := json.Unmarshal(data, &p); err != nil {
			return p, fmt.Errorf("patch: %w", err)
		}
		if p.Set == nil {
			p.Set = map[string]json.RawMessage{}
		}
		if p.Append == nil {
			p.Append = map[string][]string{}
		}
	}
	for _, s := range sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return p, fmt.Errorf("--set %q: expected field=value", s)
		}
		v = strings.TrimSpace(v)
		if strings.HasPrefix(v, "[") {
			if !json.Valid([]byte(v)) {
				return p, fmt.Errorf("--set %s: invalid JSON list", k)
			}
			p.Set[k] = json.RawMessage(v)
		} else {
			raw, _ := json.Marshal(v)
			if isListField(k) {
				raw, _ = json.Marshal([]string{v})
			}
			p.Set[k] = raw
		}
	}
	for _, s := range appends {
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return p, fmt.Errorf("--append %q: expected field=item", s)
		}
		p.Append[k] = append(p.Append[k], v)
	}
	return p, nil
}

func isListField(k string) bool {
	switch k {
	case "done_when", "decisions", "completed", "blockers", "relevant_refs", "verification":
		return true
	}
	return false
}

func (a *app) ruleCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "rule", Short: "Durable user rules (user, project, task scope)"}
	var all bool
	list := &cobra.Command{
		Use: "list", Short: "List rules for a task (active only unless --all)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, _ := c.ResolveTask(ctx, actor, a.task)
				rs, err := c.ListRules(ctx, id, !all)
				if err != nil {
					return err
				}
				return a.emit(rs, func(w io.Writer) {
					var rows [][]string
					for _, r := range rs {
						rows = append(rows, []string{r.ID, r.Scope, r.Status, r.Confirmation, trunc(r.Text, 70)})
					}
					table(w, "ID\tSCOPE\tSTATUS\tCONFIRM\tTEXT", rows)
				})
			})
		},
	}
	a.taskFlag(list)
	list.Flags().BoolVar(&all, "all", false, "include proposed, superseded, and revoked rules")

	var in coord.RuleInput
	add := &cobra.Command{
		Use: "add <text...>", Short: "Record a rule the user stated (or --inferred)", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Text = strings.Join(args, " ")
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				in.TaskID = a.task
				id, err := c.AddRule(ctx, actor, in)
				if err != nil {
					return err
				}
				return a.done("Recorded rule "+id+".", map[string]string{"id": id})
			})
		},
	}
	a.taskFlag(add)
	add.Flags().StringVar(&in.Scope, "scope", "task", "user | project | task")
	add.Flags().StringVar(&in.Source, "source", "", "who/when/where, e.g. \"user in chat 2026-11-11\" (required)")
	add.Flags().BoolVar(&in.Inferred, "inferred", false, "the lead inferred this rule; it stays proposed until the user confirms")

	userOp := func(use, short string, fn func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, id string) error) *cobra.Command {
		return &cobra.Command{Use: use + " <rule-id>", Short: short + " (user)", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
					if err := fn(ctx, c, actor, strings.ToUpper(args[0])); err != nil {
						return err
					}
					return a.done("Rule "+strings.ToUpper(args[0])+" updated.", nil)
				})
			}}
	}
	var supText, supSource string
	supersede := &cobra.Command{
		Use: "supersede <rule-id>", Short: "Replace an active rule with new text (user)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := c.SupersedeRule(ctx, actor, strings.ToUpper(args[0]), supText, supSource)
				if err != nil {
					return err
				}
				return a.done("Superseded by "+id+".", map[string]string{"id": id})
			})
		},
	}
	supersede.Flags().StringVar(&supText, "text", "", "new rule text")
	supersede.Flags().StringVar(&supSource, "source", "user", "source of the change")
	_ = supersede.MarkFlagRequired("text")

	cmd.AddCommand(list, add, supersede,
		userOp("confirm", "Activate a proposed rule", func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, id string) error {
			return c.ConfirmRule(ctx, actor, id)
		}),
		userOp("revoke", "Revoke a rule", func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, id string) error {
			return c.RevokeRule(ctx, actor, id)
		}),
	)
	return cmd
}

func (a *app) pendingCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "pending", Short: "Questions and approvals waiting for the user"}
	var all bool
	list := &cobra.Command{
		Use: "list", Short: "List open pending items (--all for resolved too)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := a.taskFor(ctx, c, actor)
				if err != nil {
					return err
				}
				ps, err := c.ListPending(ctx, id, !all)
				if err != nil {
					return err
				}
				return a.emit(ps, func(w io.Writer) {
					var rows [][]string
					for _, p := range ps {
						rows = append(rows, []string{p.ID, p.Kind, p.Status, trunc(p.Action, 40), trunc(p.Target, 30), p.ResolvedVia})
					}
					table(w, "ID\tKIND\tSTATUS\tACTION\tTARGET\tVIA", rows)
				})
			})
		},
	}
	a.taskFlag(list)
	list.Flags().BoolVar(&all, "all", false, "include resolved items")

	var kind, target string
	var expires time.Duration
	add := &cobra.Command{
		Use: "add <action-or-question...>", Short: "Open a question or an approval request", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := c.AddPending(ctx, actor, a.task, kind, strings.Join(args, " "), target, expires)
				if err != nil {
					return err
				}
				return a.done("Opened "+id+".", map[string]string{"id": id})
			})
		},
	}
	a.taskFlag(add)
	add.Flags().StringVar(&kind, "kind", "approval", "approval | question")
	add.Flags().StringVar(&target, "target", "", "exact target of the action (required for approvals)")
	add.Flags().DurationVar(&expires, "expires-in", 0, "optional expiry, e.g. 8h")

	byID := func(use, short string, nargs int, fn func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, args []string) error) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: cobra.MinimumNArgs(nargs),
			RunE: func(cmd *cobra.Command, args []string) error {
				args[0] = strings.ToUpper(args[0])
				return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
					if err := fn(ctx, c, actor, args); err != nil {
						return err
					}
					return a.done(args[0]+" updated.", map[string]string{"id": args[0]})
				})
			}}
	}
	var raAction, raTarget, raQuote string
	recApproval := byID("record-approval <P-id>", "Record an approval the user gave in the conversation (lead)", 1,
		func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, args []string) error {
			return c.RecordApproval(ctx, actor, args[0], raAction, raTarget, raQuote)
		})
	recApproval.Flags().StringVar(&raAction, "action", "", "optional; must match the recorded action")
	recApproval.Flags().StringVar(&raTarget, "target", "", "optional; must match the recorded target")
	recApproval.Flags().StringVar(&raQuote, "quote", "", "the user's words that approved it")

	cmd.AddCommand(list, add, recApproval,
		byID("record-answer <P-id> <answer...>", "Record the user's answer given in the conversation", 2,
			func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, args []string) error {
				return c.RecordAnswer(ctx, actor, args[0], strings.Join(args[1:], " "))
			}),
		byID("withdraw <P-id>", "Withdraw an item that is no longer needed", 1,
			func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, args []string) error {
				return c.WithdrawPending(ctx, actor, args[0])
			}),
		byID("approve <P-id>", "Approve a request (user)", 1,
			func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, args []string) error {
				return c.ApprovePending(ctx, actor, args[0], false)
			}),
		byID("deny <P-id>", "Deny a request (user)", 1,
			func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, args []string) error {
				return c.ApprovePending(ctx, actor, args[0], true)
			}),
		byID("answer <P-id> <answer...>", "Answer a question (user)", 2,
			func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, args []string) error {
				return c.AnswerPending(ctx, actor, args[0], strings.Join(args[1:], " "))
			}),
	)
	return cmd
}

func (a *app) delegationCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "delegation", Short: "Standing authority the user grants for a period"}
	list := &cobra.Command{
		Use: "list", Short: "List delegations and their decision logs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := a.taskFor(ctx, c, actor)
				if err != nil {
					return err
				}
				ds, err := c.ListDelegations(ctx, id, false)
				if err != nil {
					return err
				}
				return a.emit(ds, func(w io.Writer) {
					for _, d := range ds {
						fmt.Fprintf(w, "%s %s until %s: %s\n", d.ID, d.Status, d.ExpiresAt, d.Scope)
						if d.Exclusions != "" {
							fmt.Fprintf(w, "    not: %s\n", d.Exclusions)
						}
						for _, l := range d.Log {
							mark := ""
							if l.NeedsReview {
								mark = " [needs review]"
							}
							fmt.Fprintf(w, "    %s %s%s\n", l.At, l.Decision, mark)
						}
					}
				})
			})
		},
	}
	a.taskFlag(list)

	var excl, by string
	var dur time.Duration
	propose := &cobra.Command{
		Use: "propose <scope...>", Short: "Record a delegation the user stated (stays proposed until confirmed)", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := c.ProposeDelegation(ctx, actor, a.task, strings.Join(args, " "), excl, by, c.Now().Add(dur))
				if err != nil {
					return err
				}
				return a.done("Recorded "+id+".", map[string]string{"id": id})
			})
		},
	}
	a.taskFlag(propose)
	propose.Flags().StringVar(&excl, "exclusions", "", "what the delegation does not cover")
	propose.Flags().StringVar(&by, "granted-by", "user", "who granted it and where")
	propose.Flags().DurationVar(&dur, "for", 8*time.Hour, "how long it lasts")

	byID := func(use, short string, fn func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, args []string) error) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				args[0] = strings.ToUpper(args[0])
				return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
					if err := fn(ctx, c, actor, args); err != nil {
						return err
					}
					return a.done(args[0]+" updated.", nil)
				})
			}}
	}
	cmd.AddCommand(list, propose,
		byID("confirm <G-id>", "Activate a proposed delegation (user)", func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, args []string) error {
			return c.ConfirmDelegation(ctx, actor, args[0])
		}),
		byID("revoke <G-id>", "Revoke a delegation (user)", func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, args []string) error {
			return c.RevokeDelegation(ctx, actor, args[0])
		}),
		byID("review <G-id>", "Mark logged decisions reviewed (user)", func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, args []string) error {
			return c.ReviewDecisions(ctx, actor, args[0])
		}),
		byID("log <G-id> <decision...>", "Log a decision taken under an active delegation (lead)", func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, args []string) error {
			if len(args) < 2 {
				return errors.New("decision text is required")
			}
			return c.LogDecision(ctx, actor, args[0], strings.Join(args[1:], " "))
		}),
	)
	return cmd
}

func (a *app) effectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "effect", Short: "Side-effect intents and receipts"}
	var status string
	list := &cobra.Command{
		Use: "list", Short: "List side effects", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := a.taskFor(ctx, c, actor)
				if err != nil {
					return err
				}
				es, err := c.ListEffects(ctx, id, status)
				if err != nil {
					return err
				}
				return a.emit(es, func(w io.Writer) {
					var rows [][]string
					for _, e := range es {
						rows = append(rows, []string{e.ID, e.Status, trunc(e.Action, 30), trunc(e.Target, 30), trunc(e.Evidence, 30)})
					}
					table(w, "ID\tSTATUS\tACTION\tTARGET\tEVIDENCE", rows)
				})
			})
		},
	}
	a.taskFlag(list)
	list.Flags().StringVar(&status, "status", "", "filter by status")

	var target, approval, rollback string
	plan := &cobra.Command{
		Use: "plan <action...>", Short: "Record intent before an external or non-idempotent action", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := c.PlanEffect(ctx, actor, a.task, strings.Join(args, " "), target, strings.ToUpper(approval), rollback)
				if err != nil {
					return err
				}
				return a.done("Planned "+id+". Run `agent-shiftboard effect start "+id+"` immediately before the action.", map[string]string{"id": id})
			})
		},
	}
	a.taskFlag(plan)
	plan.Flags().StringVar(&target, "target", "", "exact target (required)")
	plan.Flags().StringVar(&approval, "approval", "", "approval P-id this action needs")
	plan.Flags().StringVar(&rollback, "rollback", "", "backup or rollback reference")

	var evidence string
	finish := &cobra.Command{
		Use: "finish <E-id> <confirmed|failed|not_performed>", Short: "Record the observed outcome", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				if err := c.FinishEffect(ctx, actor, strings.ToUpper(args[0]), args[1], evidence); err != nil {
					return err
				}
				return a.done(strings.ToUpper(args[0])+" is "+args[1]+".", nil)
			})
		},
	}
	finish.Flags().StringVar(&evidence, "evidence", "", "compact evidence reference (commit, URL, record ID)")
	reconcile := &cobra.Command{
		Use: "reconcile <E-id> <confirmed|failed|not_performed>", Short: "Resolve an unknown effect from a read-only check (lead)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				if err := c.ReconcileEffect(ctx, actor, strings.ToUpper(args[0]), args[1], evidence); err != nil {
					return err
				}
				return a.done(strings.ToUpper(args[0])+" reconciled as "+args[1]+".", nil)
			})
		},
	}
	reconcile.Flags().StringVar(&evidence, "evidence", "", "what the read-only check showed (required)")
	start := &cobra.Command{
		Use: "start <E-id>", Short: "Mark a planned effect in flight, immediately before the action", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				if err := c.StartEffect(ctx, actor, strings.ToUpper(args[0])); err != nil {
					return err
				}
				return a.done(strings.ToUpper(args[0])+" is in flight.", nil)
			})
		},
	}
	cmd.AddCommand(list, plan, start, finish, reconcile)
	return cmd
}

func (a *app) milestoneCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "milestone", Short: "Delivery milestones (commit, push, MR, build, deploy, verify)"}
	list := &cobra.Command{
		Use: "list", Short: "List milestones", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := a.taskFor(ctx, c, actor)
				if err != nil {
					return err
				}
				ms, err := c.ListMilestones(ctx, id)
				if err != nil {
					return err
				}
				return a.emit(ms, func(w io.Writer) {
					var rows [][]string
					for _, m := range ms {
						rows = append(rows, []string{m.ID, m.Kind, trunc(m.Target, 30), m.Status, trunc(m.Evidence, 30), m.ObservedAt})
					}
					table(w, "ID\tKIND\tTARGET\tSTATUS\tEVIDENCE\tOBSERVED", rows)
				})
			})
		},
	}
	a.taskFlag(list)
	var evidence string
	record := &cobra.Command{
		Use: "record <kind> <target> <status>", Short: "Record or update a milestone", Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := c.RecordMilestone(ctx, actor, a.task, args[0], args[1], args[2], evidence)
				if err != nil {
					return err
				}
				return a.done("Recorded "+id+".", map[string]string{"id": id})
			})
		},
	}
	a.taskFlag(record)
	record.Flags().StringVar(&evidence, "evidence", "", "evidence identifier")
	cmd.AddCommand(list, record)
	return cmd
}

func (a *app) delegateCmd() *cobra.Command {
	var in coord.AssignmentInput
	cmd := &cobra.Command{
		Use: "delegate <scope...>", Short: "Create an assignment for a worker session (lead)", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Scope = strings.Join(args, " ")
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				in.TaskID = a.task
				id, err := c.CreateAssignment(ctx, actor, in)
				if err != nil {
					return err
				}
				return a.done("Created assignment "+id+". Start it with `agent-shiftboard worker start "+id+" --provider claude|codex|hermes` (MCP: shiftboard_worker_start).", map[string]string{"id": id})
			})
		},
	}
	a.taskFlag(cmd)
	cmd.Flags().StringVar(&in.Expected, "expected", "", "expected output")
	cmd.Flags().StringVar(&in.RequiredTools, "requires", "", "tools and capabilities the worker needs")
	cmd.Flags().StringVar(&in.Constraints, "constraints", "", "shared constraints")
	cmd.Flags().StringVar(&in.Worktree, "worktree", "", "worktree for the worker")
	return cmd
}

func (a *app) assignmentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "assignment", Short: "Worker assignments"}
	list := &cobra.Command{
		Use: "list", Short: "List assignments", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := a.taskFor(ctx, c, actor)
				if err != nil {
					return err
				}
				as, err := c.ListAssignments(ctx, id)
				if err != nil {
					return err
				}
				return a.emit(as, func(w io.Writer) {
					var rows [][]string
					for _, x := range as {
						rows = append(rows, []string{x.ID, x.Status, x.Provider, trunc(x.Scope, 50), trunc(x.Result, 40)})
					}
					table(w, "ID\tSTATUS\tPROVIDER\tSCOPE\tRESULT", rows)
				})
			})
		},
	}
	a.taskFlag(list)
	var evidence, reason string
	submit := &cobra.Command{
		Use: "submit <A-id> <result...>", Short: "Submit a concise result (worker)", Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				if err := c.SubmitAssignment(ctx, actor, strings.ToUpper(args[0]), strings.Join(args[1:], " "), evidence); err != nil {
					return err
				}
				return a.done("Submitted.", nil)
			})
		},
	}
	submit.Flags().StringVar(&evidence, "evidence", "", "evidence references")
	review := func(use string, accept bool) *cobra.Command {
		c := &cobra.Command{
			Use: use + " <A-id>", Short: use + " a submitted result (lead)", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
					if err := c.ReviewAssignment(ctx, actor, strings.ToUpper(args[0]), accept, reason); err != nil {
						return err
					}
					return a.done(strings.ToUpper(args[0])+" "+use+"ed.", nil)
				})
			},
		}
		c.Flags().StringVar(&reason, "reason", "", "reason (required to reject)")
		return c
	}
	cancel := &cobra.Command{
		Use: "cancel <A-id>", Short: "Cancel an assignment (lead or user)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				if err := c.CancelAssignment(ctx, actor, strings.ToUpper(args[0]), reason); err != nil {
					return err
				}
				return a.done(strings.ToUpper(args[0])+" cancelled.", nil)
			})
		},
	}
	cancel.Flags().StringVar(&reason, "reason", "", "why")
	cmd.AddCommand(list, submit, review("accept", true), review("reject", false), cancel)
	return cmd
}

func (a *app) handoffCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "handoff", Short: "Lead handoff acknowledgement and confirmation"}
	readback := &cobra.Command{
		Use: "readback", Short: "List the IDs a new lead must acknowledge", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := a.taskFor(ctx, c, actor)
				if err != nil {
					return err
				}
				ids, err := c.RequiredReadback(ctx, id)
				if err != nil {
					return err
				}
				return a.emit(ids, func(w io.Writer) { fmt.Fprintln(w, strings.Join(ids, " ")) })
			})
		},
	}
	a.taskFlag(readback)
	ack := &cobra.Command{
		Use: "ack [IDs...]", Short: "Acknowledge a handoff by reading back required IDs (new lead)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				if err := c.AckHandoff(ctx, actor, args); err != nil {
					return err
				}
				return a.done("Handoff acknowledged; you hold the lead lease.", nil)
			})
		},
	}
	confirm := &cobra.Command{
		Use: "confirm <task>", Short: "Move the acknowledged lead into the main slot (user)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskArg(args[0])
			if err != nil {
				return err
			}
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				if err := a.confirmSwap(ctx, c, actor, id); err != nil {
					return err
				}
				return a.done("The lead is in the main slot.", nil)
			})
		},
	}
	cmd.AddCommand(readback, ack, confirm)
	return cmd
}

func (a *app) sessionCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "session", Short: "Managed agent sessions"}
	list := &cobra.Command{
		Use: "list", Short: "List sessions", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id := a.task
				if !actor.IsUser() || id > 0 {
					var err error
					id, err = c.ResolveTask(ctx, actor, id)
					if err != nil {
						return err
					}
				}
				ss, err := c.ListSessionViews(ctx, id)
				if err != nil {
					return err
				}
				return a.emit(ss, func(w io.Writer) {
					var rows [][]string
					for _, s := range ss {
						rows = append(rows, []string{s.ID, fmt.Sprintf("#%d", s.TaskID), s.Provider, s.Role, s.AssignmentID, s.Status, s.Reason, s.StatusSource, s.ObservedAt})
					}
					table(w, "SESSION\tTASK\tPROVIDER\tROLE\tASSIGNMENT\tSTATUS\tREASON\tSOURCE\tOBSERVED", rows)
				})
			})
		},
	}
	a.taskFlag(list)
	var reason, source string
	observe := &cobra.Command{
		Use: "observe <session-id> <status>", Short: "Record a status observation", Hidden: true, Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				return c.Observe(ctx, actor, args[0], args[1], reason, source)
			})
		},
	}
	observe.Flags().StringVar(&reason, "reason", "", "stop/failure reason")
	observe.Flags().StringVar(&source, "source", "user", "observation source")
	closeCmd := &cobra.Command{
		Use: "close <session|A-n|unique-prefix>", Short: "Close an idle (or unknown but at rest) worker or former lead (lead)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				sid, err := c.ResolveSession(ctx, actor, args[0])
				if err != nil {
					return err
				}
				ws := tmux.New(tmux.Exec{}, c.Root)
				if err := c.CloseSession(ctx, actor, sid, ws.Close, paneAtRest(ws)); err != nil {
					return err
				}
				return a.done("Closed session "+sid+".", map[string]string{"session": sid})
			})
		},
	}
	cmd.AddCommand(list, observe, closeCmd)
	return cmd
}
