package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/paths"
	"github.com/dlir2404/agent-shiftboard/internal/render"
)

func (a *app) leadCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "lead", Short: "Start or switch a task's lead session (user)"}
	var provider string
	var registerOnly bool
	newCmd := &cobra.Command{
		Use:   "new <task>",
		Short: "Start a new lead session from the brief (user)",
		Long: `Registers a new lead session and writes its launch context. The session must
acknowledge by readback (agent-shiftboard handoff ack ...) before it holds the
lead lease. Launching the provider CLI in the tmux workspace arrives with the
Phase 2 adapter; until then use --register-only and start the CLI yourself
with AGENT_SHIFTBOARD_SESSION_ID set.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTaskArg(args[0])
			if err != nil {
				return err
			}
			if !registerOnly {
				return fmt.Errorf("launching %s requires the tmux adapter (Phase 2); use --register-only for now", provider)
			}
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				sid, err := c.StartLead(ctx, actor, id, coord.SessionSpec{Provider: provider})
				if err != nil {
					return err
				}
				p, err := writeLaunchContext(ctx, c, id, sid)
				if err != nil {
					return err
				}
				return a.emit(map[string]string{"session": sid, "launch_context": p}, func(w io.Writer) {
					fmt.Fprintf(w, "Registered lead session %s for task #%d.\nLaunch context: %s\nStart the CLI with:\n  %s=%s %s\n",
						sid, id, p, coord.SessionEnv, sid, provider)
				})
			})
		},
	}
	newCmd.Flags().StringVar(&provider, "provider", "claude", "provider CLI for the new lead")
	newCmd.Flags().BoolVar(&registerOnly, "register-only", false, "register the session and write its launch context without launching")
	cmd.AddCommand(newCmd)
	return cmd
}

// writeLaunchContext writes the text a new session receives at launch.
func writeLaunchContext(ctx context.Context, c *coord.Coordinator, taskID int64, sid string) (string, error) {
	v, err := render.Load(ctx, c, taskID)
	if err != nil {
		return "", err
	}
	ids, err := c.RequiredReadback(ctx, taskID)
	if err != nil {
		return "", err
	}
	p := filepath.Join(paths.StateDir(c.Root), "sessions", sid, "launch.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	return p, os.WriteFile(p, []byte(render.LaunchContext(v, sid, ids)), 0o600)
}
