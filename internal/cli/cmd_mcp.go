package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/adapter"
	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/mcpserver"
	"github.com/dlir2404/agent-shiftboard/internal/paths"
	"github.com/dlir2404/agent-shiftboard/internal/tmux"
)

func (a *app) mcpCmd() *cobra.Command {
	var serve bool
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve read and agent operations over MCP (stdio, or the project's shared HTTP server with --serve)",
		Long: `Without flags: started by an agent CLI as its own stdio MCP server; it acts
for the session named in AGENT_SHIFTBOARD_SESSION_ID.

With --serve: the project's shared MCP server (Streamable HTTP on 127.0.0.1
only). The workspace starts it in a hidden window and records its address
in the runtime directory; every request must carry a session's token.
User operations are never exposed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			open := func(ctx context.Context) (*coord.Coordinator, error) { return a.open(ctx) }
			if serve {
				return a.serveMCP(cmd.Context(), open)
			}
			return mcpserver.New(Version, open, a.mcpOps()).Run(cmd.Context(), &mcp.StdioTransport{})
		},
	}
	cmd.Flags().BoolVar(&serve, "serve", false, "run the project's shared MCP server over HTTP on 127.0.0.1")
	return cmd
}

// mcpOps are the lifecycle operations both MCP modes expose.
func (a *app) mcpOps() *mcpserver.Ops {
	return &mcpserver.Ops{
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
	}
}

// mcpAddrFile records the shared server's host:port for a project.
func mcpAddrFile(root string) string { return filepath.Join(paths.RuntimeDir(root), "mcp.addr") }

// mcpServerID identifies a project's server in its health answer, so a
// launcher never hands a token to some other process that took the port.
func mcpServerID(root string) string {
	sum := sha256.Sum256([]byte(root))
	return "agent-shiftboard " + hex.EncodeToString(sum[:8])
}

func mcpURL(addr string) string { return "http://" + addr + "/mcp" }

// readMCPAddr returns the recorded address if it is a loopback host:port.
func readMCPAddr(root string) string {
	b, err := os.ReadFile(mcpAddrFile(root))
	if err != nil {
		return ""
	}
	addr := strings.TrimSpace(string(b))
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		return ""
	}
	return addr
}

// mcpAnswers reports whether this project's MCP server answers at addr.
func mcpAnswers(ctx context.Context, addr, root string) bool {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var h struct {
		Server string `json:"server"`
	}
	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&h) == nil && h.Server == mcpServerID(root)
}

// serveMCP runs the project's shared MCP server until interrupted. It
// listens on 127.0.0.1 only, on the recorded port when that is free (so
// sessions configured with it reconnect after a restart), and records the
// address it got.
func (a *app) serveMCP(ctx context.Context, open mcpserver.Opener) error {
	c, err := open(ctx)
	if err != nil {
		return err
	}
	root := c.Root
	c.Close()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	prev := readMCPAddr(root)
	if prev != "" && mcpAnswers(ctx, prev, root) {
		return fmt.Errorf("this project's MCP server already runs at %s", prev)
	}
	var ln net.Listener
	if prev != "" {
		ln, err = net.Listen("tcp", prev)
	}
	if ln == nil {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return err
		}
	}
	addr := ln.Addr().String()
	if err := writeFileAtomic(mcpAddrFile(root), []byte(addr+"\n")); err != nil {
		ln.Close()
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.HTTPHandler(Version, open, a.mcpOps()))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"server": mcpServerID(root)})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	// Like the board, run the new binary after an install: shut down,
	// then exec it, which listens on the same recorded port again.
	// Clients see their MCP session end and open a new one.
	upgraded := make(chan string, 1)
	if exe, err := os.Executable(); err == nil {
		if st, err := os.Stat(exe); err == nil {
			go func(mod time.Time) {
				for {
					select {
					case <-ctx.Done():
						return
					case <-time.After(2 * time.Second):
					}
					if st, err := os.Stat(exe); err == nil && st.ModTime().After(mod) {
						upgraded <- exe
						sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
						_ = srv.Shutdown(sctx)
						cancel()
						return
					}
				}
			}(st.ModTime())
		}
	}
	fmt.Fprintf(a.errOut, "AgentShiftboard MCP server for %s at %s (127.0.0.1 only; every request needs a session token)\n", root, mcpURL(addr))
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	select {
	case exe := <-upgraded:
		return syscall.Exec(exe, os.Args, os.Environ())
	default:
		return nil
	}
}

func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mcp.addr-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// mcpStartTimeout bounds how long a launcher waits for the shared server.
var mcpStartTimeout = 10 * time.Second

// ensureMCPServer returns the URL of the project's shared MCP server,
// starting it in the workspace's hidden "mcp" window when it does not
// answer, and waiting until it does.
func (a *app) ensureMCPServer(ctx context.Context, root string) (string, error) {
	if addr := readMCPAddr(root); addr != "" && mcpAnswers(ctx, addr, root) {
		return mcpURL(addr), nil
	}
	if err := startMCPServer(root); err != nil {
		return "", err
	}
	deadline := time.Now().Add(mcpStartTimeout)
	for {
		if addr := readMCPAddr(root); addr != "" && mcpAnswers(ctx, addr, root) {
			return mcpURL(addr), nil
		}
		if time.Now().After(deadline) {
			return "", errors.New("the shared MCP server did not start; see the workspace window \"mcp\"")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// startMCPServer starts the shared server in the workspace's hidden "mcp"
// window without waiting for it. A window whose server still runs (for
// example while it starts) is left alone; a dead one is respawned.
func startMCPServer(root string) error {
	if !tmux.Available() {
		return tmux.ErrNoTmux
	}
	ws := tmux.New(tmux.Exec{}, root)
	if !ws.Exists() {
		return errors.New("the workspace is not running")
	}
	// A server process, not an agent session: no session variable.
	_, err := ws.RunHidden("mcp", map[string]string{"AGENT_SHIFTBOARD_PROJECT": root}, []string{ws.Self, "mcp", "--serve"})
	return err
}

// mcpRestartGap spaces out the board's restarts of a server that keeps
// dying, so a broken server is not respawned on every refresh.
var mcpRestartGap = 30 * time.Second

var lastMCPRestart time.Time

// keepMCPServer runs on the board's refresh: while any session holds a
// token for the shared MCP server and the server does not answer (it
// crashed or its window was closed), it starts it again on the recorded
// port, so configured sessions reconnect. It never waits for the server.
func keepMCPServer(ctx context.Context, c *coord.Coordinator) {
	if time.Since(lastMCPRestart) < mcpRestartGap {
		return
	}
	if in, err := c.SharedMCPInUse(ctx); err != nil || !in {
		return
	}
	if addr := readMCPAddr(c.Root); addr != "" && mcpAnswers(ctx, addr, c.Root) {
		return
	}
	lastMCPRestart = time.Now()
	_ = startMCPServer(c.Root)
}

// connectMCP points a session about to be launched or resumed at the
// project's shared MCP server with a fresh token (D-28). When the server
// cannot run (no tmux workspace, for example), the session keeps a stdio
// server of its own, which still acts only for it, and any earlier token is
// revoked. The token is never printed.
func (a *app) connectMCP(ctx context.Context, c *coord.Coordinator, spec *adapter.LaunchSpec) {
	url, err := a.ensureMCPServer(ctx, c.Root)
	if err == nil {
		var token string
		if token, err = c.IssueMCPToken(ctx, spec.SessionID); err == nil {
			spec.MCPURL, spec.MCPToken = url, token
			return
		}
	}
	_ = c.RevokeMCPToken(ctx, spec.SessionID)
	fmt.Fprintf(a.errOut, "note: the session uses its own stdio MCP server (shared server unavailable: %v)\n", err)
}
