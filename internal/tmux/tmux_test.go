package tmux

import (
	"errors"
	"strings"
	"testing"
)

type fake struct {
	calls  [][]string
	exists bool
}

func (f *fake) Run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	switch args[0] {
	case "has-session":
		if f.exists {
			return "", nil
		}
		return "", errors.New("no session")
	case "new-window":
		return "%7", nil
	}
	return "", nil
}

func TestEnsureBuildsLayoutWithoutSessionEnv(t *testing.T) {
	f := &fake{}
	w := Workspace{R: f, Session: "asb-proj-abc123", Root: "/p", Self: "/bin/agent-shiftboard"}
	if _, err := w.Ensure(); err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, c := range f.calls {
		joined += strings.Join(c, " ") + "\n"
	}
	for _, want := range []string{"new-session -d -s asb-proj-abc123 -n home", "/bin/agent-shiftboard board", "-u AGENT_SHIFTBOARD_SESSION_ID"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "AGENT_SHIFTBOARD_SESSION_ID=") {
		t.Fatalf("session env must not be set globally:\n%s", joined)
	}
	f.calls, f.exists = nil, true
	if created, err := w.Ensure(); err != nil || created || len(f.calls) != 1 {
		t.Fatalf("existing session should be reused: %v %v", err, f.calls)
	}
}

func TestLaunchSetsWindowEnvAndQuotes(t *testing.T) {
	f := &fake{}
	w := Workspace{R: f, Session: "s", Root: "/p"}
	pane, err := w.Launch("lead-1", map[string]string{"AGENT_SHIFTBOARD_SESSION_ID": "abc"}, []string{"claude", "--append-system-prompt", "it's a brief"})
	if err != nil || pane != "%7" {
		t.Fatalf("%v %q", err, pane)
	}
	got := strings.Join(f.calls[0], " ")
	if !strings.Contains(got, "-e AGENT_SHIFTBOARD_SESSION_ID=abc") || !strings.Contains(got, `claude --append-system-prompt 'it'\''s a brief'`) {
		t.Fatalf("launch args: %s", got)
	}
}

func TestSessionNameIsStableAndSafe(t *testing.T) {
	a, b := SessionName("/Users/x/my project!"), SessionName("/Users/x/my project!")
	if a != b || strings.ContainsAny(a, " !.:") {
		t.Fatalf("%q %q", a, b)
	}
}
