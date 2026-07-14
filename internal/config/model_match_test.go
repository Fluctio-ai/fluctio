package config

import "testing"

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
