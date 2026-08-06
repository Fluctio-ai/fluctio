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

	// dateLine no longer embeds a wall-clock value (the get_time tool
	// serves live time on demand), so a different now in the SAME timezone
	// must yield the IDENTICAL prompt — this is the prefix-cache guarantee.
	p3 := cb.BuildSystemPromptAs(chatterUID, chatterMem, fixedNow.Add(2*time.Hour))
	if p1 != p3 {
		t.Fatalf("different now (same tz) should yield identical system prompt — dateLine must not embed wall-clock time")
	}
}
