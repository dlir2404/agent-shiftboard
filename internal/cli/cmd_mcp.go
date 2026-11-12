package cli

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/mcpserver"
	"github.com/dlir2404/agent-shiftboard/internal/tmux"
)

func (a *app) mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve read and agent operations over MCP (stdio)",
		Long:  "Started by an agent CLI as an MCP server. User operations are not exposed.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := mcpserver.New(Version, func(ctx context.Context) (*coord.Coordinator, error) { return a.open(ctx) }, &mcpserver.Ops{
				WorkerStart: a.startWorker,
				SessionClose: func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, ref string) error {
					sid, err := c.ResolveSession(ctx, actor, ref)
					if err != nil {
						return err
					}
					ws := tmux.New(tmux.Exec{}, c.Root)
					return c.CloseSession(ctx, actor, sid, ws.Close)
				},
				SessionList: func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, task int64) ([]coord.SessionView, error) {
					id, err := c.ResolveTask(ctx, actor, task)
					if err != nil {
						return nil, err
					}
					return c.ListSessionViews(ctx, id)
				},
			})
			return s.Run(cmd.Context(), &mcp.StdioTransport{})
		},
	}
}
