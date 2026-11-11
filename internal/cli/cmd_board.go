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
		Long:  "The session list shows sessions, their progress, and what needs you. In the tmux workspace it is the left pane; space opens a session's actions panel.",
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
			board.Refresh = reconcilePanes
			return board.Run(func(ctx context.Context) (*coord.Coordinator, error) { return a.open(ctx) }, a.task)
		},
	}
	a.taskFlag(cmd)
	return cmd
}

func (a *app) panelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "panel", Short: "Actions panel for one session (opened from the list)", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := a.wireWorkspace(cmd.Context()); err != nil {
				return err
			}
			return board.RunPanel(func(ctx context.Context) (*coord.Coordinator, error) { return a.open(ctx) }, a.task)
		},
	}
	a.taskFlag(cmd)
	return cmd
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
	if !tmux.Available() || os.Getenv("TMUX") == "" {
		return root, nil
	}
	ws := tmux.New(tmux.Exec{}, root)
	board.Focus = ws.Show
	board.Shown = func() string { p, _ := ws.Shown(); return p }
	board.ClosePane = ws.Close
	board.Wake = func(pane, text string) error {
		if _, err := ws.R.Run("send-keys", "-t", pane, "-l", text); err != nil {
			return err
		}
		_, err := ws.R.Run("send-keys", "-t", pane, "Enter")
		return err
	}
	board.OpenWorker = func(sid string) error { return a.resumeWorker(context.Background(), ws, sid) }
	board.Capture = func(pane string, lines int) string { return mustOut(ws.Capture(pane, lines)) }
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
	board.OpenPanel = func(taskID int64) error {
		// A floating modal over the workspace (tmux popup), like an editor's
		// settings window.
		_, err := ws.R.Run("display-popup", "-E", "-w", "84%", "-h", "80%", "-d", root,
			"-e", "AGENT_SHIFTBOARD_PROJECT="+root,
			"-T", fmt.Sprintf(" session #%d ", taskID),
			fmt.Sprintf("%s panel --task %d", tmuxQuote(ws.Self), taskID))
		return err
	}
	return root, nil
}

func tmuxQuote(s string) string { return "'" + s + "'" }
