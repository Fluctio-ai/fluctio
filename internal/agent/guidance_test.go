package agent

import (
	"strings"
	"testing"
	"time"
)

// TestGuidanceSwitchesIdentityAnchor verifies the guidance field flips
// modIdentityAnchor between the firm "IDENTITY OVERRIDE" anchor (guided,
// the default for sub-flagship models) and the soft "# Identity" anchor
// (autonomous, top-tier models). Empty guidance must behave as guided.
func TestGuidanceSwitchesIdentityAnchor(t *testing.T) {
	store := newFakeMemoryStore()
	store.put(testAgentID, ownerUID, "SOUL.md", "# Soul\nbe terse.")
	cb := newChatbotBuilder(store)
	cb.SetDisplayName("TestBot")
	chatterMem := cb.memory.WithUserID(chatterUID)
	fixedNow := time.Date(2026, 7, 27, 15, 0, 0, 0, time.UTC)

	cb.SetGuidance("autonomous")
	p := cb.BuildSystemPromptAs(chatterUID, chatterMem, fixedNow)
	if !strings.Contains(p, "# Identity\n") || strings.Contains(p, "IDENTITY OVERRIDE") {
		t.Fatalf("autonomous guidance should emit the soft # Identity anchor (no OVERRIDE hammer):\n%s", p)
	}

	cb.SetGuidance("guided")
	p = cb.BuildSystemPromptAs(chatterUID, chatterMem, fixedNow)
	if !strings.Contains(p, "IDENTITY OVERRIDE") {
		t.Fatalf("guided guidance should emit the firm IDENTITY OVERRIDE anchor:\n%s", p)
	}

	cb.SetGuidance("")
	p = cb.BuildSystemPromptAs(chatterUID, chatterMem, fixedNow)
	if !strings.Contains(p, "IDENTITY OVERRIDE") {
		t.Fatalf("empty guidance should default to guided (IDENTITY OVERRIDE):\n%s", p)
	}
}
