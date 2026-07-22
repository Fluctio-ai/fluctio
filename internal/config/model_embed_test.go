package config

import (
	"os"
	"strings"
	"testing"
)

// metaEq compares only the routing-relevant fields. InputModalities /
// OutputModalities are projected from docs/models.json and vary
// independently of the context-window assertions these tests check.
func metaEq(a, b ModelMeta) bool {
	return a.ContextWindow == b.ContextWindow && a.MaxTokens == b.MaxTokens
}

// TestBuiltinMetaTable verifies the embedded model_meta.json parses and
// carries both ContextWindow and MaxTokens for known models.
func TestBuiltinMetaTable(t *testing.T) {
	tbl := builtinMetaTable()
	if tbl == nil {
		t.Fatal("builtinMetaTable returned nil — embed failed to parse")
	}
	cases := map[string]ModelMeta{
		"claude-opus-4-8": {ContextWindow: 1000000, MaxTokens: 128000},
		"claude-sonnet-4": {ContextWindow: 1000000, MaxTokens: 64000},
		"gpt-5":           {ContextWindow: 400000, MaxTokens: 128000},
		"deepseek-chat":   {ContextWindow: 1000000, MaxTokens: 384000},
		// grok-4-fast has no maxOutputTokens in the source → MaxTokens stays 0.
		"grok-4-fast": {ContextWindow: 2000000, MaxTokens: 0},
	}
	for key, want := range cases {
		if got := tbl[key]; !metaEq(got, want) {
			t.Errorf("builtinMetaTable()[%q] = %+v, want %+v", key, got, want)
		}
	}
	if len(tbl) < 600 {
		t.Errorf("builtin meta table too small: %d entries", len(tbl))
	}
}

// TestBuiltinMetaTableModalities verifies the inputModalities /
// outputModalities projection from docs/models.json: a known vision model
// carries "image" input; a known text-only model does not.
func TestBuiltinMetaTableModalities(t *testing.T) {
	tbl := builtinMetaTable()
	if tbl == nil {
		t.Fatal("builtinMetaTable returned nil")
	}
	if got := tbl["gpt-4o"]; !got.SupportsVision() {
		t.Errorf("gpt-4o should be vision-capable, got inputModalities=%v", got.InputModalities)
	}
	if got := tbl["longcat-flash-chat"]; got.SupportsVision() {
		t.Errorf("longcat-flash-chat should be text-only, got inputModalities=%v", got.InputModalities)
	}
}

// TestMergedMetaTableLocalOverrides verifies a local override file wholly
// replaces the builtin entry for the same key while builtin entries are
// preserved for keys the local file doesn't touch.
func TestMergedMetaTableLocalOverrides(t *testing.T) {
	tmp := t.TempDir() + "/model-meta.json"
	// Override claude-opus-4-8 entirely + add a new custom model.
	local := `{"claude-opus-4-8": {"contextWindow": 999, "maxTokens": 111}, "my-custom-model": {"contextWindow": 500, "maxTokens": 50}}`
	if err := os.WriteFile(tmp, []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	merged := mergedMetaTable(tmp)
	if got := merged["claude-opus-4-8"]; !metaEq(got, ModelMeta{ContextWindow: 999, MaxTokens: 111}) {
		t.Errorf("local should wholly override builtin claude-opus-4-8: got %+v", got)
	}
	if got := merged["my-custom-model"]; !metaEq(got, ModelMeta{ContextWindow: 500, MaxTokens: 50}) {
		t.Errorf("local-only entry missing: got %+v", got)
	}
	// Builtin entry untouched by the local file is preserved.
	if got := merged["gpt-5"]; !metaEq(got, ModelMeta{ContextWindow: 400000, MaxTokens: 128000}) {
		t.Errorf("builtin entry lost after merge: got %+v", got)
	}
}

// TestMergedMetaTableNoLocalFile verifies the builtin table still loads
// when the local override path is missing.
func TestMergedMetaTableNoLocalFile(t *testing.T) {
	merged := mergedMetaTable("/nonexistent/path.json")
	if got := merged["claude-opus-4-8"]; !metaEq(got, ModelMeta{ContextWindow: 1000000, MaxTokens: 128000}) {
		t.Errorf("builtin should still load without local file: got %+v", got)
	}
}

// TestMergedMetaTableToleratesCommentKeys verifies the local override
// parser skips non-ModelMeta values (e.g. a "_comment" string in the
// seeded example file) rather than rejecting the whole file.
func TestMergedMetaTableToleratesCommentKeys(t *testing.T) {
	tmp := t.TempDir() + "/model-meta.json"
	local := `{"_comment": "human-readable note", "claude-opus-4-8": {"contextWindow": 999, "maxTokens": 111}}`
	if err := os.WriteFile(tmp, []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	merged := mergedMetaTable(tmp)
	if got := merged["claude-opus-4-8"]; !metaEq(got, ModelMeta{ContextWindow: 999, MaxTokens: 111}) {
		t.Errorf("valid entry should survive _comment: got %+v", got)
	}
	if _, present := merged["_comment"]; present {
		t.Error("_comment key should be skipped, not stored")
	}
}

// TestEnsureLocalModelMetaSeed writes the example file when absent and
// leaves it untouched when present (user edits preserved).
func TestEnsureLocalModelMetaSeed(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/model-meta.json"

	if err := EnsureLocalModelMetaSeed(dir); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("seed not written: %v", err)
	}
	if !strings.Contains(string(data), "_comment") || !strings.Contains(string(data), "example-model-id") {
		t.Errorf("seed missing comment/example: %s", data)
	}

	// Present → preserved (re-seed must not clobber user content).
	user := `{"my-model": {"contextWindow": 123, "maxTokens": 7}}`
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLocalModelMetaSeed(dir); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "my-model") {
		t.Errorf("user edit clobbered by re-seed: %s", data)
	}

	if err := EnsureLocalModelMetaSeed(""); err != nil {
		t.Errorf("empty homeDir should no-op: %v", err)
	}
}
