package coord

import (
	"errors"
	"testing"
)

func TestCloseSessionGuards(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	aid := must(f.c.CreateAssignment(f.ctx, lead, AssignmentInput{TaskID: id, Scope: "x"}))
	wid := must(f.c.RegisterWorker(f.ctx, lead, aid, SessionSpec{Provider: "codex"}))
	worker := Actor{SessionID: wid}
	ok(t, f.c.SetSessionRuntime(f.ctx, wid, "%7", 0, ""))
	var killed []string
	kill := func(p string) error { killed = append(killed, p); return nil }

	fails(t, f.c.CloseSession(f.ctx, lead, lead.SessionID, kill), "your own session")
	fails(t, f.c.CloseSession(f.ctx, worker, lead.SessionID, kill), "only the current lead")
	ok(t, f.c.Observe(f.ctx, worker, wid, "running", "", "hook"))
	fails(t, f.c.CloseSession(f.ctx, lead, wid, kill), "only an idle session")
	ok(t, f.c.Observe(f.ctx, worker, wid, "idle", "", "hook"))
	fails(t, f.c.CloseSession(f.ctx, lead, wid, kill), "cancel it first")
	ok(t, f.c.CancelAssignment(f.ctx, lead, aid, "not needed"))
	if len(killed) != 0 {
		t.Fatalf("killed before every guard passed: %v", killed)
	}

	// A failed kill records nothing.
	fails(t, f.c.CloseSession(f.ctx, lead, wid, func(string) error { return errors.New("no pane") }), "no pane")
	if s := sessionByID(t, f, id, wid); s.Status != "idle" {
		t.Fatalf("status after failed kill: %s", s.Status)
	}
	ok(t, f.c.CloseSession(f.ctx, lead, wid, kill))
	s := sessionByID(t, f, id, wid)
	if len(killed) != 1 || killed[0] != "%7" || s.Status != "stopped" || s.Reason != "completed" || s.StatusSource != "process" {
		t.Fatalf("killed=%v session=%+v", killed, s)
	}
	var n int
	ok(t, f.c.DB.QueryRow(`SELECT COUNT(*) FROM events WHERE kind='session_closed' AND detail=? AND actor=?`, wid, lead.String()).Scan(&n))
	if n != 1 {
		t.Fatalf("session_closed events: %d", n)
	}

	// The lead cannot be closed, even by the user; a former lead can.
	fails(t, f.c.CloseSession(f.ctx, User, lead.SessionID, kill), "current or pending lead")
	next := Actor{SessionID: must(f.c.StartLead(f.ctx, User, id, SessionSpec{Provider: "codex"}))}
	fails(t, f.c.CloseSession(f.ctx, User, next.SessionID, kill), "current or pending lead")
	ok(t, f.c.AckHandoff(f.ctx, next, nil))
	ok(t, f.c.SetSessionRuntime(f.ctx, lead.SessionID, "%2", 0, ""))
	ok(t, f.c.Observe(f.ctx, lead, lead.SessionID, "idle", "", "hook"))
	ok(t, f.c.CloseSession(f.ctx, next, lead.SessionID, kill))
	if s := sessionByID(t, f, id, lead.SessionID); s.Status != "stopped" {
		t.Fatalf("former lead status %s", s.Status)
	}

	// Sessions of another task are out of reach.
	other := must(f.c.CreateTask(f.ctx, User, "other", ""))
	osid := must(f.c.StartLead(f.ctx, User, other, SessionSpec{Provider: "claude"}))
	fails(t, f.c.CloseSession(f.ctx, next, osid, kill), "belongs to task")
}

func TestResolveSessionAndViews(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	aid := must(f.c.CreateAssignment(f.ctx, lead, AssignmentInput{TaskID: id, Scope: "x"}))
	wid := must(f.c.RegisterWorker(f.ctx, lead, aid, SessionSpec{Provider: "codex"}))
	if got := must(f.c.ResolveSession(f.ctx, lead, aid)); got != wid {
		t.Fatalf("A-n resolved to %s", got)
	}
	if got := must(f.c.ResolveSession(f.ctx, lead, wid[:8])); got != wid {
		t.Fatalf("prefix resolved to %s", got)
	}
	_, err := f.c.ResolveSession(f.ctx, lead, "zzzz")
	fails(t, err, "no session matches")
	vs := must(f.c.ListSessionViews(f.ctx, id))
	roles := map[string]string{}
	for _, v := range vs {
		roles[v.ID] = v.Role + " " + v.AssignmentID
	}
	if roles[lead.SessionID] != "lead " || roles[wid] != "worker "+aid {
		t.Fatalf("views %v", roles)
	}
}

func sessionByID(t *testing.T, f *fixture, task int64, id string) Session {
	t.Helper()
	for _, s := range must(f.c.ListSessions(f.ctx, task)) {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("session %s not found", id)
	return Session{}
}

func TestCheckMCPCaller(t *testing.T) {
	f := newFixture(t)
	_, lead := f.startLead(t)
	if err := f.c.CheckMCPCaller(f.ctx, User); !errors.Is(err, ErrNoSession) {
		t.Fatalf("no session: %v", err)
	}
	if err := f.c.CheckMCPCaller(f.ctx, Actor{SessionID: "made-up"}); !errors.Is(err, ErrNoSession) {
		t.Fatalf("unknown session: %v", err)
	}
	ok(t, f.c.CheckMCPCaller(f.ctx, lead))
	ok(t, f.c.Observe(f.ctx, lead, lead.SessionID, "stopped", "user_stopped", "process"))
	fails(t, f.c.CheckMCPCaller(f.ctx, lead), "has ended")
}
