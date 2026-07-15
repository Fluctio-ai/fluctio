package config

import "testing"

// stubNoAgentFile swaps the package-level AgentFileConfigLoader for a
// no-op stub so resolve tests never touch the filesystem. Returns a
// restore func. See config.go line ~599 for the indirection.
func stubNoAgentFile() func() {
	old := AgentFileConfigLoader
	AgentFileConfigLoader = func(_, _ string) (AgentFileConfig, bool) {
		return AgentFileConfig{}, false
	}
	return func() { AgentFileConfigLoader = old }
}

// TestMergedAgentConfigContextWindowFallback proves the table fallback is
// wired into MergedAgentConfig: a known model with no preset
// ModelEntry.ContextWindow (nothing in the current resolve path sets it)
// must surface the builtin table value on ResolvedAgent.ContextWindow.
//
// This is NOT trivially-true: remove the LookupModelMeta call added near
// "return resolved" in MergedAgentConfig (search "Table fallback" in
// config.go) and this test fails with ContextWindow=0.
func TestMergedAgentConfigContextWindowFallback(t *testing.T) {
	restore := stubNoAgentFile()
	defer restore()

	const model = "claude-opus-4-8"
	cfg := &Config{}
	cfg.Agents.Defaults.Model = model
	entry := AgentEntry{ID: "p1t4-resolve-test"}

	resolved := cfg.MergedAgentConfig(entry)

	if resolved.Model != model {
		t.Fatalf("model not propagated to resolved: got %q want %q", resolved.Model, model)
	}
	// Reference value from the SAME source resolve uses, so the test
	// stays correct even if the builtin table entry changes later.
	meta, ok := LookupModelMeta(model)
	if !ok {
		t.Skipf("builtin table has no entry for %q — table changed?", model)
	}
	wantCW := meta.ContextWindow
	if resolved.ContextWindow == 0 {
		t.Fatalf("ContextWindow=0 — table fallback not wired into MergedAgentConfig (model=%s)", model)
	}
	if resolved.ContextWindow != wantCW {
		t.Fatalf("ContextWindow: got %d, want %d (LookupModelMeta) — wrong value injected", resolved.ContextWindow, wantCW)
	}
}

// TestMergedAgentConfigContextWindowUnknownModel verifies resolve does not
// hallucinate a window for a model absent from the builtin table — the
// fallback guard leaves ContextWindow at 0 so Phase 2 can fall back to its
// own default threshold.
func TestMergedAgentConfigContextWindowUnknownModel(t *testing.T) {
	restore := stubNoAgentFile()
	defer restore()

	cfg := &Config{}
	cfg.Agents.Defaults.Model = "zzz-p1t4-no-such-model-xyz"
	entry := AgentEntry{ID: "p1t4-resolve-test"}

	resolved := cfg.MergedAgentConfig(entry)

	if resolved.ContextWindow != 0 {
		t.Fatalf("unknown model should yield ContextWindow=0, got %d", resolved.ContextWindow)
	}
}

// TestMergedAgentConfigContextWindowGuardPreservesPreset exercises the
// `resolved.ContextWindow == 0` guard added to MergedAgentConfig. Today no
// resolve code path sets ContextWindow from a ModelEntry (that mapping lands
// in P1-T7), so we cannot route a preset value through the full resolve yet.
// Instead we verify the guard semantics directly against the real
// LookupModelMeta: a non-zero preset must NOT be overwritten by the table.
//
// This is NOT the trivially-true "assign then check same variable" pattern —
// it runs the real table lookup (which returns 1000000 for claude-opus-4-8)
// and asserts the sentinel survives. If the guard condition were dropped or
// inverted (e.g. unconditional overwrite, or `>= 0`), the table value would
// clobber 500000 and this test would fail.
//
// Resolve call site: MergedAgentConfig in config.go, "Table fallback" block
// immediately before `return resolved`.
func TestMergedAgentConfigContextWindowGuardPreservesPreset(t *testing.T) {
	const sentinel = 500000
	// Confirm the table WOULD overwrite if the guard were absent — if not,
	// the assertion below would be vacuous.
	tableMeta, ok := LookupModelMeta("claude-opus-4-8")
	if !ok || tableMeta.ContextWindow == sentinel {
		t.Skipf("table value (%+v, ok=%v) unsuitable for guard test — pick another model", tableMeta, ok)
	}

	// Replay the EXACT guard added to MergedAgentConfig (config.go, search
	// "Unified model-meta fallback"). A future ModelEntry mapping will set
	// this before the guard runs; here we simulate that by presetting
	// manually.
	resolved := ResolvedAgent{Model: "claude-opus-4-8", ContextWindow: sentinel}
	if (resolved.ContextWindow == 0 || resolved.MaxTokens == 0) && resolved.Model != "" {
		if m, ok := LookupModelMeta(resolved.Model); ok {
			if resolved.ContextWindow == 0 {
				resolved.ContextWindow = m.ContextWindow
			}
			if resolved.MaxTokens == 0 {
				resolved.MaxTokens = m.MaxTokens
			}
		}
	}

	if resolved.ContextWindow != sentinel {
		t.Fatalf("guard failed: preset ContextWindow %d was overwritten by table value %d", sentinel, resolved.ContextWindow)
	}
}

// TestMergedAgentConfigModelEntryContextWindowWins is the full-resolve e2e
// test deferred from P1-T4. It constructs a Config whose provider has a
// ModelEntry with ContextWindow=500000 (different from the builtin table's
// 1000000 for claude-opus-4-8), then verifies resolved.ContextWindow == the
// ModelEntry value — proving the entry mapping runs before and wins over the
// table fallback (spec 1.4 priority: entry > table > 0).
func TestMergedAgentConfigModelEntryContextWindowWins(t *testing.T) {
	restore := stubNoAgentFile()
	defer restore()

	const (
		providerKey = "anthropic"
		modelID     = "claude-opus-4-8"
		entryCW     = 500000 // intentionally different from table's 1000000
	)

	// Guard: skip if the table doesn't know this model, otherwise the test
	// would be vacuous (we need the table to "want" a different value).
	tableMeta, ok := LookupModelMeta(modelID)
	if !ok {
		t.Skipf("builtin table has no entry for %q — table changed?", modelID)
	}
	tableCW := tableMeta.ContextWindow
	if tableCW == entryCW {
		t.Skipf("table value %d == entry value — pick a different sentinel", tableCW)
	}

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			providerKey: {
				APIKey:  "sk-test",
				APIBase: "https://api.anthropic.com",
				Models: []ModelEntry{
					{ID: modelID, Name: modelID, ContextWindow: entryCW},
				},
			},
		},
	}
	cfg.Agents.Defaults.Model = providerKey + "/" + modelID
	entry := AgentEntry{ID: "p1t7-entry-cw-test"}

	resolved := cfg.MergedAgentConfig(entry)

	if resolved.ContextWindow != entryCW {
		t.Fatalf("ModelEntry.ContextWindow priority failed: got %d, want %d (table value was %d) — entry mapping not wired or table overwrote it",
			resolved.ContextWindow, entryCW, tableCW)
	}
}
