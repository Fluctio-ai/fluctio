package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// TestApplyMemoryUpdates pins the fail-closed apply rules: a unique
// target applies; ambiguous, too-short, and empty-new updates are
// skipped; the per-round cap holds.
func TestApplyMemoryUpdates(t *testing.T) {
	text := "- User uses Vim as their editor\n- User lives in Shanghai\n- User lives in Shanghai\n"
	got, archived := applyMemoryUpdates(text, []persistUpdate{
		{Target: "User uses Vim as their editor", New: "User uses Neovim with Lua config"},
		{Target: "User lives in Shanghai", New: "User lives in Beijing"}, // ambiguous (x2)
		{Target: "Shanghai", New: "Beijing"},                             // too short
		{Target: "User uses Neovim with Lua config", New: "   "},         // empty new
	})
	if len(archived) != 1 || archived[0] != "User uses Vim as their editor" {
		t.Fatalf("archived=%v want the single applied target", archived)
	}
	want := "- User uses Neovim with Lua config\n- User lives in Shanghai\n- User lives in Shanghai\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}

	var ups []persistUpdate
	var base strings.Builder
	for i := 0; i < 7; i++ {
		base.WriteString(fmt.Sprintf("- unique entry number %d here\n", i))
		ups = append(ups, persistUpdate{
			Target: fmt.Sprintf("unique entry number %d here", i),
			New:    fmt.Sprintf("rewritten entry %d", i),
		})
	}
	got, archived = applyMemoryUpdates(base.String(), ups)
	if len(archived) != maxPersistUpdates {
		t.Fatalf("applied=%d want %d (cap)", len(archived), maxPersistUpdates)
	}
	if strings.Contains(got, "rewritten entry 5") || strings.Contains(got, "rewritten entry 6") {
		t.Fatalf("updates past the cap must not apply:\n%s", got)
	}
	if !strings.Contains(got, "unique entry number 5 here") || !strings.Contains(got, "unique entry number 6 here") {
		t.Fatalf("entries past the cap must stay untouched:\n%s", got)
	}
}

// TestAutoPersistAppliesUpdates walks the write-time UPDATE path end to
// end: extract proposes an update, the audit echoes it, the existing
// MEMORY.md entry is merged in place — no near-duplicate appended, no
// auto-persist section header for an updates-only round.
func TestAutoPersistAppliesUpdates(t *testing.T) {
	mem := NewMemory(t.TempDir())
	seed := "## Preferences\n- User uses Vim as their editor\n"
	if err := mem.SaveMemory(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	update := `{"memory_facts": [], "user_notes": [], "memory_updates": [{"target": "User uses Vim as their editor", "new": "User uses Neovim with a Lua config (switched 2026-10)"}], "user_updates": []}`
	prov := &persistStubProvider{resps: []string{update, update}}

	AutoPersistMemory(context.Background(), mem, prov, "stub", []provider.Message{
		{Role: "user", Content: "我换 Neovim 了，配置用 Lua 写的"},
	})

	if prov.calls != 2 {
		t.Fatalf("calls=%d want 2 (extract + audit)", prov.calls)
	}
	got := mem.LoadMemory()
	if !strings.Contains(got, "User uses Neovim with a Lua config (switched 2026-10)") {
		t.Fatalf("merged entry missing: %q", got)
	}
	if strings.Contains(got, "Vim as their editor") {
		t.Fatalf("old entry must be replaced, not duplicated: %q", got)
	}
	if strings.Contains(got, "## Auto-persisted") {
		t.Fatalf("updates-only round must not append an auto-persist section: %q", got)
	}
}

// TestAutoPersistAuditDropsUpdate: when the audit pass rejects the only
// proposal, nothing is written — the file survives the round unchanged.
func TestAutoPersistAuditDropsUpdate(t *testing.T) {
	mem := NewMemory(t.TempDir())
	seed := "## Preferences\n- User uses Vim as their editor\n"
	if err := mem.SaveMemory(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	update := `{"memory_updates": [{"target": "User uses Vim as their editor", "new": "User uses Neovim with a Lua config"}]}`
	dropped := `{"memory_facts": [], "user_notes": [], "memory_updates": [], "user_updates": []}`
	prov := &persistStubProvider{resps: []string{update, dropped}}

	AutoPersistMemory(context.Background(), mem, prov, "stub", []provider.Message{
		{Role: "user", Content: "unverifiable claim"},
	})

	if got := mem.LoadMemory(); got != seed {
		t.Fatalf("file must be unchanged when the audit drops the update: %q", got)
	}
}

// TestVerifyPersistedMemoriesUpdateIntersect covers the audit gate for
// updates: a kept update survives (fuzzy new-text match + exact target),
// an auditor-invented update is dropped, and an update whose target was
// rephrased is dropped — a fuzzy target could redirect the rewrite onto
// the wrong entry.
func TestVerifyPersistedMemoriesUpdateIntersect(t *testing.T) {
	proposed := persistExtraction{
		MemoryUpdates: []persistUpdate{
			{Target: "User uses Vim as their editor", New: "User uses Neovim with a Lua config"},
		},
	}
	keptResp := `{"memory_updates": [
		{"target": "User uses Vim as their editor", "new": "User uses Neovim with a Lua config"},
		{"target": "User works on Rust projects", "new": "User works on Rust and Zig"},
		{"target": "User uses Vim as their editor daily", "new": "User uses Neovim with a Lua config"}
	]}`
	stub := &persistStubProvider{resp: keptResp}

	got, err := verifyPersistedMemories(context.Background(), stub, "m", "[user]: I switched to Neovim", proposed)
	if err != nil {
		t.Fatalf("verify failed: %v", err)
	}
	if len(got.MemoryUpdates) != 1 {
		t.Fatalf("updates kept=%d want 1: %+v", len(got.MemoryUpdates), got.MemoryUpdates)
	}
	if got.MemoryUpdates[0].Target != "User uses Vim as their editor" {
		t.Fatalf("kept update must carry the proposed target verbatim: %+v", got.MemoryUpdates[0])
	}
}
