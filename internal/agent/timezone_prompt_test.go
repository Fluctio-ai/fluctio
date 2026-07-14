package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

func TestWithMessageTimestampsUsesExplicitChatterTimezone(t *testing.T) {
	store := newFakeMemoryStore()
	store.put(testAgentID, chatterUID, "USER.md", "# Current Chatter\n- Timezone: Asia/Shanghai")

	a := &Agent{
		memory:  NewMemoryWithStoreForUser("", store, ownerUID, testAgentID),
		agentID: testAgentID,
	}
	ts := time.Date(2026, 6, 21, 15, 9, 0, 0, time.UTC).UnixMilli()

	got := a.withMessageTimestampsForChatter([]provider.Message{{
		Role:      "user",
		Content:   "为什么是下午好",
		Timestamp: ts,
	}}, chatterUID)

	if len(got) != 1 {
		t.Fatalf("message count = %d, want 1", len(got))
	}
	if !strings.HasPrefix(got[0].Content, "[2026-06-21 23:09 Sun] ") {
		t.Fatalf("timestamp prefix = %q, want Asia/Shanghai local time", got[0].Content)
	}
}

func TestWithMessageTimestampsSkipsCompactionNotice(t *testing.T) {
	store := newFakeMemoryStore()
	a := &Agent{
		memory:  NewMemoryWithStoreForUser("", store, ownerUID, testAgentID),
		agentID: testAgentID,
	}

	normal := provider.Message{Role: "user", Content: "hello", Timestamp: 0}
	notice := provider.Message{
		Role:      "assistant",
		Content:   "📝 上下文已自动压缩…",
		Timestamp: time.Now().UnixMilli(),
		Metadata:  map[string]any{"compactionNotice": map[string]any{"tokensBefore": 1000}},
	}
	after := provider.Message{Role: "user", Content: "next turn"}

	got := a.withMessageTimestampsForChatter([]provider.Message{normal, notice, after}, chatterUID)

	if len(got) != 2 {
		t.Fatalf("message count = %d, want 2 (notice filtered)", len(got))
	}
	for _, m := range got {
		if _, ok := m.Metadata["compactionNotice"]; ok {
			t.Fatalf("compaction notice leaked into LLM-bound output: %+v", m)
		}
	}
	if got[0].Content != "hello" || got[1].Content != "next turn" {
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
