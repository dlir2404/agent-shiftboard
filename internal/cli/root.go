// Package cli implements the agent-shiftboard command tree.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/paths"
	"github.com/dlir2404/agent-shiftboard/internal/render"
	"github.com/dlir2404/agent-shiftboard/internal/tmux"
)

// Version is set at build time.
var Version = "0.1.0-dev"

type app struct {
	out    io.Writer
	errOut io.Writer
	json   bool
	task   int64
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	a := &app{out: os.Stdout, errOut: os.Stderr}
	root := a.rootCmd()
	if err := root.Execute(); err != nil {
		if a.json {
			_ = json.NewEncoder(a.out).Encode(map[string]string{"error": err.Error()})
		} else {
			fmt.Fprintln(a.errOut, "error:", err)
		}
		return 1
	}
	return 0
}

func (a *app) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "agent-shiftboard",
		Short:         "Local control plane for independent coding-agent CLI sessions",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}
	root.PersistentFlags().BoolVar(&a.json, "json", false, "print JSON")
	root.AddCommand(
		a.initCmd(), a.doctorCmd(), a.runCmd(), a.taskCmd(), a.briefCmd(), a.ruleCmd(),
		a.pendingCmd(), a.delegationCmd(), a.effectCmd(), a.milestoneCmd(),
		a.delegateCmd(), a.assignmentCmd(), a.handoffCmd(), a.sessionCmd(),
		a.configCmd(), a.secretCmd(), a.toolCmd(), a.getCmd(), a.applyCmd(), a.lockCmd(),
		a.sweepCmd(), a.mcpCmd(), a.leadCmd(), a.boardCmd(), a.hookCmd(), a.openCmd(), a.workerCmd(), a.itemCmd(), a.panelCmd(),
	)
	root.AddCommand(a.startCmd())
	root.Use = "agent-shiftboard"
	root.SuggestionsMinimumDistance = 2
	root.RunE = a.rootRun
	// No positional arguments: a mistyped subcommand must be an error with
	// a suggestion, not a new task.
	root.Args = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return nil
		}
		msg := fmt.Sprintf("unknown command %q", args[0])
		if s := cmd.SuggestionsFor(args[0]); len(s) > 0 {
			msg += fmt.Sprintf("; did you mean %q?", s[0])
		}
		return fmt.Errorf("%s (run `agent-shiftboard --help`; to start working, run `agent-shiftboard` alone)", msg)
	}
	return root
}

// open finds the project and opens the coordinator. Every invocation also
// applies time-based rules (Sweep) and re-renders changed briefs.
func (a *app) open(ctx context.Context) (*coord.Coordinator, error) {
	cwd, _ := os.Getwd()
	root, err := paths.FindProject(cwd)
	if err != nil {
		return nil, err
	}
	c, err := coord.Open(root)
	if err != nil {
		return nil, err
	}
	if ud, err := paths.UserDir(); err == nil {
		_ = tmux.WriteConfig(filepath.Join(ud, "tmux.conf"))
	}
	c.OnTaskChange = func(id int64) {
		if err := render.WriteBrief(context.Background(), c, id); err != nil {
			fmt.Fprintln(a.errOut, "warning: could not render brief:", err)
		}
	}
	if err := c.Sweep(ctx); err != nil {
		fmt.Fprintln(a.errOut, "warning: sweep failed:", err)
	}
	return c, nil
}

// with opens the coordinator, runs fn, and closes it.
func (a *app) with(cmd *cobra.Command, fn func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	c, err := a.open(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(ctx, c, c.ResolveActor(ctx))
}

// taskFor resolves --task or the actor's own task.
func (a *app) taskFor(ctx context.Context, c *coord.Coordinator, actor coord.Actor) (int64, error) {
	if id, err := c.ResolveTask(ctx, actor, a.task); err == nil {
		return id, nil
	}
	// The user without --task: the most recently updated open task.
	ts, err := c.ListTasks(ctx)
	if err != nil {
		return 0, err
	}
	var best coord.Task
	for _, t := range ts {
		if (t.Status == "queued" || t.Status == "active" || t.Status == "blocked") && t.UpdatedAt > best.UpdatedAt {
			best = t
		}
	}
	if best.ID == 0 {
		return 0, errors.New("no open task; specify --task")
	}
	return best.ID, nil
}

func (a *app) taskFlag(cmd *cobra.Command) {
	cmd.Flags().Int64VarP(&a.task, "task", "t", 0, "task number (defaults to the calling session's task)")
}

// emit prints v as JSON with --json, otherwise calls human.
func (a *app) emit(v any, human func(w io.Writer)) error {
	if a.json {
		enc := json.NewEncoder(a.out)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	human(a.out)
	return nil
}

func (a *app) done(msg string, v any) error {
	return a.emit(v, func(w io.Writer) { fmt.Fprintln(w, msg) })
}

func table(w io.Writer, header string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

func parseTaskArg(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid task %q", s)
	}
	return n, nil
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
