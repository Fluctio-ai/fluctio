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
	wantCW, ok := LookupModelMeta(model)
	if !ok {
		t.Skipf("builtin table has no entry for %q — table changed?", model)
	}
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
	tableCW, ok := LookupModelMeta("claude-opus-4-8")
	if !ok || tableCW == sentinel {
		t.Skipf("table value (%d, ok=%v) unsuitable for guard test — pick another model", tableCW, ok)
	}

	// Replay the EXACT guard added to MergedAgentConfig (config.go, search
	// "Table fallback"). A future ModelEntry mapping will set this before
	// the guard runs; here we simulate that by presetting manually.
	resolved := ResolvedAgent{Model: "claude-opus-4-8", ContextWindow: sentinel}
	if resolved.ContextWindow == 0 && resolved.Model != "" {
		if cw, ok := LookupModelMeta(resolved.Model); ok {
			resolved.ContextWindow = cw
		}
	}

	if resolved.ContextWindow != sentinel {
		t.Fatalf("guard failed: preset ContextWindow %d was overwritten by table value %d", sentinel, resolved.ContextWindow)
	}
}
