package cli

import (
	"context"
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
		Short: "Open the status board (user)",
		Long:  "The board shows task, lead, and worker status and lets you approve, deny, answer, confirm, and revoke. In the tmux workspace it runs beside the lead's CLI.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			root := c.Root
			c.Close()
			board.Refresh = reconcilePanes
			if tmux.Available() && os.Getenv("TMUX") != "" {
				ws := tmux.New(tmux.Exec{}, root)
				if tmux.ConfigPath != "" && ws.Exists() {
					_, _ = ws.R.Run("source-file", tmux.ConfigPath) // keys from an upgrade
				}
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
			}
			board.Providers = adapter.Names()
			return board.Run(func(ctx context.Context) (*coord.Coordinator, error) { return a.open(ctx) }, a.task)
		},
	}
	a.taskFlag(cmd)
	return cmd
}
