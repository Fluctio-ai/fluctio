package agent

import (
	"testing"
	"time"
)

// TestSystemPromptStableWithFixedNow guards the get_time-tool design:
// BuildSystemPromptAs takes now as a parameter so loop.go can pass the
// session's first-message timestamp, rendering the system prompt
// byte-identical across turns (prefix cache). Same now → same prompt;
// different now → dateLine must change.
func TestSystemPromptStableWithFixedNow(t *testing.T) {
	store := newFakeMemoryStore()
	store.put(testAgentID, ownerUID, "SOUL.md", "# Soul\nbe terse.")
	cb := newChatbotBuilder(store)
	chatterMem := cb.memory.WithUserID(chatterUID)

	fixedNow := time.Date(2026, 7, 27, 15, 0, 0, 0, time.UTC)

	p1 := cb.BuildSystemPromptAs(chatterUID, chatterMem, fixedNow)
	p2 := cb.BuildSystemPromptAs(chatterUID, chatterMem, fixedNow)
	if p1 != p2 {
		t.Fatalf("BuildSystemPromptAs not deterministic: same now yielded different system prompts — prefix cache would break")
	}

	// A different now must produce a different dateLine (time is really rendered).
	p3 := cb.BuildSystemPromptAs(chatterUID, chatterMem, fixedNow.Add(2*time.Hour))
	if p1 == p3 {
		t.Fatalf("different now should yield a different dateLine (time must surface in the prompt)")
	}
}
