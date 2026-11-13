package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/adapter"
	"github.com/dlir2404/agent-shiftboard/internal/board"
	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/tmux"
)

func (a *app) boardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "board",
		Short: "Open the session list (user)",
		Long:  "The session list shows sessions, their progress, and what needs you. In the tmux workspace it is the left pane; Shift+Up (or space) opens a session's actions panel.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := a.wireWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			if tmux.ConfigPath != "" && tmux.Available() && os.Getenv("TMUX") != "" {
				if ws := tmux.New(tmux.Exec{}, root); ws.Exists() {
					_, _ = ws.R.Run("source-file", tmux.ConfigPath) // keys from an upgrade
				}
			}
			board.Refresh = func(ctx context.Context, c *coord.Coordinator) {
				reconcilePanes(ctx, c)
				keepMCPServer(ctx, c)
			}
			return board.Run(func(ctx context.Context) (*coord.Coordinator, error) { return a.open(ctx) }, a.task)
		},
	}
	a.taskFlag(cmd)
	return cmd
}

func (a *app) panelCmd() *cobra.Command {
	var view string
	cmd := &cobra.Command{
		Use: "panel", Short: "Actions panel for one session (opened from the list)", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := a.wireWorkspace(cmd.Context()); err != nil {
				return err
			}
			return board.RunPanel(func(ctx context.Context) (*coord.Coordinator, error) { return a.open(ctx) }, a.task, view)
		},
	}
	a.taskFlag(cmd)
	cmd.Flags().StringVar(&view, "view", "", "view to open (Overview, Checklist, Agents, Needs you)")
	return cmd
}

// actionsCmd opens the actions panel of the session shown where the user
// is (bound to Shift+Up): the task of the focused pane, or of the session
// on the right when the list has focus, else the most recent open task.
func (a *app) actionsCmd() *cobra.Command {
	var pane string
	cmd := &cobra.Command{
		Use: "actions", Short: "Open the actions panel of the session you are in", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !coord.ActorFromEnv().IsUser() {
				return coord.ErrUserOnly
			}
			root, err := a.wireWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			ws := tmux.New(tmux.Exec{}, root)
			c, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			taskID := paneTask(cmd.Context(), c, pane)
			if taskID == 0 {
				if shown, err := ws.Shown(); err == nil {
					taskID = paneTask(cmd.Context(), c, shown)
				}
			}
			if taskID == 0 {
				taskID, _ = a.taskFor(cmd.Context(), c, coord.User)
			}
			c.Close()
			if taskID == 0 || board.OpenPanel == nil {
				_, err = ws.R.Run("display-message", "No open session")
				return err
			}
			return board.OpenPanel(taskID, "Overview")
		},
	}
	cmd.Flags().StringVar(&pane, "pane", "", "the pane the key was pressed in")
	return cmd
}

// paneTask returns the task of the live session running in pane, or 0.
func paneTask(ctx context.Context, c *coord.Coordinator, pane string) int64 {
	if pane == "" {
		return 0
	}
	ss, _ := c.ListSessions(ctx, 0)
	for _, s := range ss {
		if s.PaneID == pane && s.Status != "stopped" && s.Status != "failed" {
			return s.TaskID
		}
	}
	return 0
}

// attentionCmd jumps to what needs the user (bound to Shift+Down).
func (a *app) attentionCmd() *cobra.Command {
	return &cobra.Command{
		Use: "attention", Short: "Jump to what needs you", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !coord.ActorFromEnv().IsUser() {
				return coord.ErrUserOnly
			}
			root, err := a.wireWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			board.Passive = true
			pane, taskID, ok := board.Attention(func(ctx context.Context) (*coord.Coordinator, error) { return a.open(ctx) })
			ws := tmux.New(tmux.Exec{}, root)
			switch {
			case !ok:
				_, err = ws.R.Run("display-message", "Nothing needs you right now")
			case pane != "":
				err = ws.Show(pane)
			case board.OpenPanel != nil:
				err = board.OpenPanel(taskID, "Needs you")
			}
			return err
		},
	}
}

// wireWorkspace connects the board package to the tmux workspace when
// running inside it, and returns the project root.
func (a *app) wireWorkspace(ctx context.Context) (string, error) {
	c, err := a.open(ctx)
	if err != nil {
		return "", err
	}
	root := c.Root
	c.Close()
	board.Providers = adapter.Names()
	if !tmux.Available() || (os.Getenv("TMUX") == "" && os.Getenv("AGENT_SHIFTBOARD_IN_WORKSPACE") == "") {
		return root, nil
	}
	ws := tmux.New(tmux.Exec{}, root)
	board.Focus = ws.Show
	board.Shown = func() string { p, _ := ws.Shown(); return p }
	board.ClosePane = ws.Close
	board.Wake = ws.TypeLine
	board.OpenWorker = func(sid string) error { return a.resumeWorker(context.Background(), ws, sid) }
	board.Capture = func(pane string, lines int) string { return mustOut(ws.Capture(pane, lines)) }
	board.InputView = ws.InputView
	board.Leave = func() { _, _ = ws.R.Run("detach-client") }
	board.OpenLead = func(taskID int64, mode, provider string) error {
		c, err := a.open(context.Background())
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = a.openLead(context.Background(), c, ws, taskID, mode, provider, true)
		return err
	}
	board.NewTask = func() error {
		c, err := a.open(context.Background())
		if err != nil {
			return err
		}
		defer c.Close()
		id, err := c.CreateTask(context.Background(), coord.User, coord.DraftTitle, "(not described yet: ask the user what they want)")
		if err != nil {
			return err
		}
		_, err = a.openLead(context.Background(), c, ws, id, "new", "", true)
		return err
	}
	board.SetNeed = func(n int) { _, _ = ws.R.Run("set-option", "-g", "@asb_need", fmt.Sprint(n)) }
	board.OpenPanel = func(taskID int64, view string) error {
		// A floating modal over the workspace (tmux popup), like an editor's
		// settings window.
		_, err := ws.R.Run("display-popup", "-E", "-w", "84%", "-h", "80%", "-d", root,
			"-e", "AGENT_SHIFTBOARD_PROJECT="+root,
			"-T", fmt.Sprintf(" session #%d ", taskID),
			fmt.Sprintf("%s panel --task %d --view %s", tmuxQuote(ws.Self), taskID, tmuxQuote(view)))
		return err
	}
	return root, nil
}

func tmuxQuote(s string) string { return "'" + s + "'" }
