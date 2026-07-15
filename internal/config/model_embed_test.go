package config

import (
	"os"
	"reflect"
	"testing"
)

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
		if got := tbl[key]; !reflect.DeepEqual(got, want) {
			t.Errorf("builtinMetaTable()[%q] = %+v, want %+v", key, got, want)
		}
	}
	if len(tbl) < 600 {
		t.Errorf("builtin meta table too small: %d entries", len(tbl))
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
	if got := merged["claude-opus-4-8"]; !reflect.DeepEqual(got, ModelMeta{ContextWindow: 999, MaxTokens: 111}) {
		t.Errorf("local should wholly override builtin claude-opus-4-8: got %+v", got)
	}
	if got := merged["my-custom-model"]; !reflect.DeepEqual(got, ModelMeta{ContextWindow: 500, MaxTokens: 50}) {
		t.Errorf("local-only entry missing: got %+v", got)
	}
	// Builtin entry untouched by the local file is preserved.
	if got := merged["gpt-5"]; !reflect.DeepEqual(got, ModelMeta{ContextWindow: 400000, MaxTokens: 128000}) {
		t.Errorf("builtin entry lost after merge: got %+v", got)
	}
}

// TestMergedMetaTableNoLocalFile verifies the builtin table still loads
// when the local override path is missing.
func TestMergedMetaTableNoLocalFile(t *testing.T) {
	merged := mergedMetaTable("/nonexistent/path.json")
	if got := merged["claude-opus-4-8"]; !reflect.DeepEqual(got, ModelMeta{ContextWindow: 1000000, MaxTokens: 128000}) {
		t.Errorf("builtin should still load without local file: got %+v", got)
	}
}
