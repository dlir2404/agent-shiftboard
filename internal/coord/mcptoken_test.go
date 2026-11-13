package coord

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// Tokens are random, stored only as a hash, replaced on reissue, and
// revoked when their session ends or is closed (D-28).
func TestMCPTokens(t *testing.T) {
	f := newFixture(t)
	id, lead := f.startLead(t)
	aid := must(f.c.CreateAssignment(f.ctx, lead, AssignmentInput{TaskID: id, Scope: "x"}))
	wid := must(f.c.RegisterWorker(f.ctx, lead, aid, SessionSpec{Provider: "codex"}))

	if in, err := f.c.SharedMCPInUse(f.ctx); err != nil || in {
		t.Fatalf("shared server in use before any token: %v %v", in, err)
	}
	if _, err := f.c.IssueMCPToken(f.ctx, "no-such-session"); err == nil {
		t.Fatal("token issued for an unknown session")
	}
	lt := must(f.c.IssueMCPToken(f.ctx, lead.SessionID))
	wt := must(f.c.IssueMCPToken(f.ctx, wid))
	if b, err := base64.RawURLEncoding.DecodeString(lt); err != nil || len(b) != 32 || lt == wt {
		t.Fatalf("token %q: %d bytes, err %v", lt, len(b), err)
	}
	// Only the hash is stored.
	var stored string
	ok(t, f.c.DB.QueryRow(`SELECT mcp_token_hash FROM sessions WHERE id=?`, lead.SessionID).Scan(&stored))
	if stored == "" || strings.Contains(stored, lt) || stored != hashMCPToken(lt) {
		t.Fatalf("stored %q", stored)
	}
	if a := must(f.c.SessionForToken(f.ctx, lt)); a.SessionID != lead.SessionID {
		t.Fatalf("lead token resolves to %v", a)
	}
	if !must(f.c.SharedMCPInUse(f.ctx)) {
		t.Fatal("shared server not in use with live tokens")
	}
	if a := must(f.c.SessionForToken(f.ctx, wt)); a.SessionID != wid {
		t.Fatalf("worker token resolves to %v", a)
	}
	for _, bad := range []string{"", "x", lt + "x", stored} {
		if _, err := f.c.SessionForToken(f.ctx, bad); !errors.Is(err, ErrBadMCPToken) {
			t.Fatalf("token %q: %v", bad, err)
		}
	}

	// Using the token is recorded once; a new token starts unused.
	if must(f.c.MCPUsed(f.ctx, lead.SessionID)) {
		t.Fatal("token used before any connection")
	}
	ok(t, f.c.MarkMCPUsed(f.ctx, lead.SessionID))
	if !must(f.c.MCPUsed(f.ctx, lead.SessionID)) {
		t.Fatal("token use not recorded")
	}

	// Reissue (resume) replaces the old token.
	lt2 := must(f.c.IssueMCPToken(f.ctx, lead.SessionID))
	if _, err := f.c.SessionForToken(f.ctx, lt); !errors.Is(err, ErrBadMCPToken) {
		t.Fatalf("old token still works: %v", err)
	}
	if a := must(f.c.SessionForToken(f.ctx, lt2)); a.SessionID != lead.SessionID {
		t.Fatalf("new token resolves to %v", a)
	}
	if must(f.c.MCPUsed(f.ctx, lead.SessionID)) {
		t.Fatal("a new token counts as used")
	}

	// A session that stops or fails loses its token.
	ok(t, f.c.Observe(f.ctx, Actor{SessionID: wid}, wid, "failed", "process_crash", "process"))
	if _, err := f.c.SessionForToken(f.ctx, wt); !errors.Is(err, ErrBadMCPToken) {
		t.Fatalf("failed session's token still works: %v", err)
	}
	// Resuming it issues a token that works at once, while the record
	// still says failed (the CLI connects before it reports).
	wt2 := must(f.c.IssueMCPToken(f.ctx, wid))
	if a := must(f.c.SessionForToken(f.ctx, wt2)); a.SessionID != wid {
		t.Fatalf("resumed worker token resolves to %v", a)
	}
	ok(t, f.c.RevokeMCPToken(f.ctx, wid))
	if _, err := f.c.SessionForToken(f.ctx, wt2); !errors.Is(err, ErrBadMCPToken) {
		t.Fatalf("revoked token still works: %v", err)
	}

	// CloseSession revokes the token of the session it closes.
	wt3 := must(f.c.IssueMCPToken(f.ctx, wid))
	ok(t, f.c.Observe(f.ctx, Actor{SessionID: wid}, wid, "idle", "", "hook"))
	ok(t, f.c.CancelAssignment(f.ctx, lead, aid, "done"))
	ok(t, f.c.SetSessionRuntime(f.ctx, wid, "%7", 0, ""))
	ok(t, f.c.CloseSession(f.ctx, lead, wid, func(string) error { return nil }, nil))
	if _, err := f.c.SessionForToken(f.ctx, wt3); !errors.Is(err, ErrBadMCPToken) {
		t.Fatalf("closed session's token still works: %v", err)
	}
	if a := must(f.c.SessionForToken(f.ctx, lt2)); a.SessionID != lead.SessionID {
		t.Fatalf("closing a worker touched the lead's token: %v", a)
	}

	// A new lead that is abandoned before acknowledging loses its token.
	next := must(f.c.StartLead(f.ctx, User, id, SessionSpec{Provider: "claude"}))
	nt := must(f.c.IssueMCPToken(f.ctx, next))
	ok(t, f.c.CancelPendingHandoff(f.ctx, User, id))
	if _, err := f.c.SessionForToken(f.ctx, nt); !errors.Is(err, ErrBadMCPToken) {
		t.Fatalf("abandoned lead's token still works: %v", err)
	}
	ok(t, f.c.RevokeMCPToken(f.ctx, lead.SessionID))
	if must(f.c.SharedMCPInUse(f.ctx)) {
		t.Fatal("shared server still in use after every token was revoked")
	}
}
