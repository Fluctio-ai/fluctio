package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/session"
)

// TestAppendSteerAddsNudgeAfterBatch verifies the steering wrap: the LLM
// message slice ends with a system nudge marking the batch as mid-task
// additions, while the persisted session history keeps only the raw user
// text (clean replay / next-turn context).
func TestAppendSteerAddsNudgeAfterBatch(t *testing.T) {
	mgr := session.NewManager(t.TempDir())
	a := &Agent{name: "agent-appendsteer-test", sessions: mgr}
	sess := mgr.Get(sessionTriple(bus.InboundMessage{Channel: "web", ChatID: "chat-steer-1"}, ""))

	messages := []provider.Message{{Role: "assistant", Content: "working"}}
	messages = a.appendSteer(context.Background(), sess, messages, []provider.Message{
		{Role: "user", Content: "also add the unit tests"},
		{Role: "user", Content: "and keep the naming"},
	})

	// Slice: raw steers then exactly one trailing nudge.
	if n := len(messages); n != 4 {
		t.Fatalf("expected 4 messages (1 assistant + 2 steers + 1 nudge), got %d", n)
	}
	if messages[1].Content != "also add the unit tests" || messages[2].Content != "and keep the naming" {
		t.Fatalf("steer text mutated in LLM slice: %q %q", messages[1].Content, messages[2].Content)
	}
	tail := messages[3]
	if tail.Role != "system" {
		t.Fatalf("expected trailing system nudge, got role=%q", tail.Role)
	}
	if !strings.Contains(tail.Content, "while you were working") {
		t.Fatalf("nudge missing mid-task marker: %q", tail.Content)
	}

	// History: raw steers persisted, no nudge leak.
	hist := sess.GetMessages()
	if n := len(hist); n != 2 {
		t.Fatalf("expected 2 persisted messages, got %d: %+v", n, hist)
	}
	for _, m := range hist {
		if m.Role != "user" || strings.Contains(m.Content, "while you were working") {
			t.Fatalf("history must keep raw user text only, got %+v", m)
		}
	}
}
