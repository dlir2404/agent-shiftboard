package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/adapter"
	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/render"
	"github.com/dlir2404/agent-shiftboard/internal/tmux"
)

func (a *app) workerCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "worker", Short: "Worker sessions for assignments"}
	var provider string
	start := &cobra.Command{
		Use:   "start <A-id>",
		Short: "Start a worker session for an assignment in its own workspace window (lead)",
		Long: `Starts an independent CLI session (same or another provider) for an
assignment created with "agent-shiftboard delegate". The worker gets the
assignment, the rules in force, and the brief, and reports back with
"agent-shiftboard assignment submit". Its window is listed in the status line.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			aid := strings.ToUpper(args[0])
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				p, err := adapter.Get(provider)
				if err != nil {
					return err
				}
				if !tmux.Available() {
					return tmux.ErrNoTmux
				}
				ws := tmux.New(tmux.Exec{}, c.Root)
				if !ws.Exists() {
					return fmt.Errorf("the workspace is not running; start it with `agent-shiftboard`")
				}
				native := ""
				if p.PreassignsID() {
					native = uuid.NewString()
				}
				sid, err := c.RegisterWorker(ctx, actor, aid, coord.SessionSpec{Provider: provider, NativeSessionID: native,
					PermissionMode: c.Policy.Providers[provider].PermissionMode})
				if err != nil {
					return err
				}
				taskID, err := c.ResolveTask(ctx, coord.Actor{SessionID: sid}, 0)
				if err != nil {
					return err
				}
				as, err := c.ListAssignments(ctx, taskID)
				if err != nil {
					return err
				}
				var asg coord.Assignment
				for _, x := range as {
					if x.ID == aid {
						asg = x
					}
				}
				v, err := render.Load(ctx, c, taskID)
				if err != nil {
					return err
				}
				dir := sessionDir(c.Root, sid)
				ctxFile := filepath.Join(dir, "launch.md")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
				if err := os.WriteFile(ctxFile, []byte(render.WorkerContext(v, asg, sid)), 0o600); err != nil {
					return err
				}
				spec := a.launchSpec(c, sid, native, provider)
				spec.Role = "worker"
				spec.LaunchContext = ctxFile
				spec.InitialPrompt = "Begin your AgentShiftboard assignment " + aid + " now, following the instructions you were given."
				command, err := p.Launch(spec)
				if err != nil {
					return err
				}
				script, err := adapter.WriteScript(dir, "launch.sh", command)
				if err != nil {
					return err
				}
				name := fmt.Sprintf("%s %s", aid, provider)
				pane, err := ws.Launch(name, spec.Env(), []string{"/bin/sh", script})
				if err != nil {
					return err
				}
				_ = ws.KeepOnExit(pane)
				pid, _ := strconv.Atoi(mustOut(ws.R.Run("display-message", "-p", "-t", pane, "#{pane_pid}")))
				if err := c.SetSessionRuntime(ctx, sid, pane, pid, native); err != nil {
					return err
				}
				return a.done(fmt.Sprintf("Started a %s worker for %s in window %q. It reports back with `assignment submit`; you will see its result on your next turn (or run `agent-shiftboard assignment list`).", provider, aid, name),
					map[string]string{"session": sid, "pane": pane})
			})
		},
	}
	start.Flags().StringVar(&provider, "provider", "claude", "provider CLI for the worker (claude, codex)")
	cmd.AddCommand(start)
	return cmd
}
