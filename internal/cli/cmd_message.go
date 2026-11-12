package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
)

// messageCmd sends short notes between sessions of a task (D-25).
func (a *app) messageCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "message", Short: "Short messages between sessions of a task, delivered by the board"}
	send := &cobra.Command{
		Use:   "send <to> <text...>",
		Short: "Queue a message to another session: lead, A-n, a session ID or prefix, or a provider name",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := a.taskFor(ctx, c, actor)
				if err != nil {
					return err
				}
				m, err := c.SendMessage(ctx, actor, id, args[0], strings.Join(args[1:], " "))
				if err != nil {
					return err
				}
				return a.done(fmt.Sprintf("Queued %s for %s. The board types it in when that session is idle.", m.ID, m.ToLabel), m)
			})
		},
	}
	a.taskFlag(send)
	var queued bool
	list := &cobra.Command{
		Use: "list", Short: "List sessions you can message and recent messages", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := a.taskFor(ctx, c, actor)
				if err != nil {
					return err
				}
				ts, err := c.MessageTargets(ctx, actor, id)
				if err != nil {
					return err
				}
				ms, err := c.ListMessages(ctx, id, queued)
				if err != nil {
					return err
				}
				out := map[string]any{"targets": ts, "messages": ms}
				return a.emit(out, func(w io.Writer) {
					var rows [][]string
					for _, t := range ts {
						rows = append(rows, []string{t.Session, t.Label, t.Status, t.Assigned})
					}
					table(w, "SESSION\tWHO\tSTATUS\tASSIGNMENT", rows)
					fmt.Fprintln(w)
					rows = nil
					for _, m := range ms {
						rows = append(rows, []string{m.ID, m.Status, m.FromLabel, m.ToLabel, trunc(m.Body, 50)})
					}
					table(w, "ID\tSTATUS\tFROM\tTO\tTEXT", rows)
				})
			})
		},
	}
	a.taskFlag(list)
	list.Flags().BoolVar(&queued, "queued", false, "only messages not yet delivered")
	cancel := &cobra.Command{
		Use: "cancel <M-id>", Short: "Cancel a queued message (its sender or the user)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				if err := c.CancelMessage(ctx, actor, strings.ToUpper(args[0])); err != nil {
					return err
				}
				return a.done(strings.ToUpper(args[0])+" cancelled.", nil)
			})
		},
	}
	cmd.AddCommand(send, list, cancel)
	return cmd
}
