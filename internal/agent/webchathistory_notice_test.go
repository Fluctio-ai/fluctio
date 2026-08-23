package agent

import (
	"testing"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// WebChatHistory must surface P3-T1 persisted compaction notice
// messages as kind="compaction_notice" entries (so the frontend can
// render a notice bubble on history reload / page refresh) — and must
// keep doing normal rendering for assistant turns that lack the
// compactionNotice metadata key.
func TestWebChatHistoryCompactionNotice(t *testing.T) {
	a := newSlashTestAgent(t)
	sess := a.sessions.Get("web", "", "notice-chat", "")
	// In file-backed mode (store=nil), resolveOrMintKey returns chatID
	// directly for web channel — so that's the session_key WebChatHistory
	// must look up.
	const sessionKey = "notice-chat"

	// 1) regular user turn
	sess.Append(provider.Message{Role: "user", Content: "hello"})
	// 2) regular assistant reply (no compactionNotice → normal entry)
	sess.Append(provider.Message{
		Role:      "assistant",
		Content:   "hi there",
		Timestamp: 1000,
	})
	// 3) compaction notice persisted by P3-T1
	sess.Append(provider.Message{
		Role:    "assistant",
		Content: "📝 上下文已自动压缩（5.0万 → 2.0万 tokens，保留最近 6 轮）",
		Metadata: map[string]any{
			"compactionNotice": map[string]any{
				"before":         50000,
				"after":          20000,
				"retained_turns": 6,
			},
		},
		Timestamp: 2000,
	})

	hist, _, _ := a.WebChatHistory(sessionKey, 0, 50)

	// Expect 3 entries: user, assistant, compaction_notice.
	if len(hist) != 3 {
		t.Fatalf("expected 3 history entries, got %d: %+v", len(hist), hist)
	}

	// Entry 0: user
	if hist[0]["role"] != "user" || hist[0]["content"] != "hello" {
		t.Errorf("entry 0 wrong: %+v", hist[0])
	}

	// Entry 1: normal assistant — must NOT have kind=compaction_notice
	if hist[1]["role"] != "assistant" || hist[1]["content"] != "hi there" {
		t.Errorf("entry 1 wrong: %+v", hist[1])
	}
	if k, ok := hist[1]["kind"]; ok {
		t.Errorf("normal assistant entry should not have kind, got %q", k)
	}

	// Entry 2: compaction notice
	notice := hist[2]
	if notice["role"] != "assistant" {
		t.Errorf("notice role = %q, want assistant", notice["role"])
	}
	if notice["kind"] != "compaction_notice" {
		t.Errorf("notice kind = %q, want compaction_notice", notice["kind"])
	}
	if notice["content"] == "" {
		t.Errorf("notice content should be the notice text, got empty")
	}
	// before / after / retained_turns hoisted from metadata
	if notice["before"] != 50000 {
		t.Errorf("notice before = %v, want 50000", notice["before"])
	}
	if notice["after"] != 20000 {
		t.Errorf("notice after = %v, want 20000", notice["after"])
	}
	if notice["retained_turns"] != 6 {
		t.Errorf("notice retained_turns = %v, want 6", notice["retained_turns"])
	}
	if notice["timestamp"] != int64(2000) {
		t.Errorf("notice timestamp = %v, want 2000", notice["timestamp"])
	}
}

// WebChatHistory must surface turn-abort boundary markers (Origin=
// turn_abort, written by the crash heal / stopped-turn paths) as
// kind="turn_aborted_notice" entries — BEFORE the Origin!=OriginUser
// filter that hides runtime-injected messages, because unlike goal
// scaffolding these boundaries are user-visible history facts.
func TestWebChatHistoryTurnAbortedNotice(t *testing.T) {
	a := newSlashTestAgent(t)
	sess := a.sessions.Get("web", "", "abort-chat", "")
	const sessionKey = "abort-chat"

	sess.Append(provider.Message{Role: "user", Content: "run the build"})
	sess.Append(provider.Message{
		Role:      "user",
		Content:   "[system] 上一轮执行被中断(服务重启):被中止的工具可能已部分执行。",
		Origin:    provider.OriginTurnAbort,
		Timestamp: 3000,
	})

	hist, _, _ := a.WebChatHistory(sessionKey, 0, 50)
	if len(hist) != 2 {
		t.Fatalf("expected 2 history entries, got %d: %+v", len(hist), hist)
	}
	if hist[0]["role"] != "user" || hist[0]["content"] != "run the build" {
		t.Errorf("entry 0 wrong: %+v", hist[0])
	}
	marker := hist[1]
	if marker["kind"] != "turn_aborted_notice" {
		t.Errorf("marker kind = %v, want turn_aborted_notice", marker["kind"])
	}
	if marker["role"] != "user" {
		t.Errorf("marker role = %v, want user", marker["role"])
	}
	if marker["timestamp"] != int64(3000) {
		t.Errorf("marker timestamp = %v, want 3000", marker["timestamp"])
	}
}
