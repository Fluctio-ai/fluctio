package agent

// slash_auth_test.go — verifies the /yes approval flow survives across
// separate Get() lookups (the same in-memory Session pointer is returned
// each time, so pendingCalls + approvedPending persist between the turn
// that parks the call and the /yes turn that drains it). Regression guard
// for the "no pending operation" user report: if any future refactor
// starts evicting the session from the cache between turns, or moves
// pending state onto a per-turn scratch struct, this test fails.

import (
	"testing"

	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/session"
)

// TestAuthReplyDrainsPendingAcrossGetCalls is the RED→GREEN anchor:
// pending state stored on the cached Session pointer survives a second
// Get() for the same (channel, account, chat, project) tuple — the
// contract that slashAuthReply + drainApprovedPending rely on.
func TestAuthReplyDrainsPendingAcrossGetCalls(t *testing.T) {
	mgr := session.NewManager(t.TempDir())
	a := &Agent{
		name:     "agent-auth-test",
		sessions: mgr,
	}
	triple := bus.InboundMessage{
		Channel: "web",
		ChatID:  "chat-auth-1",
	}

	// Turn 1: gate parks a call via PushPendingCalls (same effect as
	// filterAuthorizedCalls running in ask mode).
	sess1 := mgr.Get(sessionTriple(triple, ""))
	if sess1 == nil {
		t.Fatal("first Get returned nil session")
	}
	parked := []provider.ToolCall{
		{ID: "c1", Type: "function", Function: provider.FunctionCall{Name: "exec", Arguments: `{"command":"dir"}`}},
	}
	sess1.PushPendingCalls(parked, "command execution")

	// Turn 2: user sends /yes. HandleMessage resolves the session again
	// (slashAuthReply does its own Get) — same pointer, same cache entry.
	sess2 := mgr.Get(sessionTriple(triple, ""))
	if sess2 != sess1 {
		t.Fatalf("second Get returned a different Session pointer — pending state would be lost: %p vs %p", sess1, sess2)
	}

	res := a.slashAuthReply(triple, true)
	if !res.handled {
		t.Fatalf("slashAuthReply(/yes): expected handled=true, got %+v", res)
	}
	if !res.continueToLoop {
		t.Fatalf("slashAuthReply(/yes): expected continueToLoop=true so the loop drains, got %+v", res)
	}

	// approvedPending is now set; drain it (the loop does this at the top
	// of the /yes turn). We expect 1 call back.
	got := sess2.DrainApprovedPending()
	if len(got) != 1 {
		t.Fatalf("DrainApprovedPending after /yes: got %d calls, want 1", len(got))
	}
	if got[0].ID != "c1" {
		t.Fatalf("drained call ID = %q, want c1", got[0].ID)
	}

	// Drain is idempotent: a second drain returns nothing.
	if got := sess2.DrainApprovedPending(); len(got) != 0 {
		t.Fatalf("second DrainApprovedPending: got %d calls, want 0", len(got))
	}
}

// TestAuthReplyNothingPending confirms the "nothing pending" reply path
// — the exact message the user reported seeing erroneously. With a fresh
// session (no parked calls), /yes returns the localized "no pending"
// message and does NOT enter the loop.
func TestAuthReplyNothingPending(t *testing.T) {
	mgr := session.NewManager(t.TempDir())
	a := &Agent{
		name:     "agent-auth-test-2",
		sessions: mgr,
	}
	triple := bus.InboundMessage{
		Channel: "web",
		ChatID:  "chat-auth-2",
	}

	// Fresh session, no PushPendingCalls yet.
	res := a.slashAuthReply(triple, true)
	if !res.handled {
		t.Fatal("expected handled=true even when nothing's pending")
	}
	if res.continueToLoop {
		t.Fatal("continueToLoop must be false when nothing's pending — the loop would burn an LLM call for no reason")
	}
	// Reply must mention "no pending" semantically. We don't pin the exact
	// string (it's localized) but it should mention "/yes" or "pending" so
	// the user can tell what they tried to do.
	if res.reply == "" {
		t.Fatal("reply is empty — user would see nothing and have no idea their /yes did nothing")
	}
}

// TestAuthReplyNoClearsPending confirms /no drops parked calls without
// draining (the user explicitly refused authorization).
func TestAuthReplyNoClearsPending(t *testing.T) {
	mgr := session.NewManager(t.TempDir())
	a := &Agent{
		name:     "agent-auth-test-3",
		sessions: mgr,
	}
	triple := bus.InboundMessage{
		Channel: "web",
		ChatID:  "chat-auth-3",
	}
	sess := mgr.Get(sessionTriple(triple, ""))
	sess.PushPendingCalls([]provider.ToolCall{
		{ID: "c1", Type: "function", Function: provider.FunctionCall{Name: "exec"}},
	}, "test")

	res := a.slashAuthReply(triple, false)
	if !res.handled {
		t.Fatal("/no: expected handled=true")
	}
	if res.continueToLoop {
		t.Fatal("/no must NOT continue to the loop — nothing to drain")
	}

	// Pending is cleared: a follow-up /yes finds nothing.
	if got := sess.DrainApprovedPending(); len(got) != 0 {
		t.Fatalf("/no should have cleared pending, but drain returned %d calls", len(got))
	}
	// And PopPendingCalls is also empty (slashAuthReply already popped).
	if got := sess.PopPendingCalls(); len(got) != 0 {
		t.Fatalf("/no should have popped pending, but Pop returned %d calls", len(got))
	}
}
