package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/detect"
	"github.com/dlir2404/agent-shiftboard/internal/paths"
	"github.com/dlir2404/agent-shiftboard/internal/policy"
	"github.com/dlir2404/agent-shiftboard/internal/render"
	"github.com/dlir2404/agent-shiftboard/internal/store"
)

func (a *app) initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create local AgentShiftboard state in the current directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := coord.ActorFromEnv(); !err.IsUser() {
				return coord.ErrUserOnly
			}
			root, _ := os.Getwd()
			dir := paths.StateDir(root)
			if err := initProject(root); err != nil {
				return err
			}
			ignored := gitIgnores(root)
			return a.emit(map[string]any{"root": root, "state_dir": dir, "gitignored": ignored}, func(w io.Writer) {
				fmt.Fprintf(w, "Initialized %s\n", dir)
				if !ignored {
					fmt.Fprintf(w, "Add %s/ to .gitignore: it holds machine-local state.\n", paths.StateDirName)
				}
				fmt.Fprintln(w, "Next: `agent-shiftboard doctor`, then `agent-shiftboard run \"<task>\"`.")
			})
		},
	}
}

// initProject creates the state directory, sample policy, and database.
func initProject(root string) error {
	dir := paths.StateDir(root)
	for _, d := range []string{dir, filepath.Join(dir, "tools")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	cfg := filepath.Join(dir, "config.toml")
	if _, err := os.Stat(cfg); os.IsNotExist(err) {
		if err := os.WriteFile(cfg, []byte(policy.SampleProjectConfig), 0o600); err != nil {
			return err
		}
	}
	db, err := store.Open(paths.ProjectDB(root))
	if err != nil {
		return err
	}
	return db.Close()
}

func gitIgnores(root string) bool {
	b, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == paths.StateDirName || l == paths.StateDirName+"/" || l == "/"+paths.StateDirName+"/" {
			return true
		}
	}
	return false
}

func (a *app) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "doctor",
		Aliases: []string{"agents"},
		Short:   "Detect installed agent CLIs and required tools (no model calls)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			results := detect.All(ctx)
			tools := []detect.Result{detect.Tool(ctx, "tmux", "-V"), detect.Tool(ctx, "git", "--version")}
			return a.emit(map[string]any{"providers": results, "tools": tools}, func(w io.Writer) {
				var rows [][]string
				for _, r := range results {
					rows = append(rows, []string{r.Name, r.Status, r.Version, fmt.Sprintf("phase %d", r.Phase)})
				}
				table(w, "PROVIDER\tSTATUS\tVERSION\tADAPTER", rows)
				fmt.Fprintln(w)
				rows = nil
				for _, r := range tools {
					rows = append(rows, []string{r.Name, r.Status, r.Version})
				}
				table(w, "TOOL\tSTATUS\tVERSION", rows)
				for _, r := range tools {
					if r.Name == "tmux" && r.Status != "available" {
						fmt.Fprintln(w, "\ntmux is required for the workspace and interactive sessions: brew install tmux")
					}
				}
			})
		},
	}
}

func (a *app) runCmd() *cobra.Command {
	var goal string
	cmd := &cobra.Command{
		Use:   "run <title>",
		Short: "Create a task (user)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := c.CreateTask(ctx, actor, strings.Join(args, " "), goal)
				if err != nil {
					return err
				}
				return a.emit(map[string]any{"task": id, "brief": render.BriefPath(c.Root, id)}, func(w io.Writer) {
					fmt.Fprintf(w, "Created task #%d. Brief: %s\n", id, render.BriefPath(c.Root, id))
				})
			})
		},
	}
	cmd.Flags().StringVar(&goal, "goal", "", "the outcome the user asked for (defaults to the title)")
	return cmd
}

func (a *app) taskCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "task", Short: "Inspect and change tasks"}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List tasks", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, _ coord.Actor) error {
				ts, err := c.ListTasks(ctx)
				if err != nil {
					return err
				}
				return a.emit(ts, func(w io.Writer) {
					var rows [][]string
					for _, t := range ts {
						rows = append(rows, []string{fmt.Sprintf("#%d", t.ID), t.Status, t.LeaseState, fmt.Sprintf("r%d", t.BriefRevision), trunc(t.Title, 60)})
					}
					table(w, "TASK\tSTATUS\tLEASE\tBRIEF\tTITLE", rows)
				})
			})
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "show <task>", Short: "Show a task", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskArg(args[0])
			if err != nil {
				return err
			}
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, _ coord.Actor) error {
				t, err := c.GetTask(ctx, id)
				if err != nil {
					return err
				}
				ss, err := c.ListSessions(ctx, id)
				if err != nil {
					return err
				}
				return a.emit(map[string]any{"task": t, "sessions": ss}, func(w io.Writer) {
					fmt.Fprintf(w, "#%d %s\nstatus: %s\nlease: %s", t.ID, t.Title, t.Status, t.LeaseState)
					if t.LeadSession != "" {
						fmt.Fprintf(w, " (lead %s)", t.LeadSession)
					}
					if t.PendingLead != "" {
						fmt.Fprintf(w, " (pending %s)", t.PendingLead)
					}
					fmt.Fprintf(w, "\nbrief: r%d %s\n", t.BriefRevision, render.BriefPath(c.Root, id))
					for _, s := range ss {
						fmt.Fprintf(w, "session %s %s %s %s %s\n", s.ID, s.Provider, s.Role, s.Status, s.Reason)
					}
				})
			})
		},
	})
	newTask := &cobra.Command{
		Use: "new <title...>", Short: "Create another task (user or lead); it has no lead until one is started", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := c.CreateTask(ctx, actor, strings.Join(args, " "), "")
				if err != nil {
					return err
				}
				return a.done(fmt.Sprintf("Created task #%d.", id), map[string]int64{"task": id})
			})
		},
	}
	var dTitle, dGoal string
	describe := &cobra.Command{
		Use: "describe <task>", Short: "Set a task's title and goal once the user has described it (lead)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskArg(args[0])
			if err != nil {
				return err
			}
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				if err := c.DescribeTask(ctx, actor, id, dTitle, dGoal); err != nil {
					return err
				}
				return a.done(fmt.Sprintf("Task #%d described.", id), nil)
			})
		},
	}
	describe.Flags().StringVar(&dTitle, "title", "", "short title (required)")
	describe.Flags().StringVar(&dGoal, "goal", "", "the outcome the user asked for")
	cmd.AddCommand(newTask, describe)
	simple := func(use, short string, fn func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, id int64, args []string) error) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := parseTaskArg(args[0])
				if err != nil {
					return err
				}
				return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
					if err := fn(ctx, c, actor, id, args[1:]); err != nil {
						return err
					}
					return a.done(fmt.Sprintf("Task #%d updated.", id), map[string]any{"task": id})
				})
			}}
	}
	cmd.AddCommand(
		simple("block <task> <blocker...>", "Mark a task blocked (lead)", func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, id int64, rest []string) error {
			return c.BlockTask(ctx, actor, id, strings.Join(rest, " "))
		}),
		simple("unblock <task>", "Return a blocked task to active (lead)", func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, id int64, _ []string) error {
			return c.UnblockTask(ctx, actor, id)
		}),
		simple("close <task>", "Mark a task done after recording done_when (lead)", func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, id int64, _ []string) error {
			return c.CloseTask(ctx, actor, id)
		}),
		simple("cancel <task>", "Cancel a task (user)", func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, id int64, _ []string) error {
			return c.CancelTask(ctx, actor, id)
		}),
		simple("reopen <task>", "Reopen a done or cancelled task (user)", func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, id int64, _ []string) error {
			return c.ReopenTask(ctx, actor, id)
		}),
	)
	return cmd
}

func (a *app) sweepCmd() *cobra.Command {
	return &cobra.Command{
		Use: "sweep", Short: "Apply time-based status rules now", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(context.Context, *coord.Coordinator, coord.Actor) error { return nil })
		},
	}
}
