package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Hermes runs its agents inside a gateway shared by every Hermes client on
// the machine, so it is driven through its documented Agent Client
// Protocol (`hermes acp`) by AgentShiftboard's own pane client, which
// passes the shared MCP server and the session's token per session (D-29).
// There are no hooks: the client records status from the protocol.
type hermes struct{}

func (hermes) Name() string                          { return "hermes" }
func (hermes) PreassignsID() bool                    { return false }
func (hermes) Launch(s LaunchSpec) ([]string, error) { return HermesLaunch(s) }
func (hermes) Resume(s LaunchSpec) ([]string, error) { return HermesResume(s) }
func (hermes) Observe(HookInput, time.Time) Observation {
	return Observation{}
}
func (hermes) LaunchContextEvent(HookInput) bool { return false }

// WorkerOnly: Hermes leads after its worker path has passed live tests.
func (hermes) WorkerOnly() bool { return true }

func init() { register(hermes{}) }

// workerOnly marks a provider that cannot lead a task yet.
type workerOnly interface{ WorkerOnly() bool }

// CanLead reports whether a provider may lead a task.
func CanLead(name string) error {
	p, err := Get(name)
	if err != nil {
		return err
	}
	if w, ok := p.(workerOnly); ok && w.WorkerOnly() {
		return fmt.Errorf("%s runs as a worker only for now; start a lead with one of %v", name, LeadNames())
	}
	return nil
}

// LeadNames lists the providers that may lead a task.
func LeadNames() []string {
	var out []string
	for _, n := range Names() {
		if p := providers[n]; p != nil {
			if w, ok := p.(workerOnly); ok && w.WorkerOnly() {
				continue
			}
		}
		out = append(out, n)
	}
	return out
}

// ACPConfig is the per-session file the pane client reads: the shared MCP
// server's address and the session's token. It is a credential (0600).
type ACPConfig struct {
	MCPURL   string `json:"mcp_url"`
	MCPToken string `json:"mcp_token"`
}

// ReadACPConfig reads the file written by HermesLaunch or HermesResume.
func ReadACPConfig(path string) (ACPConfig, error) {
	var cfg ACPConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(b, &cfg)
	return cfg, err
}

// ACPAgentCommand is the agent process the pane client starts for a
// provider driven over ACP.
func ACPAgentCommand(provider string) ([]string, error) {
	switch provider {
	case "hermes":
		return []string{"hermes", "acp"}, nil
	}
	return nil, fmt.Errorf("%s is not driven over ACP", provider)
}

// ACPAgentEnv returns the environment for the agent process: the user's
// environment without AgentShiftboard's session variables, the Claude Code
// variables of a session that started it, or tmux's. `hermes acp` may start
// Hermes's shared gateway, which inherits this environment and then serves
// other Hermes clients; the session's identity travels only in its MCP
// connection.
func ACPAgentEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "AGENT_SHIFTBOARD_") || strings.HasPrefix(k, "CLAUDE") || strings.HasPrefix(k, "TMUX") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (s LaunchSpec) hermesCommon() ([]string, error) {
	if s.Self == "" || s.Root == "" || s.SessionDir == "" {
		return nil, fmt.Errorf("hermes needs the project root, session directory, and agent-shiftboard executable path")
	}
	if s.MCPURL == "" {
		// The gateway is shared; a stdio server it started would have no
		// reliable session identity (D-28).
		return nil, fmt.Errorf("hermes needs the project's shared MCP server; start it from the workspace")
	}
	cfg := filepath.Join(s.SessionDir, "acp.json")
	if err := writeJSON(cfg, ACPConfig{MCPURL: s.MCPURL, MCPToken: s.MCPToken}); err != nil {
		return nil, err
	}
	return []string{s.Self, "acp", "--provider", "hermes", "--config", cfg}, nil
}

// HermesLaunch starts the pane client for a new Hermes session. The first
// prompt names the launch context file, as for Codex.
func HermesLaunch(s LaunchSpec) ([]string, error) {
	cmd, err := s.hermesCommon()
	if err != nil {
		return nil, err
	}
	if s.InitialPrompt != "" {
		prompt := s.InitialPrompt
		if s.LaunchContext != "" {
			prompt = fmt.Sprintf("Before doing any work, read the AgentShiftboard launch context at %q and follow its instructions. Then continue this startup request: %s", s.LaunchContext, prompt)
		}
		cmd = append(cmd, "--prompt", prompt)
	}
	return cmd, nil
}

// HermesResume reattaches the pane client to the same native session.
func HermesResume(s LaunchSpec) ([]string, error) {
	if s.NativeID == "" {
		return nil, fmt.Errorf("no native Hermes session ID recorded; start a new session instead")
	}
	cmd, err := s.hermesCommon()
	if err != nil {
		return nil, err
	}
	return append(cmd, "--resume", s.NativeID), nil
}
