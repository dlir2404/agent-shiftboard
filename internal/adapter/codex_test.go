package adapter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCodexLaunchAndResume(t *testing.T) {
	s := LaunchSpec{SessionID: "sid", Root: "/p", Self: "/bin/agent-shiftboard", LaunchContext: "/p/launch.md", InitialPrompt: "begin"}
	cmd, err := CodexLaunch(s)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cmd, " ")
	for _, want := range []string{
		"codex -C /p",
		`mcp_servers.agent_shiftboard.command="/bin/agent-shiftboard"`,
		`mcp_servers.agent_shiftboard.default_tools_approval_mode="approve"`,
		`hooks.PreToolUse=[{matcher=".*",hooks=[{type="command",command="/bin/agent-shiftboard hook codex PreToolUse"`,
		`hooks.SessionStart=`, `hooks.Stop=`, `hooks.PermissionRequest=`,
		"/p/launch.md",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
	for _, bad := range []string{"dangerously", "--approve-for-me", "never", "model_instructions_file", "developer_instructions"} {
		if strings.Contains(got, bad) {
			t.Fatalf("must not contain %q", bad)
		}
	}
	if _, err := CodexLaunch(LaunchSpec{Root: "/p", Self: "x", PermissionMode: "never"}); err == nil {
		t.Fatal("unsupported permission mode must fail")
	}
	s.NativeID = "019a-thread"
	res, err := CodexResume(s)
	if err != nil || res[0] != "codex" || res[1] != "resume" || res[len(res)-1] != "019a-thread" {
		t.Fatalf("resume %v %v", res, err)
	}
	if _, err := CodexResume(LaunchSpec{Root: "/p", Self: "x"}); err == nil {
		t.Fatal("resume without native ID must fail")
	}
}

func TestCodexObservation(t *testing.T) {
	cases := map[string]string{"UserPromptSubmit": "running", "PreToolUse": "running", "PermissionRequest": "waiting_for_approval", "Stop": "idle", "Interrupt": "idle", "SessionStart": "idle"}
	for ev, want := range cases {
		if got := CodexObservation(HookInput{HookEventName: ev}, time.Now()).Status; got != want {
			t.Fatalf("%s: got %s want %s", ev, got, want)
		}
	}
}

func TestHookCommandShapes(t *testing.T) {
	for raw, want := range map[string]string{
		`{"command":"git push origin main"}`:                "git push origin main",
		`{"command":["bash","-lc","git push origin main"]}`: "git push origin main",
		`{"command":["git","push","origin","main"]}`:        "git push origin main",
		`{"cmd":"kubectl rollout restart deploy/api"}`:      "kubectl rollout restart deploy/api",
		`{"file_path":"a.go"}`:                              "",
	} {
		h := HookInput{ToolInput: json.RawMessage(raw)}
		if got := h.Command(); got != want {
			t.Fatalf("%s: got %q want %q", raw, got, want)
		}
	}
}

func TestTodoCall(t *testing.T) {
	full, isFull, _ := TodoCall(HookInput{ToolName: "TodoWrite", ToolInput: json.RawMessage(`{"todos":[{"content":"a","status":"completed"},{"content":"b","status":"in_progress"}]}`)})
	if !isFull || len(full) != 2 || full[0].Status != "done" || full[1].Status != "in_progress" {
		t.Fatalf("TodoWrite: %+v", full)
	}
	full, isFull, _ = TodoCall(HookInput{ToolName: "update_plan", ToolInput: json.RawMessage(`{"plan":[{"step":"x","status":"pending"}]}`)})
	if !isFull || full[0].Text != "x" || full[0].Status != "pending" {
		t.Fatalf("update_plan: %+v", full)
	}
	_, _, one := TodoCall(HookInput{ToolName: "TaskCreate", ToolInput: json.RawMessage(`{"subject":"run tests"}`), ToolResponse: json.RawMessage(`"Task #12 created successfully: run tests"`)})
	if one == nil || one.ExtID != "12" || one.Text != "run tests" {
		t.Fatalf("TaskCreate: %+v", one)
	}
	_, _, one = TodoCall(HookInput{ToolName: "TaskUpdate", ToolInput: json.RawMessage(`{"taskId":"12","status":"completed"}`)})
	if one == nil || one.ExtID != "12" || one.Status != "done" {
		t.Fatalf("TaskUpdate: %+v", one)
	}
}

// With the shared server (D-28) Codex gets the documented streamable HTTP
// keys (url, http_headers) instead of a stdio command, and keeps its tool
// allowlist and approval mode.
func TestCodexHTTPMCPOverrides(t *testing.T) {
	s := LaunchSpec{SessionID: "sid", Root: "/p", Self: "/bin/agent-shiftboard", MCPURL: "http://127.0.0.1:4242/mcp", MCPToken: "tok-secret"}
	got := strings.Join(CodexConfigOverrides(s), "\n")
	for _, want := range []string{
		`mcp_servers.agent_shiftboard.url="http://127.0.0.1:4242/mcp"`,
		`mcp_servers.agent_shiftboard.http_headers={"Authorization"="Bearer tok-secret"}`,
		`mcp_servers.agent_shiftboard.default_tools_approval_mode="approve"`,
		`mcp_servers.agent_shiftboard.enabled_tools=["shiftboard_worker_start"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	for _, bad := range []string{"agent_shiftboard.command", "agent_shiftboard.args", "agent_shiftboard.env", "bearer_token_env_var"} {
		if strings.Contains(got, bad) {
			t.Fatalf("must not contain %q:\n%s", bad, got)
		}
	}
	s.MCPURL, s.MCPToken = "", ""
	got = strings.Join(CodexConfigOverrides(s), "\n")
	if !strings.Contains(got, `agent_shiftboard.command="/bin/agent-shiftboard"`) || strings.Contains(got, "agent_shiftboard.url") || strings.Contains(got, "http_headers") {
		t.Fatalf("stdio overrides:\n%s", got)
	}
}
