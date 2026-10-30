package gateway

import (
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// TestAnalyzeFailureChain walks every tail shape the sweep can meet:
// fresh failure, retry-spent chain, resolved chain (assistant tail — the
// user re-sent or a replay succeeded), in-flight turn, mid-tool failure,
// and degenerate tails.
func TestAnalyzeFailureChain(t *testing.T) {
	notice := func(cat string, tools int, ts int64) provider.Message {
		return provider.Message{
			Role:    "user",
			Origin:  provider.OriginTurnError,
			Content: "⚠️ failed",
			Metadata: map[string]any{
				"turnError":     cat,
				"toolsExecuted": tools,
			},
			Timestamp: ts,
		}
	}
	retryTurn := func(attempt int, base string, ts int64) provider.Message {
		return provider.Message{
			Role:      "user",
			Content:   failureRetryTag + "第" + string(rune('0'+attempt)) + "次·" + base,
			Timestamp: ts,
		}
	}

	t.Run("fresh failure", func(t *testing.T) {
		msgs := []provider.Message{
			{Role: "user", Content: "帮我找视频", Timestamp: 1},
			notice("quota_exhausted", 0, 2000),
		}
		chain, ok := analyzeFailureChain(msgs)
		if !ok {
			t.Fatalf("expected chain")
		}
		if chain.userText != "帮我找视频" || chain.retries != 0 || !chain.replayable {
			t.Errorf("chain = %+v", chain)
		}
		if chain.category != "quota_exhausted" {
			t.Errorf("category = %q", chain.category)
		}
		if chain.lastAt.UnixMilli() != 2000 {
			t.Errorf("lastAt = %v", chain.lastAt)
		}
	})

	t.Run("retry spent chain", func(t *testing.T) {
		msgs := []provider.Message{
			{Role: "user", Content: "帮我找视频", Timestamp: 1},
			notice("quota_exhausted", 0, 2),
			retryTurn(1, "帮我找视频", 3),
			notice("quota_exhausted", 0, 4000),
		}
		chain, ok := analyzeFailureChain(msgs)
		if !ok {
			t.Fatalf("expected chain")
		}
		if chain.retries != 1 {
			t.Errorf("retries = %d, want 1", chain.retries)
		}
		if chain.lastAt.UnixMilli() != 4000 {
			t.Errorf("lastAt = %v, want newest notice ts", chain.lastAt)
		}
	})

	t.Run("resolved tail (assistant reply)", func(t *testing.T) {
		msgs := []provider.Message{
			{Role: "user", Content: "帮我找视频"},
			notice("retryable", 0, 2),
			{Role: "assistant", Content: "链接是…"},
		}
		if _, ok := analyzeFailureChain(msgs); ok {
			t.Fatalf("answered tail must not be a chain")
		}
	})

	t.Run("in-flight turn tail (user unanswered)", func(t *testing.T) {
		msgs := []provider.Message{
			{Role: "user", Content: "帮我找视频"},
			notice("retryable", 0, 2),
			{Role: "user", Content: "我重发一次"}, // user handled it — chain broken
		}
		if _, ok := analyzeFailureChain(msgs); ok {
			t.Fatalf("user turn after notice must break the chain")
		}
	})

	t.Run("mid-tool failure is not replayable", func(t *testing.T) {
		msgs := []provider.Message{
			{Role: "user", Content: "跑构建"},
			notice("retryable", 3, 2),
		}
		chain, ok := analyzeFailureChain(msgs)
		if !ok {
			t.Fatalf("expected chain")
		}
		if chain.replayable {
			t.Fatalf("toolsExecuted>0 must not be replayable")
		}
	})

	t.Run("any mid-tool link poisons the chain", func(t *testing.T) {
		msgs := []provider.Message{
			{Role: "user", Content: "跑构建"},
			notice("retryable", 0, 2),
			retryTurn(1, "跑构建", 3),
			notice("retryable", 2, 4),
		}
		chain, ok := analyzeFailureChain(msgs)
		if !ok || chain.replayable {
			t.Fatalf("one toolsExecuted>0 link must poison: %+v ok=%v", chain, ok)
		}
		if chain.retries != 1 {
			t.Errorf("retries = %d, want 1", chain.retries)
		}
	})

	t.Run("degenerate tails", func(t *testing.T) {
		for _, msgs := range [][]provider.Message{
			{},
			{{Role: "user", Content: "hi"}},
			{notice("retryable", 0, 1)}, // notice with no user turn behind it
		} {
			if _, ok := analyzeFailureChain(msgs); ok {
				t.Fatalf("degenerate tail %+v must not be a chain", msgs)
			}
		}
	})

	t.Run("float64 metadata (JSON round-trip)", func(t *testing.T) {
		n := notice("retryable", 0, 1)
		n.Metadata["toolsExecuted"] = float64(4) // sessions-table JSON shape
		msgs := []provider.Message{{Role: "user", Content: "x"}, n}
		chain, ok := analyzeFailureChain(msgs)
		if !ok || chain.replayable {
			t.Fatalf("float64 toolsExecuted must parse: %+v ok=%v", chain, ok)
		}
	})
}

// TestFailureRetryBackoffSchedule pins the backoff arithmetic: first retry
// waits base, each further attempt doubles it. This is the schedule
// handleFailureSession gates on (chain.lastAt.Add(base << retries)).
func TestFailureRetryBackoffSchedule(t *testing.T) {
	base := 15 * time.Minute
	cases := []struct {
		retries int
		want    time.Duration
	}{
		{0, 15 * time.Minute},
		{1, 30 * time.Minute},
		{2, time.Hour},
	}
	for _, c := range cases {
		if got := base << c.retries; got != c.want {
			t.Errorf("base<<%d = %v, want %v", c.retries, got, c.want)
		}
	}
}
