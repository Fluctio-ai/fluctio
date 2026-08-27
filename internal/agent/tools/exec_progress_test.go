package tools

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestProgressNoticeContextRoundtrip covers the ctx carrier: attached
// notifier comes back out, bare ctx yields nil.
func TestProgressNoticeContextRoundtrip(t *testing.T) {
	if got := ProgressNoticeFromContext(context.Background()); got != nil {
		t.Fatalf("bare ctx should yield nil notifier, got %v", got)
	}
	fn := ProgressNoticeFunc(func(n TurnProgressNotice) {})
	if got := ProgressNoticeFromContext(WithProgressNotice(context.Background(), fn)); got == nil {
		t.Fatal("attached notifier should come back out")
	}
}

// TestArmExecProgressNotice covers the arming gates and the fire path:
// no notifier → no-op stop; timeout ≤ threshold → no timer; timeout beyond
// threshold and still running → fires once with kind/elapsed/command;
// stop() before the deadline → never fires.
func TestArmExecProgressNotice(t *testing.T) {
	orig := execProgressNoticeAfter
	t.Cleanup(func() { execProgressNoticeAfter = orig })
	execProgressNoticeAfter = 10 * time.Millisecond

	t.Run("fires past threshold", func(t *testing.T) {
		got := make(chan TurnProgressNotice, 1)
		ctx := WithProgressNotice(context.Background(), func(n TurnProgressNotice) { got <- n })
		stop := armExecProgressNotice(ctx, "ffmpeg -i in.mp4 out.mp4", 50*time.Millisecond)
		defer stop()
		select {
		case n := <-got:
			if n.Kind != "long_exec" || n.Elapsed != 10*time.Millisecond || n.Command != "ffmpeg -i in.mp4 out.mp4" {
				t.Errorf("unexpected notice payload: %+v", n)
			}
		case <-time.After(time.Second):
			t.Fatal("notice should fire after the threshold while exec still runs")
		}
	})

	t.Run("stop disarms", func(t *testing.T) {
		fired := false
		ctx := WithProgressNotice(context.Background(), func(TurnProgressNotice) { fired = true })
		stop := armExecProgressNotice(ctx, "sleep 60", 50*time.Millisecond)
		stop()
		time.Sleep(30 * time.Millisecond)
		if fired {
			t.Error("stopped notice must not fire")
		}
	})

	t.Run("short timeout never arms", func(t *testing.T) {
		fired := false
		ctx := WithProgressNotice(context.Background(), func(TurnProgressNotice) { fired = true })
		stop := armExecProgressNotice(ctx, "echo hi", 5*time.Millisecond) // ≤ threshold
		stop()
		time.Sleep(30 * time.Millisecond)
		if fired {
			t.Error("timeout at/below the threshold must never fire a notice")
		}
	})

	t.Run("no notifier no-op", func(t *testing.T) {
		stop := armExecProgressNotice(context.Background(), "sleep 60", 50*time.Millisecond)
		stop() // must not panic
	})
}

// TestCommandPreview pins the preview rendering: first line only, capped
// at 80 runes + ellipsis.
func TestCommandPreview(t *testing.T) {
	if got := commandPreview("  ffmpeg -i a b\ncurl example.com  "); got != "ffmpeg -i a b" {
		t.Errorf("multiline preview = %q, want first line only", got)
	}
	long := strings.Repeat("x", 120)
	got := commandPreview(long)
	if utf8.RuneCountInString(got) != 81 || !strings.HasSuffix(got, "…") {
		t.Errorf("long preview should cap at 80 runes+ellipsis, got %d runes", utf8.RuneCountInString(got))
	}
	if got := commandPreview("short"); got != "short" {
		t.Errorf("short preview = %q", got)
	}
	// CJK safety: a byte-level cut would split a rune and emit invalid
	// UTF-8 into the IM bubble. The rune count cap keeps it well-formed.
	cjk := strings.Repeat("转换", 100)
	if got := commandPreview(cjk); !utf8.ValidString(got) {
		t.Error("CJK preview must stay valid UTF-8 after truncation")
	}
}
