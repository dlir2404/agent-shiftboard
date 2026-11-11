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
	var mode, provider string
	cmd := &cobra.Command{
		Use: "start", Short: "Run the lead in this terminal (used by the workspace)", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !coord.ActorFromEnv().IsUser() {
				return coord.ErrUserOnly
			}
			if forceNew {
				mode = "new"
			}
			return a.startOnce(cmd.Context(), task, mode, provider)
		},
	}
	cmd.Flags().Int64Var(&task, "task", 0, "task to lead")
	cmd.Flags().BoolVar(&forceNew, "new", false, "start a new lead session instead of resuming")
	cmd.Flags().StringVar(&mode, "mode", "auto", "auto | resume | new")
	cmd.Flags().StringVar(&provider, "provider", "", "provider for a new lead session")
	return cmd
}

// startLoop is kept for the no-tmux path; each round re-executes the
// binary so a long-lived workspace always runs the installed version.
func (a *app) startLoop(ctx context.Context, taskID int64, forceNew bool) error {
	mode := "auto"
	if forceNew {
		mode = "new"
	}
	return a.startOnce(ctx, taskID, mode, "")
}

// startOnce runs the lead once, shows the exit menu, and re-executes
// `agent-shiftboard start` with the choice. Re-executing (instead of
// looping in this process) picks up a newly installed binary.
func (a *app) startOnce(ctx context.Context, taskID int64, mode, provider string) error {
	in := bufio.NewReader(os.Stdin)
	if taskID == 0 {
		id, err := a.defaultTask(ctx)
		if err != nil {
			return err
		}
		taskID = id
	}
	if err := a.runLeadHere(ctx, taskID, mode, provider); err != nil {
		fmt.Fprintln(a.errOut, "\nerror:", err)
	}
	provider = ""
	for {
		fmt.Fprint(a.out, "\nThe lead CLI exited. [r] resume · [s] switch lead to another CLI · [n] new task · [t] switch task · [l] fresh lead session · [q] shell: ")
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
		case "s":
			fmt.Fprintf(a.out, "New lead CLI (%s): ", strings.Join(adapter.Names(), ", "))
			name, _ := in.ReadString('\n')
			name = strings.TrimSpace(name)
			if _, err := adapter.Get(name); err != nil {
				fmt.Fprintln(a.errOut, err)
				continue
			}
			provider, mode = name, "new"
		default:
			return execShell()
		}
		return execSelf("start", "--task", strconv.FormatInt(taskID, 10), "--mode", mode, "--provider", provider)
	}
}

// execSelf replaces this process with the currently installed binary.
func execSelf(args ...string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(self, append([]string{self}, args...), os.Environ())
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
func (a *app) runLeadHere(ctx context.Context, taskID int64, mode, provider string) error {
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
	_, adapterErr := adapter.Get(holder.Provider)
	canResume := holder.ID != "" && adapterErr == nil && holder.NativeSessionID != "" &&
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
		p, _ := adapter.Get(holder.Provider)
		sid = holder.ID
		spec = a.launchSpec(c, sid, holder.NativeSessionID, holder.Provider)
		if command, err = p.Resume(spec); err != nil {
			c.Close()
			return err
		}
		fmt.Fprintf(a.out, "Resuming the lead's %s session for task #%d…\n", holder.Provider, taskID)
	} else {
		if provider == "" {
			provider = a.defaultProvider(c)
		}
		p, err := adapter.Get(provider)
		if err != nil {
			c.Close()
			return err
		}
		native := ""
		if p.PreassignsID() {
			native = uuid.NewString()
		}
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
		spec.Role, spec.LaunchContext = "lead", launch
		if command, err = p.Launch(spec); err != nil {
			c.Close()
			return err
		}
		if holder.ID != "" && holder.Provider != provider {
			fmt.Fprintf(a.out, "Handing task #%d from %s to %s. The new lead reads the brief, rules, and open items first.\n", taskID, holder.Provider, provider)
		} else {
			fmt.Fprintf(a.out, "Starting %s as the lead of task #%d.\n", provider, taskID)
		}
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
	if !tmux.Available() {
		fmt.Fprintln(a.errOut, "tmux not found: running the lead in this terminal. Install tmux (brew install tmux) for the workspace with the status board; meanwhile run `agent-shiftboard board` in another terminal.")
		return a.startLoop(ctx, taskID, false)
	}
	c, err := a.open(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	ws := tmux.New(tmux.Exec{}, c.Root)
	if _, err := ws.Ensure(); err != nil {
		return err
	}
	// First run (no open task): go straight into a lead, like `claude`.
	// Otherwise land on the session list.
	if !hasOpenTask(ctx, c) {
		id, err := c.CreateTask(ctx, coord.User, coord.DraftTitle, "(not described yet: ask the user what they want)")
		if err != nil {
			return err
		}
		if _, err := a.openLead(ctx, c, ws, id, "new", "", true); err != nil {
			return err
		}
	} else {
		_ = ws.Home()
	}
	c.Close()
	return attach(ws)
}

func hasOpenTask(ctx context.Context, c *coord.Coordinator) bool {
	ts, _ := c.ListTasks(ctx)
	for _, t := range ts {
		if t.Status == "queued" || t.Status == "active" || t.Status == "blocked" {
			return true
		}
	}
	return false
}
