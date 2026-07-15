package config

import (
	"os"
	"testing"
)

func TestBuiltinModelTableKnownEntries(t *testing.T) {
	tbl := builtinModelTable()
	cases := map[string]int{
		"claude-opus-4-8": 1000000,
		"gpt-5.4":         1050000,
		"gemini":          1048576,
		"deepseek-chat":   1000000,
		"glm-5.2":         1048576,
		"grok-4-fast":     2000000,
		"claude":          200000, // catch-all
	}
	for key, want := range cases {
		if got := tbl[key]; got != want {
			t.Errorf("builtinModelTable()[%q] = %d, want %d", key, got, want)
		}
	}
	if len(tbl) < 50 {
		t.Errorf("builtin table too small: %d entries", len(tbl))
	}
}

func TestMergedModelTableLocalOverrides(t *testing.T) {
	tmp := t.TempDir() + "/model-context.json"
	// 本地覆盖 claude 的 catch-all + 加一条新模型
	local := `{"claude": 500000, "my-custom-model": 99000}`
	if err := os.WriteFile(tmp, []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	merged := mergedModelTable(tmp)
	if merged["claude"] != 500000 {
		t.Errorf("local should override builtin claude: got %d", merged["claude"])
	}
	if merged["my-custom-model"] != 99000 {
		t.Errorf("local-only entry missing: %d", merged["my-custom-model"])
	}
	if merged["gpt-5.4"] != 1050000 {
		t.Errorf("builtin entry lost after merge: %d", merged["gpt-5.4"])
	}
}

func TestMergedModelTableNoLocalFile(t *testing.T) {
	merged := mergedModelTable("/nonexistent/path.json")
	if merged["gemini"] != 1048576 {
		t.Errorf("builtin should still load without local file: %d", merged["gemini"])
	}
}

// ---------------------------------------------------------------------------
// maxTokens 表测试（与 contextWindow 平行，互不干扰）
// ---------------------------------------------------------------------------

func TestBuiltinMaxTokensTable(t *testing.T) {
	tbl := builtinMaxTokensTable()
	if tbl == nil {
		t.Fatal("builtinMaxTokensTable returned nil — empty JSON should still parse to non-nil map")
	}
	if len(tbl) != 0 {
		t.Errorf("builtin maxTokens table should be empty by default, got %d entries", len(tbl))
	}
}

func TestMergedMaxTokensTableLocalOverrides(t *testing.T) {
	tmp := t.TempDir() + "/model-maxtokens.json"
	local := `{"claude-sonnet-4-6": 16384, "gpt-5": 32768}`
	if err := os.WriteFile(tmp, []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	merged := mergedMaxTokensTable(tmp)
	if merged["claude-sonnet-4-6"] != 16384 {
		t.Errorf("local maxTokens entry missing: %d", merged["claude-sonnet-4-6"])
	}
	if merged["gpt-5"] != 32768 {
		t.Errorf("local maxTokens entry missing: %d", merged["gpt-5"])
	}
}

func TestMergedMaxTokensTableNoLocalFile(t *testing.T) {
	merged := mergedMaxTokensTable("/nonexistent/maxtokens.json")
	// 内置空表 — 应返回空 map，不 panic
	if merged == nil {
		t.Fatal("mergedMaxTokensTable returned nil for missing local file")
	}
	if len(merged) != 0 {
		t.Errorf("expected empty table, got %d entries", len(merged))
	}
}
