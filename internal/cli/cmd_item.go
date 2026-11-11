package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
)

func (a *app) itemCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "item", Short: "Small work items inside a session (task)"}
	list := &cobra.Command{
		Use: "list", Short: "List work items", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := a.taskFor(ctx, c, actor)
				if err != nil {
					return err
				}
				items, err := c.ListItems(ctx, id)
				if err != nil {
					return err
				}
				return a.emit(items, func(w io.Writer) {
					var rows [][]string
					for _, it := range items {
						rows = append(rows, []string{it.ID, it.Status, it.Owner, trunc(it.Text, 70)})
					}
					table(w, "ID\tSTATUS\tOWNER\tTEXT", rows)
				})
			})
		},
	}
	a.taskFlag(list)
	var status, owner string
	add := &cobra.Command{
		Use: "add <text...>", Short: "Record a work item (your own todo list is mirrored automatically)", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				id, err := c.AddItem(ctx, actor, a.task, strings.Join(args, " "), status, owner)
				if err != nil {
					return err
				}
				return a.done("Added "+id+".", map[string]string{"id": id})
			})
		},
	}
	a.taskFlag(add)
	add.Flags().StringVar(&status, "status", "pending", "pending | in_progress | review | done | blocked | cancelled")
	add.Flags().StringVar(&owner, "owner", "", "who does it (defaults to you)")
	var text string
	set := &cobra.Command{
		Use: "set <I-id> <status>", Short: "Change a work item's status", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				if err := c.SetItem(ctx, actor, strings.ToUpper(args[0]), args[1], text); err != nil {
					return err
				}
				return a.done(fmt.Sprintf("%s is %s.", strings.ToUpper(args[0]), args[1]), nil)
			})
		},
	}
	set.Flags().StringVar(&text, "text", "", "new text")
	cmd.AddCommand(list, add, set)
	return cmd
}
