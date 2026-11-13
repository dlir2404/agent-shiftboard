package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/acp"
	"github.com/dlir2404/agent-shiftboard/internal/adapter"
	"github.com/dlir2404/agent-shiftboard/internal/board"
	"github.com/dlir2404/agent-shiftboard/internal/coord"
)

func (a *app) acpCmd() *cobra.Command {
	var provider, config, prompt, resume string
	cmd := &cobra.Command{
		Use:    "acp",
		Short:  "Pane client for an agent session driven over the Agent Client Protocol (started by the workspace)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runACP(cmd.Context(), provider, config, prompt, resume)
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "provider (hermes)")
	cmd.Flags().StringVar(&config, "config", "", "the session's ACP config file")
	cmd.Flags().StringVar(&prompt, "prompt", "", "first prompt of a new session")
	cmd.Flags().StringVar(&resume, "resume", "", "native session ID to resume")
	return cmd
}

// runACP hosts one agent session in the current pane (D-29): it starts the
// agent's ACP process without the session's variables, shows the session
// to the user, and records status, plan, and native ID for the session in
// AGENT_SHIFTBOARD_SESSION_ID.
func (a *app) runACP(ctx context.Context, provider, config, prompt, resume string) error {
	actor := coord.ActorFromEnv()
	if actor.IsUser() {
		return errors.New("the ACP pane client runs only inside a session AgentShiftboard launched")
	}
	argv, err := adapter.ACPAgentCommand(provider)
	if err != nil {
		return err
	}
	cfg, err := adapter.ReadACPConfig(config)
	if err != nil {
		return fmt.Errorf("read the session's ACP config: %w", err)
	}
	c, err := a.open(ctx)
	if err != nil {
		return err
	}
	root := c.Root
	c.Close()

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGHUP, os.Interrupt)
	defer stop()
	agent := exec.Command(argv[0], argv[1:]...)
	agent.Dir = root
	agent.Env = adapter.ACPAgentEnv(os.Environ())
	stdin, err := agent.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := agent.StdoutPipe()
	if err != nil {
		return err
	}
	tail := &tailBuffer{max: 4096}
	agent.Stderr = tail // agent logs: shown only if it fails, never stored
	rec := &acpRecorder{a: a, actor: actor}
	if err := agent.Start(); err != nil {
		rec.Status("failed", "process_crash")
		return fmt.Errorf("start %s: %w", strings.Join(argv, " "), err)
	}
	defer func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _ = agent.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = agent.Process.Kill()
			<-done
		}
	}()

	fd := os.Stdin.Fd()
	if term.IsTerminal(fd) {
		if old, err := term.MakeRaw(fd); err == nil {
			defer func() { _ = term.Restore(fd, old) }()
		}
	}
	client := &acp.Client{Rec: rec, In: os.Stdin, Out: os.Stdout, Cwd: root, Name: provider}
	if cfg.MCPURL != "" {
		client.Servers = []acp.MCPServer{{Type: "http", Name: "agent_shiftboard", URL: cfg.MCPURL,
			Headers: []acp.NameValue{{Name: "Authorization", Value: "Bearer " + cfg.MCPToken}}}}
	}
	client.Connect(stdout, stdin)
	reason, runErr := client.Run(ctx, resume, prompt)
	switch reason {
	case "user_stopped":
		rec.Status("stopped", "user_stopped")
	default:
		rec.Status("failed", reason)
		if t := strings.TrimSpace(tail.String()); t != "" {
			fmt.Fprintf(os.Stdout, "\r\nLast output of %s:\r\n%s\r\n", strings.Join(argv, " "), strings.ReplaceAll(t, "\n", "\r\n"))
		}
	}
	return runErr
}

// acpRecorder reports the pane client's observations for its session.
// Each call opens the coordinator briefly, like a hook handler.
type acpRecorder struct {
	a     *app
	actor coord.Actor
}

func (r *acpRecorder) do(f func(ctx context.Context, c *coord.Coordinator) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := r.a.open(ctx)
	if err != nil {
		return
	}
	defer c.Close()
	_ = f(ctx, c)
}

func (r *acpRecorder) Status(status, reason string) {
	r.do(func(ctx context.Context, c *coord.Coordinator) error {
		return c.Observe(ctx, r.actor, r.actor.SessionID, status, reason, "stream")
	})
}

func (r *acpRecorder) Turn() {
	r.do(func(ctx context.Context, c *coord.Coordinator) error { return c.ObserveTurn(ctx, r.actor.SessionID) })
}

func (r *acpRecorder) Native(id string) {
	r.do(func(ctx context.Context, c *coord.Coordinator) error {
		return c.SetSessionRuntime(ctx, r.actor.SessionID, "", 0, id)
	})
}

func (r *acpRecorder) Plan(entries []acp.PlanEntry) {
	todos := make([]coord.TodoEntry, 0, len(entries))
	for _, e := range entries {
		status := "pending"
		switch e.Status {
		case "completed":
			status = "done"
		case "in_progress":
			status = "in_progress"
		}
		todos = append(todos, coord.TodoEntry{Text: e.Content, Status: status})
	}
	r.do(func(ctx context.Context, c *coord.Coordinator) error { return c.SyncTodos(ctx, r.actor, todos) })
}

func (r *acpRecorder) Deliver(send func(string) error) bool {
	var delivered bool
	r.do(func(ctx context.Context, c *coord.Coordinator) error {
		taskID, err := c.ResolveTask(ctx, r.actor, 0)
		if err != nil {
			return err
		}
		ms, err := c.QueuedMessages(ctx, taskID)
		if err != nil {
			return err
		}
		for _, m := range ms {
			if m.To != r.actor.SessionID {
				continue
			}
			delivered, err = c.DeliverMessage(ctx, m.ID, func(x coord.Message) error { return send(board.MessageLineMCP(x)) })
			return err
		}
		return nil
	})
	return delivered
}

func (r *acpRecorder) MCPUsed() bool {
	used := true // unknown counts as used: no false warning
	r.do(func(ctx context.Context, c *coord.Coordinator) error {
		u, err := c.MCPUsed(ctx, r.actor.SessionID)
		if err == nil {
			used = u
		}
		return err
	})
	return used
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

var _ io.Writer = (*tailBuffer)(nil)
