package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/adapter"
	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/paths"
	"github.com/dlir2404/agent-shiftboard/internal/render"
	"github.com/dlir2404/agent-shiftboard/internal/tmux"
)

func (a *app) leadCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "lead", Short: "Start, resume, or switch a task's lead session (user)"}
	var provider string
	var registerOnly bool
	newCmd := &cobra.Command{
		Use:   "new <task>",
		Short: "Start a new lead session from the brief in its own workspace window (user)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskArg(args[0])
			if err != nil {
				return err
			}
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				if registerOnly {
					return a.registerLeadOnly(ctx, c, actor, id, provider)
				}
				return a.openLeadCmd(ctx, c, actor, id, "new", provider)
			})
		},
	}
	newCmd.Flags().StringVar(&provider, "provider", "", "provider CLI (default: first of lead_priority, else claude)")
	newCmd.Flags().BoolVar(&registerOnly, "register-only", false, "register the session and write its launch context without launching")

	switchCmd := &cobra.Command{
		Use: "switch <provider> <task>", Short: "Hand the task to a new lead of another provider (user)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskArg(args[1])
			if err != nil {
				return err
			}
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				return a.openLeadCmd(ctx, c, actor, id, "new", args[0])
			})
		},
	}
	resume := &cobra.Command{
		Use: "resume <task>", Short: "Resume the lead's own native session (user)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskArg(args[0])
			if err != nil {
				return err
			}
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				return a.openLeadCmd(ctx, c, actor, id, "resume", "")
			})
		},
	}
	cmd.AddCommand(newCmd, switchCmd, resume)
	return cmd
}

func (a *app) openLeadCmd(ctx context.Context, c *coord.Coordinator, actor coord.Actor, id int64, mode, provider string) error {
	if !actor.IsUser() {
		return coord.ErrUserOnly
	}
	if provider != "" {
		if _, err := adapter.Get(provider); err != nil {
			return err
		}
	}
	if !tmux.Available() {
		return tmux.ErrNoTmux
	}
	ws := tmux.New(tmux.Exec{}, c.Root)
	if _, err := ws.Ensure(); err != nil {
		return err
	}
	if _, err := a.openLead(ctx, c, ws, id, mode, provider, true); err != nil {
		return err
	}
	return a.done(fmt.Sprintf("The lead of task #%d is starting in its own window. Run `agent-shiftboard` to open the workspace.", id), map[string]any{"task": id})
}

// openLead shows the task's live lead, or opens a window that runs the
// lead (start --task) and returns its pane. mode: auto | resume | new.
func (a *app) openLead(ctx context.Context, c *coord.Coordinator, ws tmux.Workspace, id int64, mode, provider string, show bool) (string, error) {
	t, err := c.GetTask(ctx, id)
	if err != nil {
		return "", err
	}
	if mode != "new" {
		if pane := leadPane(ctx, c, t); pane != "" {
			if alive, _ := ws.PaneAlive(pane); alive {
				if show {
					return pane, ws.Show(pane)
				}
				return pane, nil
			}
		}
	}
	cmd := []string{ws.Self, "start", "--task", strconv.FormatInt(id, 10), "--mode", mode}
	if provider != "" {
		cmd = append(cmd, "--provider", provider)
	}
	// The start loop is a user process: no agent session variable.
	return ws.OpenWindow(fmt.Sprintf("#%d lead", id), map[string]string{"AGENT_SHIFTBOARD_PROJECT": c.Root}, cmd, show)
}

func (a *app) registerLeadOnly(ctx context.Context, c *coord.Coordinator, actor coord.Actor, id int64, provider string) error {
	if provider == "" {
		provider = a.defaultProvider(c)
	}
	native := ""
	if p, err := adapter.Get(provider); err == nil && p.PreassignsID() {
		native = uuid.NewString()
	}
	sid, err := c.StartLead(ctx, actor, id, coord.SessionSpec{Provider: provider, NativeSessionID: native,
		PermissionMode: c.Policy.Providers[provider].PermissionMode})
	if err != nil {
		return err
	}
	launch, _, err := writeLaunchContext(ctx, c, id, sid)
	if err != nil {
		return err
	}
	return a.emit(map[string]string{"session": sid, "launch_context": launch}, func(w io.Writer) {
		fmt.Fprintf(w, "Registered lead session %s for task #%d.\nLaunch context: %s\nStart the CLI yourself with %s=%s set.\n",
			sid, id, launch, coord.SessionEnv, sid)
	})
}

func (a *app) defaultProvider(c *coord.Coordinator) string {
	if len(c.Policy.LeadPriority) > 0 {
		return c.Policy.LeadPriority[0]
	}
	return "claude"
}

func sessionDir(root, sid string) string {
	return filepath.Join(paths.StateDir(root), "sessions", sid)
}

func (a *app) launchSpec(c *coord.Coordinator, sid, native, provider string) adapter.LaunchSpec {
	ws := tmux.New(tmux.Exec{}, c.Root)
	return adapter.LaunchSpec{
		SessionID: sid, NativeID: native, Root: c.Root, Self: ws.Self,
		SessionDir: sessionDir(c.Root, sid), PermissionMode: c.Policy.Providers[provider].PermissionMode,
	}
}

// writeLaunchContext writes the text a new session receives at launch and
// returns its path and content.
func writeLaunchContext(ctx context.Context, c *coord.Coordinator, taskID int64, sid string) (string, string, error) {
	v, err := render.Load(ctx, c, taskID)
	if err != nil {
		return "", "", err
	}
	ids, err := c.RequiredReadback(ctx, taskID)
	if err != nil {
		return "", "", err
	}
	text := render.LaunchContext(v, sid, ids)
	p := filepath.Join(sessionDir(c.Root, sid), "launch.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", "", err
	}
	return p, text, os.WriteFile(p, []byte(text), 0o600)
}

// attach replaces this process with tmux attached to the workspace.
func attach(ws tmux.Workspace) error {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return tmux.ErrNoTmux
	}
	args := append([]string{"tmux"}, ws.AttachArgs()...)
	if os.Getenv(coord.SessionEnv) != "" {
		return errors.New("refusing to attach from inside a managed agent session")
	}
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "TMUX=") {
			env = append(env, e)
		}
	}
	return syscall.Exec(bin, args, env)
}

func mustOut(s string, err error) string {
	if err != nil {
		return ""
	}
	return s
}
