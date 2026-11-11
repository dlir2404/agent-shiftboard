// Package paths locates project and user state directories.
package paths

import (
	"errors"
	"os"
	"path/filepath"
)

// StateDirName is the ignored per-project state directory.
const StateDirName = ".agent-shiftboard"

// ErrNoProject means no initialized project was found.
var ErrNoProject = errors.New("no AgentShiftboard project found; run `agent-shiftboard init` in the project root")

// FindProject walks up from start to the directory that contains StateDirName.
// AGENT_SHIFTBOARD_PROJECT overrides the search.
func FindProject(start string) (string, error) {
	if p := os.Getenv("AGENT_SHIFTBOARD_PROJECT"); p != "" {
		if isDir(filepath.Join(p, StateDirName)) {
			return filepath.Abs(p)
		}
		return "", ErrNoProject
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if isDir(filepath.Join(dir, StateDirName)) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNoProject
		}
		dir = parent
	}
}

// StateDir returns the project's state directory.
func StateDir(root string) string { return filepath.Join(root, StateDirName) }

// ProjectDB returns the project database path.
func ProjectDB(root string) string { return filepath.Join(StateDir(root), "state.db") }

// UserDir returns the user-scope directory. AGENT_SHIFTBOARD_USER_DIR overrides it.
func UserDir() (string, error) {
	if d := os.Getenv("AGENT_SHIFTBOARD_USER_DIR"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "agent-shiftboard"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "agent-shiftboard"), nil
}

// UserDB returns the user database path.
func UserDB() (string, error) {
	d, err := UserDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "state.db"), nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
