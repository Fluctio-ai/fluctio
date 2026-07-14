package config

import "testing"

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
