package tmux

import (
	"errors"
	"strings"
	"testing"
)

type fake struct {
	calls  [][]string
	exists bool
	noList bool // the main window lost its list pane
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
	case "list-panes":
		if f.noList {
			return "/bin/agent-shiftboard start --task 1", nil
		}
		return "'/bin/agent-shiftboard' board\n/bin/agent-shiftboard start --task 1", nil
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
	for _, want := range []string{"new-session -d -s asb-proj-abc123 -n main", "split-window -h -d", "/bin/agent-shiftboard board", "-u AGENT_SHIFTBOARD_SESSION_ID"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "AGENT_SHIFTBOARD_SESSION_ID=") {
		t.Fatalf("session env must not be set globally:\n%s", joined)
	}
	f.calls, f.exists = nil, true
	if created, err := w.Ensure(); err != nil || created || len(f.calls) != 3 || f.calls[2][0] != "resize-pane" {
		t.Fatalf("existing session should be reused: %v %v", err, f.calls)
	}
	// A main window that lost its list pane gets it back on the left.
	f.calls, f.noList = nil, true
	if _, err := w.Ensure(); err != nil {
		t.Fatal(err)
	}
	joined = ""
	for _, c := range f.calls {
		joined += strings.Join(c, " ") + "\n"
	}
	if !strings.Contains(joined, "split-window -h -b -d -t asb-proj-abc123:main.0 -l 38") || !strings.Contains(joined, "AGENT_SHIFTBOARD_PROJECT=/p /bin/agent-shiftboard board") {
		t.Fatalf("list pane not restored:\n%s", joined)
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

func TestTypeLinePastesThenSubmits(t *testing.T) {
	submitDelay = 0
	f := &fake{}
	w := Workspace{R: f}
	if err := w.TypeLine("%3", "AgentShiftboard M-1 from claude lead: hi"); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range f.calls {
		got = append(got, strings.Join(c, " "))
	}
	want := []string{
		"set-buffer -b agent-shiftboard-line-3 AgentShiftboard M-1 from claude lead: hi",
		"paste-buffer -p -d -b agent-shiftboard-line-3 -t %3",
		"send-keys -t %3 Enter",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
	}
}

type windows struct {
	fake
	list string
}

func (f *windows) Run(args ...string) (string, error) {
	if args[0] == "list-windows" {
		f.calls = append(f.calls, args)
		return f.list, nil
	}
	return f.fake.Run(args...)
}

// RunHidden starts a missing window, respawns a dead one, and leaves a
// running one alone.
func TestRunHidden(t *testing.T) {
	for _, tc := range []struct{ list, pane, cmd string }{
		{"main\t%0\t0", "%7", "new-window"},
		{"main\t%0\t0\nmcp\t%3\t1", "%3", "respawn-pane"},
		{"main\t%0\t0\nmcp\t%3\t0", "%3", ""},
	} {
		f := &windows{list: tc.list}
		w := Workspace{R: f, Session: "asb-p", Root: "/p", Self: "/bin/agent-shiftboard"}
		pane, err := w.RunHidden("mcp", map[string]string{"AGENT_SHIFTBOARD_PROJECT": "/p"}, []string{"/bin/agent-shiftboard", "mcp", "--serve"})
		if err != nil || pane != tc.pane {
			t.Fatalf("%q: pane %q err %v", tc.list, pane, err)
		}
		var ran []string
		for _, c := range f.calls[1:] {
			ran = append(ran, c[0])
		}
		joined := strings.Join(ran, " ")
		if tc.cmd == "" && joined != "" || tc.cmd != "" && !strings.HasPrefix(joined, tc.cmd) {
			t.Fatalf("%q: ran %v", tc.list, ran)
		}
		for _, c := range f.calls {
			if strings.Contains(strings.Join(c, " "), "AGENT_SHIFTBOARD_SESSION_ID") {
				t.Fatalf("session variable passed: %v", c)
			}
		}
	}
}
