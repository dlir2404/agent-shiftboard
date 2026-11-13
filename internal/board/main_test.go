package board

import (
	"os"
	"testing"
)

// The board ignores the pane it runs in (TMUX_PANE). Tests use fixed pane
// IDs such as %9, so a test run inside a tmux pane with that ID would
// treat a test agent as the board itself.
func TestMain(m *testing.M) {
	os.Unsetenv("TMUX_PANE")
	os.Exit(m.Run())
}
