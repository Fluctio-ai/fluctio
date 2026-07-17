package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

func TestConversationGapContextInjectsTimingNote(t *testing.T) {
	old := time.Date(2026, 5, 21, 15, 9, 0, 0, time.UTC).UnixMilli()
	now := time.Date(2026, 6, 21, 15, 9, 0, 0, time.UTC).UnixMilli()
	got := withConversationGapContext([]provider.Message{
		{Role: "assistant", Content: "旧回复", Timestamp: old},
		{Role: "user", Content: "新问题", Timestamp: now},
	})

	if len(got) != 3 || got[0].Role != "system" {
		t.Fatalf("messages = %#v, want timing context plus unchanged history", got)
	}
	if got[2].Content != "新问题" {
		t.Fatalf("latest content = %q, want no timestamp prefix", got[2].Content)
	}
	if !strings.Contains(got[0].Content, "about 31 days") || !strings.Contains(got[0].Content, "do not repeat") {
		t.Fatalf("gap context = %q", got[0].Content)
	}
}

func TestConversationGapContextSkipsRecentTurns(t *testing.T) {
	now := time.Now().UnixMilli()
	msgs := []provider.Message{
		{Role: "assistant", Content: "刚才的回复", Timestamp: now - time.Hour.Milliseconds()},
		{Role: "user", Content: "继续", Timestamp: now},
	}
	got := withConversationGapContext(msgs)
	if len(got) != len(msgs) || got[1].Content != "继续" {
		t.Fatalf("recent messages changed: %#v", got)
	}
}

func TestConversationGapContextSkipsCompactionNotice(t *testing.T) {
	old := time.Date(2026, 5, 21, 15, 9, 0, 0, time.UTC).UnixMilli()
	now := time.Date(2026, 6, 21, 15, 9, 0, 0, time.UTC).UnixMilli()
	normal := provider.Message{Role: "user", Content: "hello", Timestamp: old}
	notice := provider.Message{
		Role:      "assistant",
		Content:   "📝 上下文已自动压缩…",
		Timestamp: old + 1000,
		Metadata:  map[string]any{"compactionNotice": map[string]any{"tokensBefore": 1000}},
	}
	after := provider.Message{Role: "user", Content: "next turn", Timestamp: now}

	got := withConversationGapContext([]provider.Message{normal, notice, after})

	for _, m := range got {
		if _, ok := m.Metadata["compactionNotice"]; ok {
			t.Fatalf("compaction notice leaked into LLM-bound output: %+v", m)
		}
	}
	// Expect [system note, normal, after] = 3 (notice filtered, gap > 24h).
	if len(got) != 3 || got[0].Role != "system" {
		t.Fatalf("messages = %#v, want system note + 2 messages (notice filtered)", got)
	}
	if got[1].Content != "hello" || got[2].Content != "next turn" {
		t.Fatalf("order/content wrong: got %+v", got)
	}
}

func TestRuntimeContextUsesChatterTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	cb := NewContextBuilder("", nil, "")
	cb.userID = ownerUID
	cb.SetTimezoneResolver(func(uid string) *time.Location {
		if uid == chatterUID {
			return loc
		}
		return time.UTC
	})

	got := cb.BuildRuntimeContextAs(chatterUID, "web", "chat-1")

	if !strings.Contains(got, "Timezone: Asia/Shanghai") {
		t.Fatalf("runtime context = %q, want chatter timezone", got)
	}
}
