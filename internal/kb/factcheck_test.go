package kb

import (
	"testing"
)

// Pure-function tests for the grounding primitives. The integrated path
// (Search / SearchRawKB → KBResult → match) is covered by the store-level
// tests; here we pin the sentence splitter and the normaliser that decide
// what counts as a "verbatim match".

func TestSplitClaimSentences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"CN periods", "今天天气很好。适合出门。但明天下雨。", []string{"今天天气很好", "适合出门", "但明天下雨"}},
		{"EN periods", "First sentence. Second one.", []string{"First sentence", "Second one"}},
		{"mixed newlines", "line one here\ntwo line\nthree here", []string{"line one here", "two line", "three here"}},
		{"drops short filler", "嗯。OK。这是一个真正的句子。", []string{"这是一个真正的句子"}},
		{"empty", "", nil},
		{"whitespace only", "   \n  ", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitClaimSentences(tc.in)
			if !eqStrSlice(got, tc.want) {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestNormalizeForMatch(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"lowercases ASCII", "Hello World", "helloworld"},
		{"strips indentation/newlines", "  a b  c\n", "abc"},
		{"strips ideographic space", "甲 乙　丙", "甲乙丙"},
		{"empty", "", ""},
		{"CJK unchanged case-wise", "中文不変", "中文不変"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeForMatch(tc.in); got != tc.want {
				t.Errorf("normalizeForMatch(%q) = %q want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A verbatim sentence (modulo whitespace) sourced from a claimSource must
// resolve to sourced; a paraphrase with the same meaning but different
// bytes must not. Guards the core promise of the tool directly, without
// needing a live KBStore.
func TestRunVerifyClaim_SourceBucketMatching(t *testing.T) {
	src := claimSource{
		title: "测试源",
		raw:   "公司成立于 2018 年,总部在上海。\n主要产品是智能助手。\n",
		norm:  normalizeForMatch("公司成立于 2018 年,总部在上海。\n主要产品是智能助手。\n"),
	}
	// Sourced: appears verbatim modulo whitespace.
	sourced := normalizeForMatch("总部在上海")
	if !contains(src.norm, sourced) {
		t.Errorf("verbatim sentence should match: norm=%q looking for %q", src.norm, sourced)
	}
	// Paraphrase: same meaning, different bytes — must NOT match.
	paraphrase := normalizeForMatch("公司把总部设在了上海这座城市")
	if contains(src.norm, paraphrase) {
		t.Errorf("paraphrase must NOT match (verbatim-only is the contract): got hit in %q", src.norm)
	}
}

func eqStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(haystack, needle string) bool { return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0) }

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
