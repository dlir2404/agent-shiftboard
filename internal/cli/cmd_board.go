package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/board"
	"github.com/dlir2404/agent-shiftboard/internal/coord"
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
			c.Close()
			board.Refresh = reconcilePanes
			return board.Run(func(ctx context.Context) (*coord.Coordinator, error) { return a.open(ctx) }, a.task)
		},
	}
	a.taskFlag(cmd)
	return cmd
}
