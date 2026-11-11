package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/adapter"
	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/hooks"
	"github.com/dlir2404/agent-shiftboard/internal/tmux"
)

// hookCmd is the entry point provider hooks call. It never fails the
// provider CLI: errors go to stderr and the exit code stays 0.
func (a *app) hookCmd() *cobra.Command {
	return &cobra.Command{
		Use: "hook <provider> <event>", Short: "Provider hook handler", Hidden: true, Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := adapter.Get(args[0]); err != nil {
				return nil
			}
			ctx := cmd.Context()
			c, err := a.open(ctx)
			if err != nil {
				fmt.Fprintln(a.errOut, "agent-shiftboard hook:", err)
				return nil
			}
			defer c.Close()
			r := hooks.Handle(ctx, c, args[0], c.ResolveActor(ctx), args[1], os.Stdin, a.errOut, time.Now())
			if len(r.Stdout) > 0 {
				a.out.Write(r.Stdout)
			}
			if r.ExitCode != 0 {
				os.Exit(r.ExitCode)
			}
			return nil
		},
	}
}

// openWorkspace opens (creating if needed) the project's tmux session,
// shows the task's lead when taskID is set, and attaches.
func (a *app) openWorkspace(cmd *cobra.Command, taskID int64) error {
	if !tmux.Available() {
		return fmt.Errorf("%w; without it, use `agent-shiftboard board`", tmux.ErrNoTmux)
	}
	return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
		if !actor.IsUser() {
			return coord.ErrUserOnly
		}
		ws := tmux.New(tmux.Exec{}, c.Root)
		if _, err := ws.Ensure(); err != nil {
			return err
		}
		if taskID > 0 {
			if _, err := a.openLead(ctx, c, ws, taskID, "auto", "", true); err != nil {
				return err
			}
		}
		return attach(ws)
	})
}

func leadPane(ctx context.Context, c *coord.Coordinator, t coord.Task) string {
	ss, _ := c.ListSessions(ctx, t.ID)
	for _, s := range ss {
		if s.ID == t.LeadSession {
			return s.PaneID
		}
	}
	return ""
}

func (a *app) openCmd() *cobra.Command {
	return &cobra.Command{
		Use: "open <task>", Short: "Show a task's lead in the main slot and attach (user)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskArg(args[0])
			if err != nil {
				return err
			}
			return a.openWorkspace(cmd, id)
		},
	}
}

// confirmSwap records the confirmation and shows the lead's window
// (user operation).
func (a *app) confirmSwap(ctx context.Context, c *coord.Coordinator, actor coord.Actor, id int64) error {
	if err := c.ConfirmSwap(ctx, actor, id); err != nil {
		return err
	}
	t, err := c.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if t.LeaseState != "held" {
		return fmt.Errorf("task #%d has no acknowledged lead yet (lease %s)", id, t.LeaseState)
	}
	pane := leadPane(ctx, c, t)
	if pane == "" || !tmux.Available() {
		return fmt.Errorf("the lead of task #%d has no tmux pane", id)
	}
	return tmux.New(tmux.Exec{}, c.Root).Show(pane)
}

// reconcilePanes marks sessions whose tmux pane vanished or whose process
// exited without a SessionEnd hook. Called by the board on each refresh.
func reconcilePanes(ctx context.Context, c *coord.Coordinator) {
	if !tmux.Available() {
		return
	}
	ws := tmux.New(tmux.Exec{}, c.Root)
	if !ws.Exists() {
		return
	}
	ss, err := c.ListSessions(ctx, 0)
	if err != nil {
		return
	}
	for _, s := range ss {
		if s.PaneID == "" || s.Status == "stopped" || s.Status == "failed" {
			continue
		}
		if _, err := ws.R.Run("display-message", "-p", "-t", s.PaneID, "#{pane_id}"); err != nil {
			_ = c.Observe(ctx, coord.User, s.ID, "stopped", "host_lost", "process")
			continue
		}
		if alive, _ := ws.PaneAlive(s.PaneID); !alive {
			_ = c.Observe(ctx, coord.User, s.ID, "failed", "process_crash", "process")
		}
	}
}
