package coord

import "testing"

func TestAgentBackendIn(t *testing.T) {
	table := `    1     0 /sbin/launchd
  100     1 /Users/u/.hermes/tools/python3 -m hermes_cli.main --profile default gateway run --quiet --idle-exit
  200   100 /bin/zsh -c agent-shiftboard pending record-approval P-1
  300   200 agent-shiftboard pending record-approval P-1
  400     1 /bin/zsh -l
  500   400 agent-shiftboard pending list
  600     1 /usr/local/bin/hermes chat
  700   600 agent-shiftboard brief show
  800     1 /usr/bin/vim notes-about-hermes_cli.txt
  900   800 agent-shiftboard task list`
	for pid, want := range map[int]string{300: "hermes", 500: "", 700: "hermes", 900: ""} {
		if got := agentBackendIn(table, pid); got != want {
			t.Errorf("pid %d: got %q, want %q", pid, got, want)
		}
	}
}
