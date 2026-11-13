package coord

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// ResolveActor returns who is calling: the session named in the
// environment or, when the variable is missing, a live managed session
// whose recorded process is an ancestor of this process. A command an
// agent runs with the variable removed, or one a user types through the
// agent's own shell escape, is still treated as that agent.
func (c *Coordinator) ResolveActor(ctx context.Context) Actor {
	if a := ActorFromEnv(); !a.IsUser() {
		return a
	}
	rows, err := c.DB.QueryContext(ctx, `SELECT id,pid FROM sessions WHERE pid>0 AND status NOT IN ('stopped','failed')`)
	if err != nil {
		return User
	}
	defer rows.Close()
	live := map[int]string{}
	for rows.Next() {
		var id string
		var pid int
		if rows.Scan(&id, &pid) == nil {
			live[pid] = id
		}
	}
	if len(live) == 0 {
		return User
	}
	for _, pid := range Ancestors(os.Getpid()) {
		if id, ok := live[pid]; ok {
			return Actor{SessionID: id}
		}
	}
	if AgentBackend() != "" {
		return Unidentified
	}
	return User
}

// Unidentified is a caller that runs inside an agent backend shared across
// conversations (Hermes's gateway) without a managed session's identity:
// an agent, but not one AgentShiftboard can name. Every operation refuses
// it; it never gets the user's authority.
var Unidentified = Actor{SessionID: "unidentified-agent"}

// ErrUnidentified explains why an unidentified agent is refused.
var ErrUnidentified = errors.New("this command runs inside an agent backend (Hermes's gateway) without an AgentShiftboard session identity, so it is refused; an agent AgentShiftboard launched uses its MCP tools, and the user runs commands from their own terminal")

// AgentBackend names an agent runtime among this process's ancestors that
// runs agents for several conversations, so a command it runs cannot be
// told apart from the user by its environment (D-29). It returns "" when
// there is none or the process table cannot be read.
var AgentBackend = func() string {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return ""
	}
	return agentBackendIn(string(out), os.Getpid())
}

func agentBackendIn(table string, pid int) string {
	type proc struct {
		ppid int
		cmd  string
	}
	procs := map[int]proc{}
	for _, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		p, _ := strconv.Atoi(f[0])
		pp, _ := strconv.Atoi(f[1])
		procs[p] = proc{pp, strings.Join(f[2:], " ")}
	}
	for cur, n := procs[pid].ppid, 0; cur > 1 && n < 64; cur, n = procs[cur].ppid, n+1 {
		if isHermes(procs[cur].cmd) {
			return "hermes"
		}
	}
	return ""
}

// isHermes recognizes Hermes's processes: its CLI entry point, a Python
// process running hermes_cli, or anything run from Hermes's install.
func isHermes(cmd string) bool {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return false
	}
	base := f[0][strings.LastIndex(f[0], "/")+1:]
	return base == "hermes" || strings.Contains(f[0], "/.hermes/") ||
		(strings.HasPrefix(base, "python") && strings.Contains(cmd, "hermes_cli"))
}

// Ancestors lists the parent chain of pid (excluding pid itself).
var Ancestors = func(pid int) []int {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	parent := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		p, _ := strconv.Atoi(f[0])
		pp, _ := strconv.Atoi(f[1])
		parent[p] = pp
	}
	var chain []int
	for cur, n := parent[pid], 0; cur > 1 && n < 64; cur, n = parent[cur], n+1 {
		chain = append(chain, cur)
	}
	return chain
}
