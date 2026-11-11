package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/adapter"
	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/paths"
	"github.com/dlir2404/agent-shiftboard/internal/render"
	"github.com/dlir2404/agent-shiftboard/internal/tmux"
)

const initialPrompt = "You were started by AgentShiftboard. Follow the AgentShiftboard instructions in your system prompt: acknowledge the handoff with the readback command first, then read the brief and reply with one line stating the next action. Do not take any other action yet."

func (a *app) leadCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "lead", Short: "Start, resume, or switch a task's lead session (user)"}
	var provider string
	var registerOnly, noWait bool
	newCmd := &cobra.Command{
		Use:   "new <task>",
		Short: "Start a new lead session from the brief (user)",
		Long: `Starts the provider's native CLI in a hidden tmux window with the brief,
rules, open items, and resource index. After the session acknowledges by
readback, it moves into the main slot. Choosing this command is your
confirmation for that swap.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskArg(args[0])
			if err != nil {
				return err
			}
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				return a.startLead(ctx, c, actor, id, provider, registerOnly, noWait)
			})
		},
	}
	newCmd.Flags().StringVar(&provider, "provider", "", "provider CLI (default: first of lead_priority, else claude)")
	newCmd.Flags().BoolVar(&registerOnly, "register-only", false, "register the session and write its launch context without launching")
	newCmd.Flags().BoolVar(&noWait, "no-wait", false, "do not wait for the acknowledgement; confirm the swap later with `handoff confirm`")

	switchCmd := &cobra.Command{
		Use: "switch <provider> <task>", Short: "Start a new lead of another provider (user)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskArg(args[1])
			if err != nil {
				return err
			}
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				return a.startLead(ctx, c, actor, id, args[0], false, noWait)
			})
		},
	}
	switchCmd.Flags().BoolVar(&noWait, "no-wait", false, "do not wait for the acknowledgement")

	resume := &cobra.Command{
		Use: "resume <task>", Short: "Resume the lead's own native session in the main slot (user)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskArg(args[0])
			if err != nil {
				return err
			}
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				return a.resumeLead(ctx, c, actor, id)
			})
		},
	}
	cmd.AddCommand(newCmd, switchCmd, resume)
	return cmd
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

func (a *app) startLead(ctx context.Context, c *coord.Coordinator, actor coord.Actor, id int64, provider string, registerOnly, noWait bool) error {
	if provider == "" {
		provider = a.defaultProvider(c)
	}
	if !registerOnly {
		if provider != "claude" {
			return fmt.Errorf("the %s adapter is not implemented yet (Claude Code first; Codex in Phase 3, agy and hermes in Phase 5)", provider)
		}
		if !tmux.Available() {
			return tmux.ErrNoTmux
		}
	}
	native := ""
	if provider == "claude" {
		native = uuid.NewString()
	}
	sid, err := c.StartLead(ctx, actor, id, coord.SessionSpec{Provider: provider, NativeSessionID: native,
		PermissionMode: c.Policy.Providers[provider].PermissionMode})
	if err != nil {
		return err
	}
	launch, text, err := writeLaunchContext(ctx, c, id, sid)
	if err != nil {
		return err
	}
	if registerOnly {
		return a.emit(map[string]string{"session": sid, "launch_context": launch}, func(w io.Writer) {
			fmt.Fprintf(w, "Registered lead session %s for task #%d.\nLaunch context: %s\nStart the CLI yourself with %s=%s set.\n",
				sid, id, launch, coord.SessionEnv, sid)
		})
	}
	ws := tmux.New(tmux.Exec{}, c.Root)
	if err := ws.Ensure(); err != nil {
		return err
	}
	spec := a.launchSpec(c, sid, native, provider)
	spec.LaunchContext, spec.InitialPrompt = text, initialPrompt
	command, err := adapter.ClaudeLaunch(spec)
	if err != nil {
		return err
	}
	pane, err := ws.Launch("lead-"+sid[:8], spec.Env(), command)
	if err != nil {
		return err
	}
	_ = ws.KeepOnExit(pane)
	if err := c.SetSessionRuntime(ctx, sid, pane, 0, native); err != nil {
		return err
	}
	fmt.Fprintf(a.errOut, "Started %s lead session %s for task #%d in a hidden window (pane %s).\n", provider, sid, id, pane)
	if noWait {
		fmt.Fprintf(a.errOut, "After it acknowledges, run `agent-shiftboard handoff confirm %d` to show it in the main slot.\n", id)
		return nil
	}
	return a.waitAndSwap(ctx, c, ws, id, sid, pane)
}

// waitAndSwap waits for the readback acknowledgement, then moves the pane
// into the main slot. Starting the session was the user's confirmation.
func (a *app) waitAndSwap(ctx context.Context, c *coord.Coordinator, ws tmux.Workspace, id int64, sid, pane string) error {
	deadline := time.Now().Add(time.Duration(c.Policy.HandoffAckTimeoutSeconds) * time.Second)
	fmt.Fprint(a.errOut, "Waiting for the readback acknowledgement")
	for time.Now().Before(deadline) {
		t, err := c.GetTask(ctx, id)
		if err != nil {
			return err
		}
		if t.LeaseState == "held" && t.LeadSession == sid {
			fmt.Fprintln(a.errOut, " — acknowledged.")
			if err := ws.SwapIntoMain(pane); err != nil {
				return err
			}
			_ = c.ConfirmSwap(ctx, coord.User, id)
			return a.done(fmt.Sprintf("Session %s is the lead of task #%d and is in the main slot.", sid, id), map[string]string{"session": sid, "pane": pane})
		}
		if t.PendingLead != sid {
			fmt.Fprintln(a.errOut)
			return fmt.Errorf("the handoff to %s did not complete (lease is %s); see `agent-shiftboard task show %d`", sid, t.LeaseState, id)
		}
		fmt.Fprint(a.errOut, ".")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		_ = c.Sweep(ctx)
	}
	fmt.Fprintln(a.errOut)
	return fmt.Errorf("no acknowledgement within %ds; look at the session's window, then use `agent-shiftboard handoff confirm %d` or start another lead", c.Policy.HandoffAckTimeoutSeconds, id)
}

func (a *app) resumeLead(ctx context.Context, c *coord.Coordinator, actor coord.Actor, id int64) error {
	if !actor.IsUser() {
		return coord.ErrUserOnly
	}
	if !tmux.Available() {
		return tmux.ErrNoTmux
	}
	t, err := c.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if t.LeadSession == "" || (t.LeaseState != "held" && t.LeaseState != "suspended") {
		return fmt.Errorf("task #%d has no lead to resume (lease %s); start one with `agent-shiftboard lead new %d`", id, t.LeaseState, id)
	}
	ss, err := c.ListSessions(ctx, id)
	if err != nil {
		return err
	}
	var s coord.Session
	for _, x := range ss {
		if x.ID == t.LeadSession {
			s = x
		}
	}
	if s.Provider != "claude" {
		return fmt.Errorf("resume for %s is not implemented yet", s.Provider)
	}
	ws := tmux.New(tmux.Exec{}, c.Root)
	if err := ws.Ensure(); err != nil {
		return err
	}
	spec := a.launchSpec(c, s.ID, s.NativeSessionID, s.Provider)
	command, err := adapter.ClaudeResume(spec)
	if err != nil {
		return err
	}
	if alive, _ := ws.PaneAlive(s.PaneID); s.PaneID != "" && !alive {
		if err := ws.Respawn(s.PaneID, spec.Env(), command); err == nil {
			_ = ws.SwapIntoMainIfNeeded(s.PaneID)
			return a.done("Resumed the lead's native session in its pane.", nil)
		}
	} else if alive {
		_ = ws.SwapIntoMainIfNeeded(s.PaneID)
		return a.done("The lead's session is still running; it is in the main slot.", nil)
	}
	pane, err := ws.Launch("lead-"+s.ID[:8], spec.Env(), command)
	if err != nil {
		return err
	}
	_ = ws.KeepOnExit(pane)
	if err := c.SetSessionRuntime(ctx, s.ID, pane, 0, ""); err != nil {
		return err
	}
	if err := ws.SwapIntoMain(pane); err != nil {
		return err
	}
	return a.done("Resumed the lead's native session in a new pane in the main slot.", nil)
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
	return syscall.Exec(bin, args, os.Environ())
}
