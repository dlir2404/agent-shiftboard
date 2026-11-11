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
	b, _ := json.Marshal(ClaudeSettings("/bin/agent-shiftboard"))
	s := string(b)
	for _, want := range []string{`"PreToolUse":[{"hooks":[{"command":"/bin/agent-shiftboard hook claude PreToolUse"`, `"matcher":"*"`, `"StopFailure"`, `"mcp__agent-shiftboard__*"`} {
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
