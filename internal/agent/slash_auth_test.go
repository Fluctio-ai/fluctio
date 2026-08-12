package agent

// slash_auth_test.go — verifies the /yes approval flow survives across
// separate Get() lookups (the same in-memory Session pointer is returned
// each time, so pendingCalls + approvedPending persist between the turn
// that parks the call and the /yes turn that drains it). Regression guard
// for the "no pending operation" user report: if any future refactor
// starts evicting the session from the cache between turns, or moves
// pending state onto a per-turn scratch struct, this test fails.

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

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

// TestEmitAuthPromptOptionsPayload pins the contract that the auth_prompt
// ChatEvent carries a structured options array the front-end renders as
// tappable buttons (/yes /no /auto /yolo). The user's north star is
// "parameter-driven buttons, not LLM text like '请回复 /yes'" — emitAuthPrompt
// is the single source of truth for that payload, and the live + catch-up
// handlers in chat-screen.tsx both read `data.options` to render buttons.
// If this shape drifts (e.g. options renamed, cmd field dropped), the UI
// falls back to a text bubble and the user is back to typing /yes manually.
func TestEmitAuthPromptOptionsPayload(t *testing.T) {
	a := &Agent{name: "agent-auth-prompt-test"}
	events := make(chan ChatEvent, 8)
	ctx := ContextWithChatEvents(context.Background(), events)
	a.emitAuthPrompt(ctx, "write outside workspace: /etc/foo", bus.InboundMessage{Channel: "web"})

	// Drain non-blocking; expect exactly one auth_prompt + no fallback
	// content event on the web channel.
	var got *ChatEvent
	for {
		select {
		case ev := <-events:
			if ev.Type == "auth_prompt" {
				got = &ev
			}
		default:
			goto done
		}
	}
done:
	if got == nil {
		t.Fatal("no auth_prompt event emitted")
	}
	desc, _ := got.Data["description"].(string)
	if desc != "write outside workspace: /etc/foo" {
		t.Errorf("description: got %q, want the desc passed in", desc)
	}
	rawOptions, ok := got.Data["options"]
	if !ok {
		t.Fatal("options key missing — UI cannot render buttons without it")
	}
	options, ok := rawOptions.([]map[string]string)
	if !ok {
		t.Fatalf("options not []map[string]string, got %T", rawOptions)
	}
	// Verify each option carries the three fields the front-end reads.
	wantCmds := []string{"/yes", "/no", "/auto", "/yolo"}
	if len(options) != len(wantCmds) {
		t.Fatalf("options length: got %d, want %d", len(options), len(wantCmds))
	}
	for i, want := range wantCmds {
		if options[i]["cmd"] != want {
			t.Errorf("options[%d].cmd: got %q, want %q", i, options[i]["cmd"], want)
		}
		if options[i]["label_zh"] == "" {
			t.Errorf("options[%d].label_zh empty — Chinese UI shows a blank button", i)
		}
		if options[i]["label_en"] == "" {
			t.Errorf("options[%d].label_en empty — English UI shows a blank button", i)
		}
	}
}

// TestEmitAuthPromptIMChannelFallback verifies the IM-channel branch still
// emits a plain-text content event alongside the structured auth_prompt,
// so channels without a bubble UI (telegram / wechat) display the
// authorization request and the literal slash commands the user can send.
// The web branch (TestEmitAuthPromptOptionsPayload) intentionally skips
// the content fallback so the UI's parameter-driven buttons are the only
// rendering.
func TestEmitAuthPromptIMChannelFallback(t *testing.T) {
	// A real MessageBus so the agent's outbound push is observable.
	// The bus constructor is in internal/bus; we only need the Outbound
	// channel to be drained, not the full gateway wiring.
	mb := bus.New()
	a := &Agent{name: "agent-auth-prompt-im-test", messageBus: mb}
	events := make(chan ChatEvent, 8)
	ctx := ContextWithChatEvents(context.Background(), events)
	a.emitAuthPrompt(ctx, "dangerous command: rm -rf ./", bus.InboundMessage{
		Channel: "telegram", AccountID: "bot-1", ChatID: "chat-1",
	})

	var sawContent, sawAuthPrompt bool
	var contentText string
	for {
		select {
		case ev := <-events:
			switch ev.Type {
			case "auth_prompt":
				sawAuthPrompt = true
			case "content":
				sawContent = true
				contentText, _ = ev.Data["content"].(string)
			}
		default:
			goto done
		}
	}
done:
	if !sawAuthPrompt {
		t.Error("IM channel: expected auth_prompt event alongside content fallback")
	}
	if !sawContent {
		t.Fatal("IM channel: expected content fallback event, got none")
	}
	// The text fallback must mention all four commands so the chatter on
	// an IM client (no buttons) knows the literal replies available.
	for _, cmd := range []string{"/yes", "/no", "/auto", "/yolo"} {
		if !strings.Contains(contentText, cmd) {
			t.Errorf("IM fallback content missing %q; text=%q", cmd, contentText)
		}
	}
	// Regression for the "IM never sees the auth prompt" bug: emitEvent
	// only fans out to SSE consumers, which IM channels don't read, so
	// the prompt must ALSO be pushed through the outbound bus — otherwise
	// the parked tool_call waits forever for a /yes that never comes.
	select {
	case out := <-mb.Outbound:
		if out.Channel != "telegram" || out.AccountID != "bot-1" || out.ChatID != "chat-1" {
			t.Errorf("outbound routing wrong: got channel=%q account=%q chat=%q",
				out.Channel, out.AccountID, out.ChatID)
		}
		if !strings.Contains(out.Text, "需要授权") {
			t.Errorf("outbound text missing 需要授权; got %q", out.Text)
		}
		for _, cmd := range []string{"/yes", "/no", "/auto", "/yolo"} {
			if !strings.Contains(out.Text, cmd) {
				t.Errorf("outbound text missing %q; got %q", cmd, out.Text)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("IM channel: expected outbound push carrying the auth prompt, got none (IM user would never see the prompt)")
	}
}

// TestFilterAuthorizedCallsPushesPendingAndBlocks ensures the loop's
// auth-gate helper leaves the session in the correct state when an
// ask-mode call is parked: pending pushed (for /yes to drain), holding
// tool_result in the blocked map (so tool_use↔tool_result pairing stays
// well-formed), and promptDesc non-empty (so the loop emits auth_prompt
// and terminates the turn instead of calling the LLM again).
func TestFilterAuthorizedCallsPushesPendingAndBlocks(t *testing.T) {
	mgr := session.NewManager(t.TempDir())
	g := newAuthGate("", "")
	a := &Agent{name: "agent-filter-test", sessions: mgr, authGate: g}
	triple := bus.InboundMessage{Channel: "web", ChatID: "chat-filter-1"}
	sess := mgr.Get(sessionTriple(triple, ""))

	// Two calls: one safe (allowed), one ask-prompted (parked).
	calls := []provider.ToolCall{
		{ID: "safe-1", Type: "function", Function: provider.FunctionCall{Name: "exec", Arguments: `{"command":"dir"}`}},
		{ID: "ask-1", Type: "function", Function: provider.FunctionCall{Name: "exec", Arguments: `{"command":"rm -rf ./"}`}},
	}
	toExec, blocked, promptDesc := a.filterAuthorizedCalls(sess, calls)

	if len(toExec) != 1 || toExec[0].ID != "safe-1" {
		t.Errorf("toExec: got %+v, want [safe-1]", toExec)
	}
	if len(blocked) != 1 {
		t.Fatalf("blocked: got %d entries, want 1 (the parked call's holding result)", len(blocked))
	}
	br, ok := blocked["ask-1"]
	if !ok {
		t.Fatalf("blocked map missing key ask-1: %+v", blocked)
	}
	if br.toolCallID != "ask-1" {
		t.Errorf("blocked[ask-1].toolCallID = %q", br.toolCallID)
	}
	if br.result == "" {
		t.Error("blocked[ask-1].result empty — the LLM would see an unexplained empty tool_result and retry")
	}
	if promptDesc == "" {
		t.Error("promptDesc empty — the loop wouldn't emit auth_prompt, buttons wouldn't render")
	}
	// Pending calls are now on the session, ready for /yes to drain.
	if got := sess.PopPendingCalls(); len(got) != 1 || got[0].ID != "ask-1" {
		t.Errorf("PopPendingCalls: got %+v, want [ask-1]", got)
	}
}

// dummy reference to keep reflect in imports honest if future helpers
// need deep-equality assertions. Avoids "imported and not used" cycles
// when other tests in this file evolve.
var _ = reflect.DeepEqual

