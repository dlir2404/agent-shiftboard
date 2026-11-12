package coord

import (
	"strings"
	"testing"
	"time"
)

func TestMessagesBetweenSessions(t *testing.T) {
	f := newFixture(t)
	id, claude := f.startLead(t)
	// Voluntary switch to Codex; the Claude session stays open as former lead.
	codex := Actor{SessionID: must(f.c.StartLead(f.ctx, User, id, SessionSpec{Provider: "codex"}))}
	ok(t, f.c.AckHandoff(f.ctx, codex, nil))
	ok(t, f.c.Observe(f.ctx, claude, claude.SessionID, "idle", "", "hook"))

	// The new lead reaches the former lead by provider name.
	m := must(f.c.SendMessage(f.ctx, codex, 0, "claude", "  please check\n the board   labels "))
	if m.To != claude.SessionID || m.Body != "please check the board labels" || m.Status != "queued" {
		t.Fatalf("sent %+v", m)
	}
	if !strings.HasPrefix(m.FromLabel, "codex lead ") || !strings.HasPrefix(m.ToLabel, "claude former lead ") {
		t.Fatalf("labels %q -> %q", m.FromLabel, m.ToLabel)
	}
	// The former lead replies to "lead".
	r := must(f.c.SendMessage(f.ctx, claude, 0, "lead", "done"))
	if r.To != codex.SessionID {
		t.Fatalf("reply went to %s", r.To)
	}

	_, err := f.c.SendMessage(f.ctx, codex, 0, "codex", "hi")
	fails(t, err, "no open session")
	_, err = f.c.SendMessage(f.ctx, codex, 0, codex.SessionID, "hi")
	fails(t, err, "your own session")
	_, err = f.c.SendMessage(f.ctx, codex, 0, "claude", strings.Repeat("x", MaxMessageLen+1))
	fails(t, err, "keep it to")
	_, err = f.c.SendMessage(f.ctx, codex, 0, "claude", "token ghp_abcdefghijklmnopqrstuvwxyz0123456789")
	fails(t, err, "secret")

	// Delivery clears the body; a failed send leaves it queued.
	var got Message
	delivered := must(f.c.DeliverMessage(f.ctx, m.ID, func(x Message) error { got = x; return nil }))
	if !delivered || got.Body != "please check the board labels" {
		t.Fatalf("delivered=%v %+v", delivered, got)
	}
	if again := must(f.c.DeliverMessage(f.ctx, m.ID, func(Message) error { t.Fatal("delivered twice"); return nil })); again {
		t.Fatal("delivered twice")
	}
	_, err = f.c.DeliverMessage(f.ctx, r.ID, func(Message) error { return errString("pane gone") })
	fails(t, err, "pane gone")
	ms := must(f.c.ListMessages(f.ctx, id, false))
	if len(ms) != 2 || ms[0].ID != r.ID || ms[0].Status != "queued" || ms[0].Body != "done" {
		t.Fatalf("after failed send: %+v", ms)
	}
	if ms[1].Status != "delivered" || ms[1].Body != "" {
		t.Fatalf("delivered body kept: %+v", ms[1])
	}

	fails(t, f.c.CancelMessage(f.ctx, codex, r.ID), "only the sender")
	ok(t, f.c.CancelMessage(f.ctx, claude, r.ID))
	if q := must(f.c.ListMessages(f.ctx, id, true)); len(q) != 0 {
		t.Fatalf("queued after cancel: %+v", q)
	}

	// An exited session cannot be messaged.
	ok(t, f.c.Observe(f.ctx, claude, claude.SessionID, "stopped", "user_stopped", "process"))
	_, err = f.c.SendMessage(f.ctx, codex, 0, claude.SessionID, "hi")
	fails(t, err, "has exited")
	if ts := must(f.c.MessageTargets(f.ctx, codex, 0)); len(ts) != 0 {
		t.Fatalf("targets %+v", ts)
	}
}

func TestMessageToWorkerByAssignment(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	aid := must(f.c.CreateAssignment(f.ctx, lead, AssignmentInput{TaskID: id, Scope: "add coverage"}))
	wsid := must(f.c.RegisterWorker(f.ctx, lead, aid, SessionSpec{Provider: "codex"}))
	m := must(f.c.SendMessage(f.ctx, lead, 0, strings.ToLower(aid), "also cover the error path"))
	if m.To != wsid || !strings.HasPrefix(m.ToLabel, "codex worker ") {
		t.Fatalf("sent %+v", m)
	}
	ts := must(f.c.MessageTargets(f.ctx, lead, 0))
	if len(ts) != 1 || ts[0].Assigned != aid {
		t.Fatalf("targets %+v", ts)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestMessageDeliveryOrderAndGuards(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	aid := must(f.c.CreateAssignment(f.ctx, lead, AssignmentInput{TaskID: id, Scope: "x"}))
	_ = must(f.c.RegisterWorker(f.ctx, lead, aid, SessionSpec{Provider: "codex"}))
	var ids []string
	for _, txt := range []string{"one", "two", "three"} {
		ids = append(ids, must(f.c.SendMessage(f.ctx, lead, 0, aid, txt)).ID)
	}
	q := must(f.c.QueuedMessages(f.ctx, id))
	if len(q) != 3 || q[0].ID != ids[0] || q[2].ID != ids[2] || q[0].Body != "one" {
		t.Fatalf("queue not oldest first: %+v", q)
	}

	// A failed send changes nothing and records no delivery.
	_, err := f.c.DeliverMessage(f.ctx, ids[0], func(Message) error { return errString("busy") })
	fails(t, err, "busy")
	var n int
	ok(t, f.c.DB.QueryRow(`SELECT COUNT(*) FROM events WHERE kind='message_delivered'`).Scan(&n))
	if n != 0 {
		t.Fatalf("delivery event after a failed send: %d", n)
	}
	// Cancelled while being typed: it stays cancelled, no delivery event.
	if d := must(f.c.DeliverMessage(f.ctx, ids[1], func(Message) error { return f.c.CancelMessage(f.ctx, lead, ids[1]) })); d {
		t.Fatal("a message cancelled mid-send was marked delivered")
	}

	// During a handoff the old holder can no longer cancel its messages.
	_ = must(f.c.StartLead(f.ctx, User, id, SessionSpec{Provider: "codex"}))
	fails(t, f.c.CancelMessage(f.ctx, lead, ids[2]), "can no longer write")
}

func TestMessageClaimIsExclusive(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	aid := must(f.c.CreateAssignment(f.ctx, lead, AssignmentInput{TaskID: id, Scope: "x"}))
	_ = must(f.c.RegisterWorker(f.ctx, lead, aid, SessionSpec{Provider: "codex"}))
	m := must(f.c.SendMessage(f.ctx, lead, 0, aid, "hello"))

	// A second relay that starts while the first is typing skips the message.
	typed := 0
	var inner bool
	outer := must(f.c.DeliverMessage(f.ctx, m.ID, func(Message) error {
		typed++
		inner = must(f.c.DeliverMessage(f.ctx, m.ID, func(Message) error { typed++; return nil }))
		return nil
	}))
	if !outer || inner || typed != 1 {
		t.Fatalf("outer=%v inner=%v typed=%d", outer, inner, typed)
	}

	// A claim left by a crashed relay is taken over after the timeout.
	m2 := must(f.c.SendMessage(f.ctx, lead, 0, aid, "again"))
	_, err := f.c.DB.Exec(`UPDATE messages SET claimed_at=? WHERE id=?`, f.c.now(), strings.TrimPrefix(m2.ID, "M-"))
	ok(t, err)
	if d := must(f.c.DeliverMessage(f.ctx, m2.ID, func(Message) error { return nil })); d {
		t.Fatal("delivered while another relay holds the claim")
	}
	f.advance(claimTimeout + time.Second)
	if d := must(f.c.DeliverMessage(f.ctx, m2.ID, func(Message) error { return nil })); !d {
		t.Fatal("stale claim was not taken over")
	}
}
