package gateway

import (
	"fmt"
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
			Content:   fmt.Sprintf("%s第%d次·%s", failureRetryTag, attempt, base),
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

// TestAnalyzeFailureChainMetadataRetry covers the metadata-marked replay:
// the loop stamps failureRetry metadata on SourceFailureRetry turns (see
// agent.markFailureRetryTurn), so the turn's TEXT no longer has to carry
// the Chinese prefix — a chain of clean-text replay turns must count.
// The legacy prefix shape stays covered by TestAnalyzeFailureChain.
func TestAnalyzeFailureChainMetadataRetry(t *testing.T) {
	notice := func(ts int64) provider.Message {
		return provider.Message{
			Role:    "user",
			Origin:  provider.OriginTurnError,
			Content: "⚠️ failed",
			Metadata: map[string]any{
				"turnError":     "quota_exhausted",
				"toolsExecuted": 0,
			},
			Timestamp: ts,
		}
	}
	metaRetryTurn := func(text string, ts int64) provider.Message {
		return provider.Message{
			Role:      "user",
			Content:   text,
			Metadata:  map[string]any{"failureRetry": true},
			Timestamp: ts,
		}
	}

	t.Run("clean-text replay counts via metadata", func(t *testing.T) {
		msgs := []provider.Message{
			{Role: "user", Content: "帮我找视频", Timestamp: 1},
			notice(2),
			metaRetryTurn("第1次·帮我找视频", 3),
			notice(4),
		}
		chain, ok := analyzeFailureChain(msgs)
		if !ok {
			t.Fatalf("expected chain")
		}
		if chain.retries != 1 {
			t.Errorf("retries = %d, want 1 (metadata-marked, no text prefix)", chain.retries)
		}
		if chain.userText != "帮我找视频" {
			t.Errorf("userText = %q", chain.userText)
		}
	})

	t.Run("mixed metadata and legacy-prefix links both count", func(t *testing.T) {
		msgs := []provider.Message{
			{Role: "user", Content: "跑构建", Timestamp: 1},
			notice(2),
			{Role: "user", Content: failureRetryTag + "第1次·跑构建", Timestamp: 3}, // legacy
			notice(4),
			metaRetryTurn("第2次·跑构建", 5),
			notice(6),
		}
		chain, ok := analyzeFailureChain(msgs)
		if !ok {
			t.Fatalf("expected chain")
		}
		if chain.retries != 2 {
			t.Errorf("retries = %d, want 2 (one legacy + one metadata)", chain.retries)
		}
	})
}

// TestPostReplayScanState pins the HIGH-1 fix: after a replay the scan
// state must keep the session PENDING (nextAt set) whenever the chain is
// still unresolved — a replay that dies at the quota breaker writes no
// llm_call_diag row at all, so waiting for a newer row would stall the
// ladder forever. A resolved chain settles.
func TestPostReplayScanState(t *testing.T) {
	notice := func(ts int64) provider.Message {
		return provider.Message{
			Role:    "user",
			Origin:  provider.OriginTurnError,
			Content: "⚠️ failed",
			Metadata: map[string]any{
				"turnError":     "quota_exhausted",
				"toolsExecuted": 0,
			},
			Timestamp: ts,
		}
	}
	failed := []provider.Message{
		{Role: "user", Content: "x", Timestamp: 1},
		notice(2),
		{Role: "user", Content: failureRetryTag + "第1次·x", Metadata: map[string]any{"failureRetry": true}, Timestamp: 3},
		notice(4),
	}

	st := postReplayScanState(failed, 42)
	if st.handledUpTo != 42 || st.nextAt.IsZero() {
		t.Errorf("unresolved chain must stay pending, got %+v", st)
	}

	resolved := append(append([]provider.Message{}, failed...),
		provider.Message{Role: "assistant", Content: "done", Timestamp: 5})
	st = postReplayScanState(resolved, 42)
	if st.handledUpTo != 42 || !st.nextAt.IsZero() {
		t.Errorf("resolved chain must settle, got %+v", st)
	}
}

// TestFailureScanWindowFor pins the window derivation: the 6h baseline
// holds at the default knobs, hot-tuned knobs that push the longest
// backoff past it stretch the window (or the due retry slides out of
// the seed query and the ladder stalls), and an absurd maxAttempts
// can't overflow the shift.
func TestFailureScanWindowFor(t *testing.T) {
	cases := []struct {
		name string
		base time.Duration
		max  int
		want time.Duration
	}{
		{"defaults hold 6h", 15 * time.Minute, 3, 6 * time.Hour},
		{"90min base stretches", 90 * time.Minute, 3, 90*time.Minute<<2 + 10*time.Minute},
		{"shift capped at 20", time.Hour, 1000, time.Hour<<20 + 10*time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := failureScanWindowFor(c.base, c.max); got != c.want {
				t.Errorf("failureScanWindowFor(%v, %d) = %v, want %v", c.base, c.max, got, c.want)
			}
		})
	}
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

// TestShouldScan pins the per-session cache semantics — the whole point of
// the cache is that a settled session costs no session load per tick, and
// a pending retry isn't woken before its backoff elapses.
func TestShouldScan(t *testing.T) {
	now := time.Now()
	st := func(upTo int64, nextAt time.Time) failureScanState {
		return failureScanState{handledUpTo: upTo, nextAt: nextAt}
	}
	cases := []struct {
		name     string
		state    failureScanState
		newestID int64
		want     bool
		reason   string
	}{
		{"never analyzed", st(0, time.Time{}), 7, true, "first sighting"},
		{"settled, same newest row", st(7, time.Time{}), 7, false, "already handled, nothing new"},
		{"settled, older row", st(9, time.Time{}), 7, false, "already handled a newer one"},
		{"settled, newer failure arrived", st(7, time.Time{}), 8, true, "new failure row"},
		{"pending, before nextAt", st(7, now.Add(time.Minute)), 7, false, "backoff not elapsed"},
		{"pending, after nextAt", st(7, now.Add(-time.Minute)), 7, true, "retry due"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldScan(c.state, c.newestID, now); got != c.want {
				t.Errorf("shouldScan = %v, want %v (%s)", got, c.want, c.reason)
			}
		})
	}
}
