package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// TestCardsPushStampAndDigest covers the once-a-day stamp round-trip and
// the digest body: count, first-three teaser, no link without a public
// base URL. The full push cycle needs a live channel manager — e2e
// territory (M6), not unit-testable here.
func TestCardsPushStampAndDigest(t *testing.T) {
	dbs, err := store.NewDBStore("sqlite", "file::memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer dbs.Close()
	if err := dbs.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	today := time.Now().In(diaryCST).Format("2006-01-02")

	if cardsPushedToday(ctx, dbs, "a1", today) {
		t.Fatalf("nothing pushed yet but flagged pushed")
	}
	if err := stampCardsPush(ctx, dbs, "a1", today, 5, "telegram"); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if !cardsPushedToday(ctx, dbs, "a1", today) {
		t.Fatalf("stamp not readable back")
	}
	if cardsPushedToday(ctx, dbs, "a2", today) {
		t.Fatalf("stamp leaked across agents")
	}
	if cardsPushedToday(ctx, dbs, "a1", "2020-01-01") {
		t.Fatalf("stamp leaked across dates")
	}

	due := []kb.KBCard{
		{Question: "Q1？"}, {Question: "Q2？"}, {Question: "Q3？"}, {Question: "Q4？"},
	}
	msg := formatCardsDigest("agt_x", due, kb.KBCardStats{StreakDays: 3})
	if !strings.Contains(msg, "今日卡片组（4 张 · 连续 3 天）") {
		t.Fatalf("digest missing group header: %q", msg)
	}
	if !strings.Contains(msg, "1. Q1？") || !strings.Contains(msg, "4. Q4？") {
		t.Fatalf("digest missing numbered questions (cap 12 > 4, all listed): %q", msg)
	}
	if strings.Contains(msg, "/knowledge/cards") {
		t.Fatalf("no public base URL configured — link should be omitted: %q", msg)
	}

	// Repeat marks and carry-over line.
	due2 := []kb.KBCard{
		{Question: "旧卡", ReviewCount: 2},
		{Question: "新卡"},
	}
	due2[0].DueAt = ptrTime(time.Now().In(time.FixedZone("CST", 8*3600)).AddDate(0, 0, -3))
	msg2 := formatCardsDigest("agt_x", due2, kb.KBCardStats{})
	if !strings.Contains(msg2, "旧卡 🔁") {
		t.Fatalf("digest missing repeat mark: %q", msg2)
	}
	if !strings.Contains(msg2, "来自前几日未完成") {
		t.Fatalf("digest missing carry-over line: %q", msg2)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// TestArchivePushNoticeHidden covers the reghook-style web-visibility write
// shared by both push sweeps (cards digest, todo reminder): the notice lands
// in the session archive as an assistant row the web history renders (origin
// empty, content set) but llm_visible=0 keeps it out of the LLM working set /
// summary / recall.
func TestArchivePushNoticeHidden(t *testing.T) {
	dbs, err := store.NewDBStore("sqlite", "file::memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer dbs.Close()
	if err := dbs.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	digest := "🧠 今日卡片组（2 张）\n1. Q1？\n2. Q2？"
	if err := archivePushNotice(ctx, dbs, "a1", "s-key", digest, "cards_digest"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := archivePushNotice(ctx, dbs, "a1", "s-key", "⏰ 待办提醒：交报告", "todo_reminder"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	msgs, err := dbs.ListSessionMessages(ctx, "a1", "s-key")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 archived rows, got %d", len(msgs))
	}
	for i, want := range []string{"cards_digest", "todo_reminder"} {
		m := msgs[i]
		if m.Role != "assistant" {
			t.Fatalf("row %d role=%q, want assistant", i, m.Role)
		}
		if m.LLMVisible {
			t.Fatalf("row %d must archive hidden (llm_visible=0)", i)
		}
		if m.Origin != "" {
			t.Fatalf("origin must stay empty — WebChatHistory hides non-OriginUser rows, got %q", m.Origin)
		}
		if v, _ := m.Metadata["pushNotice"].(string); v != want {
			t.Fatalf("row %d metadata marker=%v, want %q", i, m.Metadata["pushNotice"], want)
		}
	}
	if msgs[0].Content != digest || msgs[1].Content != "⏰ 待办提醒：交报告" {
		t.Fatalf("contents mismatch: %q / %q", msgs[0].Content, msgs[1].Content)
	}
}
