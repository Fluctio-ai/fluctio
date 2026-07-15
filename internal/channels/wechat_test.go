package channels

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestWechatNormalizeMarkdownBlocks(t *testing.T) {
	cases := []struct{ in, want string }{
		// runs of blank lines collapse to one outside code blocks
		{"a\n\n\n\nb", "a\n\nb"},
		{"a\n\nb", "a\n\nb"},
		// surrounding blank lines + leading/trailing whitespace stripped
		{"\n\na\n\n", "a"},
		// blank lines inside a fenced block are preserved verbatim
		{"```\n\n\nx\n\n\n```", "```\n\n\nx\n\n\n```"},
	}
	for _, c := range cases {
		if got := wechatNormalizeMarkdownBlocks(c.in); got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWechatWrapCopyFriendlyLines(t *testing.T) {
	// A long paragraph with spaces: every wrapped line fits the width.
	long := strings.Repeat("the quick brown fox ", 12)
	for _, line := range strings.Split(wechatWrapCopyFriendlyLines(long), "\n") {
		if n := utf8.RuneCountInString(line); n > wechatCopyLineWidth {
			t.Errorf("wrapped line %d runes > %d: %q", n, wechatCopyLineWidth, line)
		}
	}
	// Fenced code block lines are never wrapped, even when huge.
	code := "```\n" + strings.Repeat("x", 300) + "\n```"
	if got := wechatWrapCopyFriendlyLines(code); got != code {
		t.Errorf("code block should be preserved, got %q", got)
	}
	// Table rows (start with |) and the separator row are preserved.
	row := "| " + strings.Repeat("aa ", 80) + "|"
	if got := wechatWrapCopyFriendlyLines(row); got != row {
		t.Errorf("table row should be preserved, got len %d want %d", len(got), len(row))
	}
	sep := "| " + strings.Repeat("---|", 60)
	if got := wechatWrapCopyFriendlyLines(sep); got != sep {
		t.Errorf("table separator should be preserved, got len %d want %d", len(got), len(sep))
	}
	// A bare long token (URL / run of CJK with no spaces) is kept whole.
	url := "https://example.com/" + strings.Repeat("path/", 60)
	if got := wechatWrapCopyFriendlyLines(url); got != url {
		t.Errorf("bare long token should be preserved, got %q", got)
	}
}
