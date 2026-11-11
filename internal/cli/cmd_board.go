package cli

import (
	"context"

	"github.com/spf13/cobra"

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
			if tmux.Available() {
				ws := tmux.New(tmux.Exec{}, root)
				if tmux.ConfigPath != "" && ws.Exists() {
					_, _ = ws.R.Run("source-file", tmux.ConfigPath) // keys from an upgrade
				}
				board.Focus = func(pane string) error {
					if err := ws.SwapIntoMainIfNeeded(pane); err != nil {
						return err
					}
					_, err := ws.R.Run("select-pane", "-t", ws.MainSlot())
					return err
				}
				board.MainPane = func() string { p, _ := ws.MainPane(); return p }
			}
			return board.Run(func(ctx context.Context) (*coord.Coordinator, error) { return a.open(ctx) }, a.task)
		},
	}
	a.taskFlag(cmd)
	return cmd
}
