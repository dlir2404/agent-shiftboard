package coord

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dlir2404/agent-shiftboard/internal/paths"
)

type fixture struct {
	c   *Coordinator
	now time.Time
	ctx context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AGENT_SHIFTBOARD_USER_DIR", t.TempDir())
	t.Setenv("AGENT_SHIFTBOARD_PROJECT", "")
	if err := os.MkdirAll(paths.StateDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	f := &fixture{c: c, now: time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC), ctx: context.Background()}
	c.Now = func() time.Time { return f.now }
	return f
}

func (f *fixture) advance(d time.Duration) { f.now = f.now.Add(d) }

// must panics on error; a panic fails the test with the error and stack.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func fails(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("expected error containing %q, got %q", substr, err)
	}
}

// startLead creates a task with an acknowledged lead and returns both.
func (f *fixture) startLead(t *testing.T) (int64, Actor) {
	t.Helper()
	id := must(f.c.CreateTask(f.ctx, User, "implement cache", "add a cache"))
	sid := must(f.c.StartLead(f.ctx, User, id, SessionSpec{Provider: "claude"}))
	lead := Actor{SessionID: sid}
	ok(t, f.c.AckHandoff(f.ctx, lead, nil))
	return id, lead
}

func TestAgentCannotRunUserOperations(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	if err := f.c.CancelTask(f.ctx, lead, id); !errors.Is(err, ErrUserOnly) {
		t.Fatalf("CancelTask: got %v", err)
	}
	pid := must(f.c.AddPending(f.ctx, lead, id, "approval", "git push", "origin main", 0))
	if err := f.c.ApprovePending(f.ctx, lead, pid, false); !errors.Is(err, ErrUserOnly) {
		t.Fatalf("ApprovePending: got %v", err)
	}
	rid := must(f.c.AddRule(f.ctx, lead, RuleInput{Scope: "project", Text: "ask before every push", Source: "user in chat", Inferred: true}))
	if err := f.c.ConfirmRule(f.ctx, lead, rid); !errors.Is(err, ErrUserOnly) {
		t.Fatalf("ConfirmRule: got %v", err)
	}
	if err := f.c.BreakLock(f.ctx, lead, "x"); !errors.Is(err, ErrUserOnly) {
		t.Fatalf("BreakLock: got %v", err)
	}
	if _, err := f.c.StartLead(f.ctx, lead, id, SessionSpec{Provider: "codex"}); !errors.Is(err, ErrUserOnly) {
		t.Fatalf("StartLead: got %v", err)
	}
}

func TestFirstLeadNeedsReadback(t *testing.T) {
	f := newFixture(t)
	id := must(f.c.CreateTask(f.ctx, User, "t", ""))
	rid := must(f.c.AddRule(f.ctx, User, RuleInput{Scope: "user", Text: "reply in Vietnamese", Source: "user"}))
	sid := must(f.c.StartLead(f.ctx, User, id, SessionSpec{Provider: "claude"}))
	lead := Actor{SessionID: sid}

	task := must(f.c.GetTask(f.ctx, id))
	if task.LeaseState != "handoff_pending" || task.Status != "queued" {
		t.Fatalf("got lease %s status %s", task.LeaseState, task.Status)
	}
	_, err := f.c.UpdateBrief(f.ctx, lead, id, 1, BriefPatch{Set: map[string]json.RawMessage{"next_action": json.RawMessage(`"x"`)}})
	fails(t, err, "acknowledge the handoff first")
	fails(t, f.c.AckHandoff(f.ctx, lead, nil), rid)
	ok(t, f.c.AckHandoff(f.ctx, lead, []string{strings.ToLower(rid)}))

	task = must(f.c.GetTask(f.ctx, id))
	if task.LeaseState != "held" || task.LeadSession != sid || task.Status != "active" {
		t.Fatalf("after ack: %+v", task)
	}
}

func TestBriefOptimisticConcurrency(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	patch := BriefPatch{
		Set:    map[string]json.RawMessage{"next_action": json.RawMessage(`"inspect invalidation"`)},
		Append: map[string][]string{"decisions": {"use LRU"}},
	}
	rev := must(f.c.UpdateBrief(f.ctx, lead, id, 1, patch))
	if rev != 2 {
		t.Fatalf("rev %d", rev)
	}
	_, err := f.c.UpdateBrief(f.ctx, lead, id, 1, patch)
	fails(t, err, "revision 2, not 1")
	b := must(f.c.GetBrief(f.ctx, id))
	if b.Fields.NextAction != "inspect invalidation" || len(b.Fields.Decisions) != 1 || b.Fields.Goal != "add a cache" {
		t.Fatalf("brief %+v", b.Fields)
	}
	_, err = f.c.UpdateBrief(f.ctx, lead, id, 2, BriefPatch{Append: map[string][]string{"relevant_refs": {"PGPASSWORD=hunter2hunter2"}}})
	fails(t, err, "secret")
	_, err = f.c.UpdateBrief(f.ctx, lead, id, 2, BriefPatch{Set: map[string]json.RawMessage{"rules_in_force": json.RawMessage(`"x"`)}})
	fails(t, err, "coordinator-owned")
}

func TestRuleStatuses(t *testing.T) {
	f := newFixture(t)
	_, lead := f.startLead(t)
	stated := must(f.c.AddRule(f.ctx, lead, RuleInput{Scope: "project", Text: "dev DB is read-only", Source: "user in chat"}))
	inferred := must(f.c.AddRule(f.ctx, lead, RuleInput{Scope: "project", Text: "prefer small MRs", Source: "lead", Inferred: true}))
	userScope := must(f.c.AddRule(f.ctx, lead, RuleInput{Scope: "user", Text: "reply in Vietnamese", Source: "user in chat"}))
	rules := must(f.c.ListRules(f.ctx, 1, false))
	st := map[string]string{}
	for _, r := range rules {
		st[r.ID] = r.Status
	}
	if st[stated] != "active" || st[inferred] != "proposed" || st[userScope] != "proposed" {
		t.Fatalf("statuses %v", st)
	}
	ok(t, f.c.ConfirmRule(f.ctx, User, inferred))
	newID := must(f.c.SupersedeRule(f.ctx, User, stated, "dev DB writes need explicit approval", "user 2026-11-11"))
	active := must(f.c.ListRules(f.ctx, 1, true))
	var ids []string
	for _, r := range active {
		ids = append(ids, r.ID)
	}
	if strings.Contains(strings.Join(ids, ","), stated+",") || !strings.Contains(strings.Join(ids, ","), newID) {
		t.Fatalf("active rules %v", ids)
	}
}

func TestConversationApproval(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	pid := must(f.c.AddPending(f.ctx, lead, id, "approval", "git push", "origin fix/a", 0))
	fails(t, f.c.RecordApproval(f.ctx, lead, pid, "git push", "origin fix/b", "ok push đi"), "differ")
	fails(t, f.c.RecordApproval(f.ctx, lead, pid, "git push", "origin fix/a", ""), "quote")
	ok(t, f.c.RecordApproval(f.ctx, lead, pid, "git push", "origin fix/a", "ok push fix/a đi"))
	items := must(f.c.ListPending(f.ctx, id, false))
	if items[0].Status != "approved" || items[0].ResolvedVia != "conversation" {
		t.Fatalf("%+v", items[0])
	}
	f.c.Policy.ConversationApprovals = "board_only"
	pid2 := must(f.c.AddPending(f.ctx, lead, id, "approval", "git push", "origin fix/b", 0))
	fails(t, f.c.RecordApproval(f.ctx, lead, pid2, "git push", "origin fix/b", "ok"), "board_only")
	ok(t, f.c.ApprovePending(f.ctx, User, pid2, false))
}

func TestSideEffectLifecycle(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	pid := must(f.c.AddPending(f.ctx, lead, id, "approval", "git push", "origin fix/a", 0))
	eid := must(f.c.PlanEffect(f.ctx, lead, id, "git push", "origin fix/a", pid, ""))
	fails(t, f.c.StartEffect(f.ctx, lead, eid), "needs approval")
	ok(t, f.c.ApprovePending(f.ctx, User, pid, false))
	ok(t, f.c.StartEffect(f.ctx, lead, eid))
	ok(t, f.c.FinishEffect(f.ctx, lead, eid, "confirmed", "pushed 4f2a9c1"))

	// in_flight at turn end becomes unknown; planned stays.
	e2 := must(f.c.PlanEffect(f.ctx, lead, id, "kubectl rollout restart", "deploy/api", "", ""))
	e3 := must(f.c.PlanEffect(f.ctx, lead, id, "glab mr create", "fix/a", "", ""))
	ok(t, f.c.StartEffect(f.ctx, lead, e2))
	ok(t, f.c.Observe(f.ctx, lead, lead.SessionID, "idle", "", "hook"))
	assertEffect(t, f, id, e2, "unknown")
	assertEffect(t, f, id, e3, "planned")
	fails(t, f.c.FinishEffect(f.ctx, lead, e2, "confirmed", ""), "reconcile")

	// Quota stop: planned becomes unknown and the lease is suspended.
	ok(t, f.c.Observe(f.ctx, lead, lead.SessionID, "cooling_down", "quota", "hook"))
	assertEffect(t, f, id, e3, "unknown")
	if task := must(f.c.GetTask(f.ctx, id)); task.LeaseState != "suspended" {
		t.Fatalf("lease %s", task.LeaseState)
	}
	// Suspended holder may still write and reconcile; recovery returns the lease.
	ok(t, f.c.ReconcileEffect(f.ctx, lead, e2, "confirmed", "rollout history shows revision 12"))
	ok(t, f.c.Observe(f.ctx, lead, lead.SessionID, "idle", "", "hook"))
	if task := must(f.c.GetTask(f.ctx, id)); task.LeaseState != "held" {
		t.Fatalf("lease %s", task.LeaseState)
	}
	// Unknown effects are part of the readback.
	ids := must(f.c.RequiredReadback(f.ctx, id))
	if !strings.Contains(strings.Join(ids, ","), e3) || strings.Contains(strings.Join(ids, ","), e2) {
		t.Fatalf("readback %v", ids)
	}
}

func assertEffect(t *testing.T, f *fixture, task int64, id, status string) {
	t.Helper()
	for _, e := range must(f.c.ListEffects(f.ctx, task, "")) {
		if e.ID == id {
			if e.Status != status {
				t.Fatalf("%s is %s, want %s", id, e.Status, status)
			}
			return
		}
	}
	t.Fatalf("%s not found", id)
}

func TestVoluntaryHandoffBlocksOldLead(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	newSid := must(f.c.StartLead(f.ctx, User, id, SessionSpec{Provider: "codex"}))
	_, err := f.c.UpdateBrief(f.ctx, lead, id, 1, BriefPatch{Set: map[string]json.RawMessage{"next_action": json.RawMessage(`"x"`)}})
	fails(t, err, "can no longer write")
	// The old holder becoming idle must not undo the pending handoff.
	ok(t, f.c.Observe(f.ctx, lead, lead.SessionID, "idle", "", "hook"))
	if task := must(f.c.GetTask(f.ctx, id)); task.LeaseState != "handoff_pending" {
		t.Fatalf("lease %s", task.LeaseState)
	}
	ok(t, f.c.AckHandoff(f.ctx, Actor{SessionID: newSid}, nil))
	if task := must(f.c.GetTask(f.ctx, id)); task.LeadSession != newSid {
		t.Fatalf("holder %s", task.LeadSession)
	}
}

func TestSweepUnknownAndGrace(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	ok(t, f.c.Observe(f.ctx, lead, lead.SessionID, "running", "", "hook"))
	eid := must(f.c.PlanEffect(f.ctx, lead, id, "git push", "origin x", "", ""))
	ok(t, f.c.StartEffect(f.ctx, lead, eid))

	f.advance(11 * time.Minute)
	ok(t, f.c.Sweep(f.ctx))
	ss := must(f.c.ListSessions(f.ctx, id))
	if ss[0].Status != "unknown" {
		t.Fatalf("status %s", ss[0].Status)
	}
	// A brief unknown leaves effects and the lease alone.
	assertEffect(t, f, id, eid, "in_flight")
	if task := must(f.c.GetTask(f.ctx, id)); task.LeaseState != "held" {
		t.Fatalf("lease %s", task.LeaseState)
	}
	f.advance(6 * time.Minute)
	ok(t, f.c.Sweep(f.ctx))
	assertEffect(t, f, id, eid, "unknown")
	if task := must(f.c.GetTask(f.ctx, id)); task.LeaseState != "suspended" {
		t.Fatalf("lease %s", task.LeaseState)
	}
}

func TestHandoffAckTimeout(t *testing.T) {
	f := newFixture(t)
	id := must(f.c.CreateTask(f.ctx, User, "t", ""))
	must(f.c.StartLead(f.ctx, User, id, SessionSpec{Provider: "claude"}))
	f.advance(6 * time.Minute)
	ok(t, f.c.Sweep(f.ctx))
	if task := must(f.c.GetTask(f.ctx, id)); task.LeaseState != "vacant" {
		t.Fatalf("lease %s", task.LeaseState)
	}
}

func TestToolsAndResources(t *testing.T) {
	f := newFixture(t)
	_, lead := f.startLead(t)
	home, _ := os.UserHomeDir()
	err := f.c.RegisterTool(f.ctx, lead, Tool{Name: "pagetool", Scope: "project", Description: "dump and restore pages",
		Command: filepath.Join(home, ".claude/jobs/abc/tmp/pagetool.py"), Effect: "external_write"})
	fails(t, err, "provider-private")
	ok(t, f.c.RegisterTool(f.ctx, lead, Tool{Name: "pagetool", Scope: "project", Description: "dump and restore pages",
		Command: ".agent-shiftboard/tools/pagetool.py", Effect: "external_write",
		Requires: ToolRequires{Secrets: []string{"dev-db"}, Executables: []string{"kubectl"}}, Locks: []string{"pod-tmp:api"}}))
	fails(t, f.c.SetConfig(f.ctx, lead, ConfigValue{Name: "api", Scope: "user", Value: "x"}), "project-scope")
	fails(t, f.c.SetConfig(f.ctx, lead, ConfigValue{Name: "api", Scope: "project", Value: "token=abcdefgh12345678"}), "secret")
	ok(t, f.c.AddSecretRef(f.ctx, lead, SecretRef{Name: "dev-db", Scope: "project", Location: "~/.config/x/dev-db.env",
		Keys: []string{"PGHOST", "PGPASSWORD"}, Handling: "read-only use; never print"}))

	manifest := []byte(`scope = "user"
[config.lang]
value = "vi"
[tools.scan]
description = "scan failing threads"
command = "scanner"
effect = "read_only"
`)
	_, err = f.c.ApplyManifest(f.ctx, lead, manifest)
	if !errors.Is(err, ErrUserOnly) {
		t.Fatalf("apply as agent: %v", err)
	}
	n := must(f.c.ApplyManifest(f.ctx, User, manifest))
	if n != 2 {
		t.Fatalf("applied %d", n)
	}
	r := must(f.c.GetResources(f.ctx))
	if len(r.Tools) != 2 || len(r.Secrets) != 1 || len(r.Config) != 1 {
		t.Fatalf("resources %+v", r)
	}
	out := string(must(f.c.ExportManifest(f.ctx, "project")))
	if !strings.Contains(out, "pagetool") || strings.Contains(out, "scanner") {
		t.Fatalf("export:\n%s", out)
	}
}

func TestLocks(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	aid := must(f.c.CreateAssignment(f.ctx, lead, AssignmentInput{TaskID: id, Scope: "review API"}))
	wsid := must(f.c.RegisterWorker(f.ctx, lead, aid, SessionSpec{Provider: "codex"}))
	worker := Actor{SessionID: wsid}

	ok(t, f.c.AcquireLock(f.ctx, lead, "browser-profile:demo"))
	fails(t, f.c.AcquireLock(f.ctx, worker, "browser-profile:demo"), "held by")
	ok(t, f.c.Observe(f.ctx, lead, lead.SessionID, "stopped", "user_stopped", "process"))
	fails(t, f.c.AcquireLock(f.ctx, worker, "browser-profile:demo"), "stale")
	ok(t, f.c.BreakLock(f.ctx, User, "browser-profile:demo"))
	ok(t, f.c.AcquireLock(f.ctx, worker, "browser-profile:demo"))
}

func TestAssignmentFlow(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	aid := must(f.c.CreateAssignment(f.ctx, lead, AssignmentInput{TaskID: id, Scope: "add coverage", Expected: "tests"}))
	wsid := must(f.c.RegisterWorker(f.ctx, lead, aid, SessionSpec{Provider: "codex"}))
	worker := Actor{SessionID: wsid}
	ok(t, f.c.Observe(f.ctx, worker, wsid, "running", "", "hook"))
	fails(t, f.c.SubmitAssignment(f.ctx, lead, aid, "done", ""), "only the worker")
	ok(t, f.c.SubmitAssignment(f.ctx, worker, aid, "added 12 tests", "commit abc1234"))
	fails(t, f.c.ReviewAssignment(f.ctx, worker, aid, true, ""), "only the current lead")
	ok(t, f.c.ReviewAssignment(f.ctx, lead, aid, true, ""))
	as := must(f.c.ListAssignments(f.ctx, id))
	if as[0].Status != "accepted" {
		t.Fatalf("status %s", as[0].Status)
	}
}

func TestDelegations(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	gid := must(f.c.ProposeDelegation(f.ctx, lead, id, "product decisions for CR fixes", "pushing to main", "user in chat", f.now.Add(8*time.Hour)))
	fails(t, f.c.LogDecision(f.ctx, lead, gid, "keep ETA as template"), "proposed")
	ok(t, f.c.ConfirmDelegation(f.ctx, User, gid))
	ok(t, f.c.LogDecision(f.ctx, lead, gid, "keep ETA as template"))
	f.advance(9 * time.Hour)
	fails(t, f.c.LogDecision(f.ctx, lead, gid, "late"), "expired")
	ds := must(f.c.ListDelegations(f.ctx, id, false))
	if ds[0].Status != "expired" || len(ds[0].Log) != 1 || !ds[0].Log[0].NeedsReview {
		t.Fatalf("%+v", ds[0])
	}
}

func TestCancelPendingHandoff(t *testing.T) {
	f := newFixture(t)
	id := must(f.c.CreateTask(f.ctx, User, "t", ""))
	sid := must(f.c.StartLead(f.ctx, User, id, SessionSpec{Provider: "claude"}))
	ok(t, f.c.CancelPendingHandoff(f.ctx, User, id))
	if task := must(f.c.GetTask(f.ctx, id)); task.LeaseState != "vacant" {
		t.Fatalf("lease %s", task.LeaseState)
	}
	fails(t, f.c.AckHandoff(f.ctx, Actor{SessionID: sid}, nil), "no pending handoff")
	must(f.c.StartLead(f.ctx, User, id, SessionSpec{Provider: "claude"}))
}

func TestResolveActorByProcessAncestry(t *testing.T) {
	f := newFixture(t)
	_, lead := f.startLead(t)
	_, err := f.c.DB.Exec(`UPDATE sessions SET pid=4242 WHERE id=?`, lead.SessionID)
	ok(t, err)
	t.Setenv(SessionEnv, "")
	orig := Ancestors
	defer func() { Ancestors = orig }()
	Ancestors = func(int) []int { return []int{100, 4242, 1} }
	if a := f.c.ResolveActor(f.ctx); a.SessionID != lead.SessionID {
		t.Fatalf("got %v, want the lead session", a)
	}
	Ancestors = func(int) []int { return []int{100, 1} }
	if a := f.c.ResolveActor(f.ctx); !a.IsUser() {
		t.Fatalf("got %v, want user", a)
	}
}

func TestAgentCreatesAndDescribesTasks(t *testing.T) {
	f := newFixture(t)
	id := must(f.c.CreateTask(f.ctx, User, DraftTitle, "(not described yet)"))
	sid := must(f.c.StartLead(f.ctx, User, id, SessionSpec{Provider: "claude"}))
	lead := Actor{SessionID: sid}
	ok(t, f.c.AckHandoff(f.ctx, lead, nil))
	ok(t, f.c.DescribeTask(f.ctx, lead, id, "add hello.txt", "add hello.txt and push it"))
	task := must(f.c.GetTask(f.ctx, id))
	b := must(f.c.GetBrief(f.ctx, id))
	if task.Title != "add hello.txt" || b.Fields.Goal != "add hello.txt and push it" {
		t.Fatalf("%+v %+v", task, b.Fields)
	}
	follow := must(f.c.CreateTask(f.ctx, lead, "write docs", ""))
	if tk := must(f.c.GetTask(f.ctx, follow)); tk.Status != "queued" || tk.LeaseState != "vacant" {
		t.Fatalf("%+v", tk)
	}
	// Record an approval by ID and quote only.
	pid := must(f.c.AddPending(f.ctx, lead, id, "approval", "git push origin main (commit abc)", "origin main @ abc", 0))
	ok(t, f.c.RecordApproval(f.ctx, lead, pid, "", "", "ok push"))
}

func TestReplaceDeadWorker(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	aid := must(f.c.CreateAssignment(f.ctx, lead, AssignmentInput{TaskID: id, Scope: "x"}))
	w1 := must(f.c.RegisterWorker(f.ctx, lead, aid, SessionSpec{Provider: "codex"}))
	_, err := f.c.RegisterWorker(f.ctx, lead, aid, SessionSpec{Provider: "codex"})
	fails(t, err, "live worker")
	ok(t, f.c.Observe(f.ctx, User, w1, "stopped", "host_lost", "process"))
	must(f.c.RegisterWorker(f.ctx, lead, aid, SessionSpec{Provider: "codex"}))
}

func TestItemsFromTodosAssignmentsAndAgents(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	// Mirror the lead's todo list; a later list updates, adds, and cancels.
	ok(t, f.c.SyncTodos(f.ctx, lead, []TodoEntry{{Text: "create goodbye.txt", Status: "in_progress"}, {Text: "commit files", Status: "pending"}}))
	ok(t, f.c.SyncTodos(f.ctx, lead, []TodoEntry{{Text: "create goodbye.txt", Status: "done"}, {Text: "push", Status: "pending"}}))
	// Explicit item and an assignment item.
	iid := must(f.c.AddItem(f.ctx, lead, id, "ask the user about commit", "", ""))
	ok(t, f.c.SetItem(f.ctx, lead, iid, "blocked", ""))
	aid := must(f.c.CreateAssignment(f.ctx, lead, AssignmentInput{TaskID: id, Scope: "write README"}))
	wid := must(f.c.RegisterWorker(f.ctx, lead, aid, SessionSpec{Provider: "codex"}))
	ok(t, f.c.Observe(f.ctx, Actor{SessionID: wid}, wid, "running", "", "hook"))
	ok(t, f.c.SubmitAssignment(f.ctx, Actor{SessionID: wid}, aid, "done", ""))
	// Tool-by-tool todo updates.
	ok(t, f.c.UpsertTodo(f.ctx, lead, TodoEntry{ExtID: "7", Text: "run tests", Status: "pending"}))
	ok(t, f.c.UpsertTodo(f.ctx, lead, TodoEntry{ExtID: "7", Status: "done"}))

	got := map[string]string{}
	owners := map[string]string{}
	for _, it := range must(f.c.ListItems(f.ctx, id)) {
		got[it.Text] = it.Status
		owners[it.Text] = it.Owner
	}
	want := map[string]string{"create goodbye.txt": "done", "commit files": "cancelled", "push": "pending",
		"ask the user about commit": "blocked", "write README": "review", "run tests": "done"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%q: got %q want %q (all: %v)", k, got[k], v, got)
		}
	}
	if owners["write README"] != "codex worker" || owners["push"] != "claude lead" {
		t.Fatalf("owners: %v", owners)
	}
}
