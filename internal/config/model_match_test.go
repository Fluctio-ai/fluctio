package config

import (
	"os"
	"testing"
)

func TestLookupModelMetaSubstringLongestFirst(t *testing.T) {
	// 不设本地覆盖（用内置表）
	cases := []struct {
		id     string
		want   int
		reason string
	}{
		{"claude-opus-4-8-20250929", 1000000, "版本后缀 → claude-opus-4-8"},
		{"anthropic/claude-sonnet-4-6", 1000000, "provider 前缀 → claude-sonnet-4-6"},
		{"claude-3-5-sonnet", 200000, "无精确 → catch-all claude"},
		{"gpt-5.4-mini", 400000, "gpt-5.4-mini 优先于 gpt-5.4 / gpt-5（longest-first）"},
		{"gpt-5", 400000, "gpt-5 catch-all"},
		{"some-unknown-model", 0, "无匹配 → matched=false"},
	}
	for _, c := range cases {
		got, ok := lookupModelMetaIn(c.id, "") // 用显式表入口测，避免读磁盘
		if c.want == 0 {
			if ok {
				t.Errorf("%s: expected no match, got %d", c.id, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("%s: got (%d,%v), want %d — %s", c.id, got, ok, c.want, c.reason)
		}
	}
}

// ---------------------------------------------------------------------------
// maxTokens lookup 测试（与 contextWindow lookup 平行）
// ---------------------------------------------------------------------------

func TestLookupMaxTokensEmptyBuiltin(t *testing.T) {
	// 内置表为空 — 无本地覆盖时应 unmatched
	_, ok := lookupMaxTokensIn("claude-sonnet-4-6", "")
	if ok {
		t.Error("expected no match on empty builtin + no local override")
	}
}

func TestLookupMaxTokensLocalOverride(t *testing.T) {
	tmp := t.TempDir() + "/model-maxtokens.json"
	local := `{"claude-sonnet-4-6": 16384, "gpt-5": 32768, "claude": 8192}`
	if err := os.WriteFile(tmp, []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id     string
		want   int
		reason string
	}{
		{"anthropic/claude-sonnet-4-6", 16384, "longest-first → claude-sonnet-4-6 而非 catch-all claude"},
		{"claude-opus-4-8", 8192, "catch-all claude"},
		{"openai/gpt-5-preview", 32768, "gpt-5 子串匹配"},
		{"unknown-model", 0, "无匹配 → matched=false"},
	}
	for _, c := range cases {
		got, ok := lookupMaxTokensIn(c.id, tmp)
		if c.want == 0 {
			if ok {
				t.Errorf("%s: expected no match, got %d", c.id, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("%s: got (%d,%v), want %d — %s", c.id, got, ok, c.want, c.reason)
		}
	}
}
