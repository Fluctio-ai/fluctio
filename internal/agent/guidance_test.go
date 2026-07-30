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

// TestVolatileSectionsAfterOperational locks the cache-friendly ordering:
// per-chatter USER.md and MEMORY.md must render AFTER the stable
// operational sections (confidentiality, sandbox, ...) so the long
// identity + operational prefix stays byte-identical and cacheable
// across compression rebuilds and across chatters on a shared agent.
// Regression guard for the modChatterProfile split (step 5).
func TestVolatileSectionsAfterOperational(t *testing.T) {
	store := newFakeMemoryStore()
	store.put(testAgentID, ownerUID, "SOUL.md", "# Soul\nterse.")
	store.put(testAgentID, chatterUID, "USER.md", "# Chatter\n- Name: test")
	store.put(testAgentID, chatterUID, "MEMORY.md", "# Memory\n- fact")
	cb := newChatbotBuilder(store)
	cb.SetDisplayName("Bot")
	chatterMem := cb.memory.WithUserID(chatterUID)
	fixedNow := time.Date(2026, 7, 27, 15, 0, 0, 0, time.UTC)
	p := cb.BuildSystemPromptAs(chatterUID, chatterMem, fixedNow)

	confIdx := strings.Index(p, "Confidentiality")
	profIdx := strings.Index(p, "<current_chatter_profile")
	memIdx := strings.Index(p, "<chatter_long_term_memory")
	if confIdx < 0 || profIdx < 0 || memIdx < 0 {
		t.Fatalf("missing expected sections: conf=%d prof=%d mem=%d\n%s", confIdx, profIdx, memIdx, p)
	}
	if !(confIdx < profIdx) || !(confIdx < memIdx) {
		t.Fatalf("volatile USER.md/MEMORY.md must come after Confidentiality (stable prefix): conf=%d prof=%d mem=%d", confIdx, profIdx, memIdx)
	}
}
