package cli

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/mcpserver"
)

func (a *app) mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve read and agent operations over MCP (stdio)",
		Long:  "Started by an agent CLI as an MCP server. User operations are not exposed.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := mcpserver.New(Version, func(ctx context.Context) (*coord.Coordinator, error) { return a.open(ctx) })
			return s.Run(cmd.Context(), &mcp.StdioTransport{})
		},
	}
}
