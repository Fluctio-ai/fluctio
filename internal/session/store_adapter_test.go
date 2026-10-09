package session

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDisplaySessionTitle(t *testing.T) {
	tests := []struct {
		name        string
		storedTitle string
		sessionKey  string
		preview     string
		want        string
	}{
		{
			name:       "empty title uses first user message",
			sessionKey: "s-1783753587119-lvlph0",
			preview:    "帮我分析一下这个问题",
			want:       "帮我分析一下这个问题",
		},
		{
			name:        "legacy session id title uses first user message",
			storedTitle: "s-1783753587119-lvlph0",
			sessionKey:  "s-1783753587119-lvlph0",
			preview:     "帮我分析一下这个问题",
			want:        "帮我分析一下这个问题",
		},
		{
			name:        "custom title is preserved",
			storedTitle: "故障排查",
			sessionKey:  "s-1783753587119-lvlph0",
			preview:     "帮我分析一下这个问题",
			want:        "故障排查",
		},
		{
			name:        "surrounding whitespace is normalized",
			storedTitle: "  故障排查  ",
			sessionKey:  "s-1783753587119-lvlph0",
			preview:     "帮我分析一下这个问题",
			want:        "故障排查",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := displaySessionTitle(tt.storedTitle, tt.sessionKey, tt.preview); got != tt.want {
				t.Fatalf("displaySessionTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	// The reported bug: byte-wise cuts split multi-byte UTF-8 chars and the
	// sidebar showed mojibake at the truncation point.
	cjk := strings.Repeat("故", 120) // 120 runes, 360 bytes
	got := truncateRunes(cjk, 60)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateRunes produced invalid UTF-8: %q", got)
	}
	if want := strings.Repeat("故", 60) + "..."; got != want {
		t.Fatalf("truncateRunes(120 CJK, 60) = %q, want %q", got, want)
	}
	short := "短标题"
	if got := truncateRunes(short, 60); got != short {
		t.Fatalf("truncateRunes(short) = %q, want unchanged %q", got, short)
	}
}
