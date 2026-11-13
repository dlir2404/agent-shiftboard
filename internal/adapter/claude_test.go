package adapter

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestClaudeLaunchCommand(t *testing.T) {
	dir := t.TempDir()
	s := LaunchSpec{SessionID: "sid", NativeID: "3f1c2b9e-1d2c-4b5a-9e8f-0a1b2c3d4e5f", Root: "/p", Self: "/bin/agent-shiftboard",
		SessionDir: dir, LaunchContext: "ctx", InitialPrompt: "ack", PermissionMode: "manual"}
	cmd, err := ClaudeLaunch(s)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cmd, " ")
	for _, want := range []string{"claude --session-id 3f1c2b9e", "--settings " + dir, "--mcp-config " + dir, "--permission-mode manual", "--append-system-prompt @file:ctx", " ack"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
	for _, bad := range []string{"dangerously", "bypassPermissions", "--bare"} {
		if strings.Contains(got, bad) {
			t.Fatalf("must not contain %q: %s", bad, got)
		}
	}
	res, _ := ClaudeResume(s)
	if !strings.Contains(strings.Join(res, " "), "--resume 3f1c2b9e") {
		t.Fatalf("resume: %v", res)
	}
}

func TestClaudeSettingsShape(t *testing.T) {
	b, _ := json.Marshal(ClaudeSettings(LaunchSpec{Self: "/bin/agent-shiftboard", SessionID: "s1", Root: "/p"}))
	s := string(b)
	// The session's identity is in the file itself, so a process that loses
	// the launch environment (a forked Claude session) still reports.
	for _, want := range []string{`"command":"AGENT_SHIFTBOARD_SESSION_ID=s1 AGENT_SHIFTBOARD_PROJECT=/p /bin/agent-shiftboard hook claude PreToolUse"`,
		`"env":{"AGENT_SHIFTBOARD_PROJECT":"/p","AGENT_SHIFTBOARD_SESSION_ID":"s1"}`, `"matcher":"*"`, `"StopFailure"`, `"mcp__agent-shiftboard__*"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}
}

func TestResetTime(t *testing.T) {
	now := time.Date(2026, 11, 11, 14, 0, 0, 0, time.Local)
	r, ok := ResetTime("You've hit your session limit · resets 3:45pm", now)
	if !ok || r.Hour() != 15 || r.Minute() != 45 || r.Day() != 11 {
		t.Fatalf("%v %v", r, ok)
	}
	r, ok = ResetTime("resets 9am", now)
	if !ok || r.Day() != 12 || r.Hour() != 9 {
		t.Fatalf("%v %v", r, ok)
	}
	if _, ok := ResetTime("API Error: Rate limit reached", now); ok {
		t.Fatal("no reset time expected")
	}
}

func TestWriteScriptReadsContextFromFile(t *testing.T) {
	dir := t.TempDir()
	p, err := WriteScript(dir, "launch.sh", []string{"claude", "--append-system-prompt", FileArg + "/s/launch.md", "it's go"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	want := "exec claude --append-system-prompt \"$(cat /s/launch.md)\" 'it'\\''s go'"
	if !strings.Contains(string(b), want) {
		t.Fatalf("script:\n%s\nwant %s", b, want)
	}
}

// With the shared server (D-28) the MCP config is an http server carrying
// the session's token in a header; otherwise a stdio server of its own.
// The token never reaches the settings file or the environment.
func TestClaudeMCPConfigShapes(t *testing.T) {
	dir := t.TempDir()
	s := LaunchSpec{SessionID: "s1", NativeID: "n1", Root: "/p", Self: "/bin/agent-shiftboard", SessionDir: dir,
		MCPURL: "http://127.0.0.1:4242/mcp", MCPToken: "tok-secret"}
	if _, err := ClaudeLaunch(s); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dir + "/claude-mcp.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	srv := cfg.MCPServers["agent-shiftboard"]
	if srv["type"] != "http" || srv["url"] != "http://127.0.0.1:4242/mcp" || srv["command"] != nil || srv["env"] != nil {
		t.Fatalf("http config: %s", b)
	}
	if h, _ := srv["headers"].(map[string]any); h["Authorization"] != "Bearer tok-secret" {
		t.Fatalf("headers: %s", b)
	}
	if fi, _ := os.Stat(dir + "/claude-mcp.json"); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	settings, _ := os.ReadFile(dir + "/claude-settings.json")
	if strings.Contains(string(settings), "tok-secret") {
		t.Fatal("token leaked into the settings file")
	}
	for _, v := range s.Env() {
		if strings.Contains(v, "tok-secret") {
			t.Fatal("token leaked into the environment")
		}
	}

	s.MCPURL, s.MCPToken = "", ""
	b, _ = json.Marshal(ClaudeMCPConfig(s))
	if got := string(b); !strings.Contains(got, `"type":"stdio"`) || !strings.Contains(got, `"args":["mcp"]`) || strings.Contains(got, "headers") {
		t.Fatalf("stdio config: %s", got)
	}
}
