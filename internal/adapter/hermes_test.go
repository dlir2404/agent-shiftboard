package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHermesLaunch(t *testing.T) {
	dir := t.TempDir()
	s := LaunchSpec{SessionID: "sid", Root: "/repo", Self: "/bin/asb", SessionDir: dir, LaunchContext: dir + "/launch.md",
		InitialPrompt: "Begin A-1", MCPURL: "http://127.0.0.1:5/mcp", MCPToken: "tok"}
	cmd, err := HermesLaunch(s)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd, " ")
	if cmd[0] != "/bin/asb" || cmd[1] != "acp" || !strings.Contains(joined, "--provider hermes") || !strings.Contains(joined, "launch.md") || !strings.HasSuffix(joined, "Begin A-1") {
		t.Fatalf("launch: %q", cmd)
	}
	if strings.Contains(joined, "tok") {
		t.Fatal("the token is on the command line")
	}
	cfg := filepath.Join(dir, "acp.json")
	st, err := os.Stat(cfg)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config %v %v", st, err)
	}
	if c, err := ReadACPConfig(cfg); err != nil || c.MCPURL != s.MCPURL || c.MCPToken != "tok" {
		t.Fatalf("config %+v %v", c, err)
	}
	s.NativeID = "local-abc"
	if cmd, err := HermesResume(s); err != nil || cmd[len(cmd)-2] != "--resume" || cmd[len(cmd)-1] != "local-abc" {
		t.Fatalf("resume %q %v", cmd, err)
	}
	s.MCPURL = ""
	if _, err := HermesLaunch(s); err == nil {
		t.Fatal("launched without the shared MCP server")
	}
}

func TestHermesLeadsNothingYet(t *testing.T) {
	if err := CanLead("hermes"); err == nil {
		t.Fatal("hermes may lead")
	}
	if err := CanLead("claude"); err != nil {
		t.Fatal(err)
	}
	for _, n := range LeadNames() {
		if n == "hermes" {
			t.Fatal("hermes listed as a lead")
		}
	}
	if _, err := ACPAgentCommand("hermes"); err != nil {
		t.Fatal(err)
	}
}

func TestACPAgentEnv(t *testing.T) {
	got := ACPAgentEnv([]string{"PATH=/bin", "AGENT_SHIFTBOARD_SESSION_ID=x", "CLAUDE_CODE_SESSION_ID=y", "CLAUDECODE=1", "TMUX=/tmp/s", "TMUX_PANE=%1", "HOME=/h"})
	if strings.Join(got, ",") != "PATH=/bin,HOME=/h" {
		t.Fatalf("env %v", got)
	}
}
