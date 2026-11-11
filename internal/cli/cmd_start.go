package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"

	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/adapter"
	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/paths"
	"github.com/dlir2404/agent-shiftboard/internal/tmux"
)

// startCmd runs in the main slot (or, without tmux, in the current
// terminal): it continues the latest open task (or a new draft task), runs the lead's native CLI right here so
// the user sees every native prompt, and offers resume or a new session
// when the CLI exits.
func (a *app) startCmd() *cobra.Command {
	var task int64
	var forceNew bool
	cmd := &cobra.Command{
		Use: "start", Short: "Run the lead in this terminal (used by the workspace)", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !coord.ActorFromEnv().IsUser() {
				return coord.ErrUserOnly
			}
			return a.startLoop(cmd.Context(), task, forceNew)
		},
	}
	cmd.Flags().Int64Var(&task, "task", 0, "task to lead")
	cmd.Flags().BoolVar(&forceNew, "new", false, "start a new lead session instead of resuming")
	return cmd
}

func (a *app) startLoop(ctx context.Context, taskID int64, forceNew bool) error {
	in := bufio.NewReader(os.Stdin)
	mode := "auto"
	if forceNew {
		mode = "new"
	}
	for {
		if taskID == 0 {
			id, err := a.defaultTask(ctx)
			if err != nil {
				return err
			}
			taskID = id
		}
		if err := a.runLeadHere(ctx, taskID, mode); err != nil {
			fmt.Fprintln(a.errOut, "\nerror:", err)
		}
		fmt.Fprint(a.out, "\nThe lead CLI exited. [r] resume · [n] new task · [t] switch task · [l] fresh lead session for this task · [q] shell: ")
		line, err := in.ReadString('\n')
		if err != nil {
			return execShell()
		}
		switch strings.TrimSpace(strings.ToLower(line)) {
		case "r", "":
			mode = "resume"
		case "n":
			id, err := a.newDraftTask(ctx)
			if err != nil {
				return err
			}
			taskID, mode = id, "new"
		case "t":
			id, err := a.pickTask(ctx, in, taskID)
			if err != nil {
				return err
			}
			taskID, mode = id, "auto"
		case "l":
			mode = "new"
		default:
			return execShell()
		}
	}
}

// defaultTask continues the most recently updated open task, or creates a
// draft task the lead describes once the user says what they want.
func (a *app) defaultTask(ctx context.Context) (int64, error) {
	c, err := a.open(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Close()
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
	if best.ID > 0 {
		return best.ID, nil
	}
	return c.CreateTask(ctx, coord.User, coord.DraftTitle, "(not described yet: ask the user what they want)")
}

func (a *app) newDraftTask(ctx context.Context) (int64, error) {
	c, err := a.open(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	return c.CreateTask(ctx, coord.User, coord.DraftTitle, "(not described yet: ask the user what they want)")
}

// pickTask lists open tasks and reads a task number.
func (a *app) pickTask(ctx context.Context, in *bufio.Reader, current int64) (int64, error) {
	c, err := a.open(ctx)
	if err != nil {
		return 0, err
	}
	ts, err := c.ListTasks(ctx)
	c.Close()
	if err != nil {
		return 0, err
	}
	var open []coord.Task
	for _, t := range ts {
		if t.Status == "queued" || t.Status == "active" || t.Status == "blocked" {
			open = append(open, t)
		}
	}
	if len(open) == 0 {
		return a.newDraftTask(ctx)
	}
	fmt.Fprintln(a.out, "\nOpen tasks:")
	for _, t := range open {
		fmt.Fprintf(a.out, "  %d) %s  (%s, lead %s)\n", t.ID, t.Title, t.Status, t.LeaseState)
	}
	for {
		fmt.Fprint(a.out, "Task number: ")
		line, err := in.ReadString('\n')
		if err != nil {
			return current, nil
		}
		n, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(line), "#"), 10, 64)
		if err == nil {
			for _, t := range open {
				if t.ID == n {
					return n, nil
				}
			}
		}
		fmt.Fprintln(a.out, "Not an open task.")
	}
}

func titleFrom(desc string) string {
	if r := []rune(desc); len(r) > 72 {
		return string(r[:71]) + "…"
	}
	return desc
}

// runLeadHere runs the lead's CLI as a child of this process in this
// terminal. mode: auto (resume the holder if possible, else new), resume,
// or new.
func (a *app) runLeadHere(ctx context.Context, taskID int64, mode string) error {
	c, err := a.open(ctx)
	if err != nil {
		return err
	}
	t, err := c.GetTask(ctx, taskID)
	if err != nil {
		c.Close()
		return err
	}
	if t.Status == "done" || t.Status == "cancelled" {
		c.Close()
		return fmt.Errorf("task #%d is %s; reopen it with `agent-shiftboard task reopen %d`", taskID, t.Status, taskID)
	}
	// A previous new session that never acknowledged (for example because
	// it was waiting on a prompt nobody could see) is abandoned.
	if t.LeaseState == "handoff_pending" {
		if err := c.CancelPendingHandoff(ctx, coord.User, taskID); err != nil {
			c.Close()
			return err
		}
		t, _ = c.GetTask(ctx, taskID)
	}
	holder := sessionByID(ctx, c, taskID, t.LeadSession)
	canResume := holder.ID != "" && holder.Provider == "claude" && holder.NativeSessionID != "" &&
		(t.LeaseState == "held" || t.LeaseState == "suspended")
	if mode == "resume" && !canResume {
		mode = "new"
	}
	if mode == "auto" {
		mode = map[bool]string{true: "resume", false: "new"}[canResume]
	}

	var sid string
	var command []string
	var spec adapter.LaunchSpec
	if mode == "resume" {
		sid = holder.ID
		spec = a.launchSpec(c, sid, holder.NativeSessionID, holder.Provider)
		if command, err = adapter.ClaudeResume(spec); err != nil {
			c.Close()
			return err
		}
		fmt.Fprintf(a.out, "Resuming the lead's Claude Code session for task #%d…\n", taskID)
	} else {
		provider := a.defaultProvider(c)
		if provider != "claude" {
			c.Close()
			return fmt.Errorf("the %s adapter is not implemented yet; set lead_priority to claude", provider)
		}
		native := uuid.NewString()
		sid, err = c.StartLead(ctx, coord.User, taskID, coord.SessionSpec{Provider: provider, NativeSessionID: native,
			PermissionMode: c.Policy.Providers[provider].PermissionMode})
		if err != nil {
			c.Close()
			return err
		}
		launch, _, err := writeLaunchContext(ctx, c, taskID, sid)
		if err != nil {
			c.Close()
			return err
		}
		spec = a.launchSpec(c, sid, native, provider)
		spec.LaunchContext = launch
		if command, err = adapter.ClaudeLaunch(spec); err != nil {
			c.Close()
			return err
		}
		fmt.Fprintf(a.out, "Starting Claude Code as the lead of task #%d.\n", taskID)
	}
	_ = c.SetSessionRuntime(ctx, sid, os.Getenv("TMUX_PANE"), 0, "")
	c.Close()

	args, err := resolveFileArgs(command)
	if err != nil {
		return err
	}
	child := exec.Command(args[0], args[1:]...)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	child.Env = os.Environ()
	for k, v := range spec.Env() {
		child.Env = append(child.Env, k+"="+v)
	}
	// The child owns the terminal; this process ignores interrupts meant for it.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	if err := child.Start(); err != nil {
		return err
	}
	if c, err := a.open(ctx); err == nil {
		_ = c.SetSessionRuntime(ctx, sid, "", child.Process.Pid, "")
		c.Close()
	}
	runErr := child.Wait()

	c, err = a.open(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	s := sessionByID(ctx, c, taskID, sid)
	switch {
	case s.Status == "stopped" || s.Status == "failed":
		// A SessionEnd hook already reported it.
	case runErr == nil:
		_ = c.Observe(ctx, coord.User, sid, "stopped", "user_stopped", "process")
	case s.Status == "starting":
		// It exited before reporting anything (for example the user
		// declined folder trust); the cause is not known.
		_ = c.Observe(ctx, coord.User, sid, "unknown", "unknown", "process")
	default:
		_ = c.Observe(ctx, coord.User, sid, "failed", "process_crash", "process")
	}
	return nil
}

func sessionByID(ctx context.Context, c *coord.Coordinator, taskID int64, sid string) coord.Session {
	if sid == "" {
		return coord.Session{}
	}
	ss, _ := c.ListSessions(ctx, taskID)
	for _, s := range ss {
		if s.ID == sid {
			return s
		}
	}
	return coord.Session{}
}

// resolveFileArgs replaces adapter.FileArg arguments with the file content.
func resolveFileArgs(cmd []string) ([]string, error) {
	out := make([]string, len(cmd))
	for i, a := range cmd {
		if strings.HasPrefix(a, adapter.FileArg) {
			b, err := os.ReadFile(strings.TrimPrefix(a, adapter.FileArg))
			if err != nil {
				return nil, err
			}
			a = string(b)
		}
		out[i] = a
	}
	return out, nil
}

func execShell() error {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	return syscall.Exec(sh, []string{sh, "-l"}, os.Environ())
}

// ensureProject finds the project or initializes the current directory.
func (a *app) ensureProject() (string, error) {
	cwd, _ := os.Getwd()
	root, err := paths.FindProject(cwd)
	if err == nil {
		return root, nil
	}
	if !errors.Is(err, paths.ErrNoProject) {
		return "", err
	}
	if err := initProject(cwd); err != nil {
		return "", err
	}
	fmt.Fprintf(a.errOut, "Initialized AgentShiftboard in %s\n", cwd)
	if !gitIgnores(cwd) {
		fmt.Fprintf(a.errOut, "Add %s/ to .gitignore: it holds machine-local state.\n", paths.StateDirName)
	}
	return cwd, nil
}

// rootRun is `agent-shiftboard [task description...]`: initialize if
// needed, open the workspace, and put the lead in the main slot.
func (a *app) rootRun(cmd *cobra.Command, args []string) error {
	if !coord.ActorFromEnv().IsUser() {
		return coord.ErrUserOnly
	}
	ctx := cmd.Context()
	if _, err := a.ensureProject(); err != nil {
		return err
	}
	var taskID int64
	if len(args) > 0 {
		c, err := a.open(ctx)
		if err != nil {
			return err
		}
		desc := strings.Join(args, " ")
		taskID, err = c.CreateTask(ctx, coord.User, titleFrom(desc), desc)
		c.Close()
		if err != nil {
			return err
		}
	}
	if !tmux.Available() {
		fmt.Fprintln(a.errOut, "tmux not found: running the lead in this terminal. Install tmux (brew install tmux) for the workspace with the status board; meanwhile run `agent-shiftboard board` in another terminal.")
		return a.startLoop(ctx, taskID, false)
	}
	c, err := a.open(ctx)
	if err != nil {
		return err
	}
	ws := tmux.New(tmux.Exec{}, c.Root)
	start := []string{ws.Self, "start"}
	if taskID > 0 {
		start = append(start, "--task", strconv.FormatInt(taskID, 10))
	}
	created, err := ws.Ensure(start)
	if err == nil && !created && taskID > 0 {
		err = a.placeStart(ctx, c, ws, start)
	}
	c.Close()
	if err != nil {
		return err
	}
	return attach(ws)
}

// placeStart runs a start loop in the main slot, unless a live lead is
// there; then it opens it in a new, selected window.
func (a *app) placeStart(ctx context.Context, c *coord.Coordinator, ws tmux.Workspace, start []string) error {
	if !mainHasLiveLead(ctx, c, ws) {
		return ws.RunInMain(start)
	}
	if _, err := ws.Launch("task", map[string]string{"AGENT_SHIFTBOARD_PROJECT": c.Root}, start); err != nil {
		return err
	}
	_, err := ws.R.Run("select-window", "-t", ws.Session+":task")
	return err
}

func mainHasLiveLead(ctx context.Context, c *coord.Coordinator, ws tmux.Workspace) bool {
	main, err := ws.MainPane()
	if err != nil {
		return false
	}
	ss, _ := c.ListSessions(ctx, 0)
	for _, s := range ss {
		if s.PaneID == main && s.Status != "stopped" && s.Status != "failed" {
			alive, _ := ws.PaneAlive(main)
			return alive
		}
	}
	return false
}
