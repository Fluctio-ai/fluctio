package config

import (
	"os"
	"reflect"
	"testing"
)

// TestLookupMetaSubstringLongestFirst exercises the unified matcher against
// the builtin table (no local override): substring matching with
// longest-first tiebreak, returning a full ModelMeta.
func TestLookupMetaSubstringLongestFirst(t *testing.T) {
	cases := []struct {
		id     string
		want   ModelMeta
		reason string
	}{
		{"claude-opus-4-8", ModelMeta{ContextWindow: 1000000, MaxTokens: 128000}, "exact key"},
		{"claude-opus-4-8-20250929", ModelMeta{ContextWindow: 1000000, MaxTokens: 128000}, "version suffix -> claude-opus-4-8"},
		{"anthropic/claude-sonnet-4-6", ModelMeta{ContextWindow: 1000000, MaxTokens: 128000}, "provider prefix -> claude-sonnet-4-6"},
		// longest-first: both claude-opus-4 and claude-opus-4-6 are substrings;
		// the longer key (claude-opus-4-6, cw=1000000) wins over the shorter
		// (claude-opus-4, cw=200000).
		{"claude-opus-4-6-preview", ModelMeta{ContextWindow: 1000000, MaxTokens: 128000}, "longest-first -> claude-opus-4-6 beats claude-opus-4"},
		{"openai/gpt-5-preview", ModelMeta{ContextWindow: 400000, MaxTokens: 128000}, "gpt-5 substring match"},
		{"zzz-no-such-model-xyz", ModelMeta{}, "no match -> zero value + matched=false"},
	}
	for _, c := range cases {
		got, ok := lookupMetaIn(c.id, "")
		if c.reason == "no match -> zero value + matched=false" {
			if ok {
				t.Errorf("%s: expected no match, got %+v", c.id, got)
			}
			continue
		}
		if !ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got (%+v,%v), want %+v — %s", c.id, got, ok, c.want, c.reason)
		}
	}
}

// TestLookupMetaLocalOverride verifies the local override file participates
// in the merged table + longest-first match (whole-object replacement).
func TestLookupMetaLocalOverride(t *testing.T) {
	tmp := t.TempDir() + "/model-meta.json"
	local := `{"claude-opus-4-8": {"contextWindow": 999, "maxTokens": 111}, "claude-opus": {"contextWindow": 500, "maxTokens": 55}}`
	if err := os.WriteFile(tmp, []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id   string
		want ModelMeta
	}{
		// Local override wins over builtin for the same key.
		{"anthropic/claude-opus-4-8", ModelMeta{ContextWindow: 999, MaxTokens: 111}},
		// longest-first between the two local keys.
		{"claude-opus-4-8-preview", ModelMeta{ContextWindow: 999, MaxTokens: 111}},
		{"claude-opus-mini", ModelMeta{ContextWindow: 500, MaxTokens: 55}},
	}
	for _, c := range cases {
		got, ok := lookupMetaIn(c.id, tmp)
		if !ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got (%+v,%v), want %+v", c.id, got, ok, c.want)
		}
	}
}
