package agent

import (
	"reflect"
	"strings"
	"testing"
)

func TestExtractReplyOptionsNoBlock(t *testing.T) {
	text := "plain reply, nothing to see"
	clean, opts := ExtractReplyOptions(text)
	if clean != text || opts != nil {
		t.Fatalf("expected passthrough, got %q %v", clean, opts)
	}
}

func TestExtractReplyOptionsBasic(t *testing.T) {
	text := "你想怎么处理?\n\n```options\n[\"方案一：重构\", \"方案二：保守修复\"]\n```\n"
	clean, opts := ExtractReplyOptions(text)
	if strings.Contains(clean, "options") || strings.Contains(clean, "[") {
		t.Fatalf("block not stripped: %q", clean)
	}
	if want := []string{"方案一：重构", "方案二：保守修复"}; !reflect.DeepEqual(opts, want) {
		t.Fatalf("opts = %v, want %v", opts, want)
	}
}

func TestExtractReplyOptionsMultipleBlocksMergeDedup(t *testing.T) {
	text := "first\n```options\n[\"A\",\"B\"]\n```\nmiddle\n```options\n[\"B\",\"C\"]\n```\n"
	_, opts := ExtractReplyOptions(text)
	if want := []string{"A", "B", "C"}; !reflect.DeepEqual(opts, want) {
		t.Fatalf("opts = %v, want %v", opts, want)
	}
}

func TestExtractReplyOptionsBadJSONStillStrips(t *testing.T) {
	text := "reply\n```options\nthis is not json at all\n```\nend"
	clean, opts := ExtractReplyOptions(text)
	if strings.Contains(clean, "not json") {
		t.Fatalf("bad block must still be stripped: %q", clean)
	}
	if opts != nil {
		t.Fatalf("bad block must contribute no options, got %v", opts)
	}
	if !strings.Contains(clean, "reply") || !strings.Contains(clean, "end") {
		t.Fatalf("surrounding text lost: %q", clean)
	}
}

func TestExtractReplyOptionsUnclosedBlockSwallowed(t *testing.T) {
	text := "reply body\n```options\n[\"A\",\"B\"]\n" // never closed
	clean, opts := ExtractReplyOptions(text)
	if strings.Contains(clean, "```") || strings.Contains(clean, "\"A\"") {
		t.Fatalf("open block must be swallowed: %q", clean)
	}
	if opts != nil {
		t.Fatalf("open block must contribute nothing, got %v", opts)
	}
	if !strings.Contains(clean, "reply body") {
		t.Fatalf("prefix lost: %q", clean)
	}
}

func TestExtractReplyOptionsCapsCountAndRunes(t *testing.T) {
	long := strings.Repeat("长", 60)
	text := "```options\n[\"" + long + "\",\"1\",\"2\",\"3\",\"4\",\"5\"]\n```\n"
	_, opts := ExtractReplyOptions(text)
	if len(opts) != replyOptionsMaxCount {
		t.Fatalf("count cap = %d, got %d options", replyOptionsMaxCount, len(opts))
	}
	for _, o := range opts {
		if len([]rune(o)) > replyOptionsMaxRunes {
			t.Fatalf("rune cap violated: %q", o)
		}
	}
}

func TestExtractReplyOptionsTolerantJSON(t *testing.T) {
	// llmjson chain handles fenced/wrapper variants; the bare-array form
	// is the most common drift, verify it flows through here.
	text := "```options\n[\"继续\", \"停止\"]\n```"
	_, opts := ExtractReplyOptions(text)
	if want := []string{"继续", "停止"}; !reflect.DeepEqual(opts, want) {
		t.Fatalf("opts = %v, want %v", opts, want)
	}
}
