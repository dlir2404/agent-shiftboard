package coord

import (
	"context"
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
	return User
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
