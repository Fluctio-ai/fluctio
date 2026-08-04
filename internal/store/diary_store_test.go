package store

import (
	"context"
	"testing"
)

// TestDailyDiaryRoundTrip covers the migrate (table created via
// openTestDB's Migrate), Insert upsert semantics, Get, List ordering,
// and per-agent isolation. This is the data-layer smoke for the diary
// generator — the LLM-distillation path above it is pure prompt logic.
func TestDailyDiaryRoundTrip(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Absent → (nil, nil).
	if got, err := db.GetDailyDiary(ctx, "agent-1", "2026-08-03"); err != nil {
		t.Fatalf("get absent: %v", err)
	} else if got != nil {
		t.Fatalf("absent returned non-nil")
	}

	d1 := DailyDiary{
		AgentID:  "agent-1",
		Date:     "2026-08-03",
		Overview: "聊了日记功能 + seq 锚点",
		Themes: []DiaryTheme{{
			Title:   "每日日记",
			Summary: "基于 summary 生成",
			Points:  []string{"主题聚合", "盲区补充"},
			Segments: []DiarySegRef{
				{Session: "sess-1", Start: 12, End: 18},
				{Session: "sess-1", Start: 40, End: 40},
			},
		}},
		Blindspots: []DiaryBlindspot{{Point: "seq 锚点", Reason: "避免 LLM 编造行号"}},
		Archives:   []string{},
		Model:      "test/model",
	}
	if err := db.InsertDailyDiary(ctx, d1); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := db.GetDailyDiary(ctx, "agent-1", "2026-08-03")
	if err != nil {
		t.Fatalf("get present: %v", err)
	}
	if got == nil {
		t.Fatalf("present returned nil")
	}
	if got.Overview != d1.Overview {
		t.Errorf("overview = %q, want %q", got.Overview, d1.Overview)
	}
	if len(got.Themes) != 1 || got.Themes[0].Title != "每日日记" {
		t.Errorf("theme round-trip mismatch: %+v", got.Themes)
	}
	if len(got.Themes[0].Segments) != 2 || got.Themes[0].Segments[0].Start != 12 {
		t.Errorf("segments mismatch: %+v", got.Themes[0].Segments)
	}
	if len(got.Blindspots) != 1 || got.Blindspots[0].Point != "seq 锚点" {
		t.Errorf("blindspot mismatch: %+v", got.Blindspots)
	}

	// Upsert (same agent+date) replaces mutable content.
	d1.Overview = "更新后的概要"
	d1.Themes = []DiaryTheme{{Title: "新主题", Summary: "x"}}
	if err := db.InsertDailyDiary(ctx, d1); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ = db.GetDailyDiary(ctx, "agent-1", "2026-08-03")
	if got.Overview != "更新后的概要" || len(got.Themes) != 1 || got.Themes[0].Title != "新主题" {
		t.Errorf("upsert mismatch: %+v", got)
	}

	// A second day + List (newest first).
	if err := db.InsertDailyDiary(ctx, DailyDiary{AgentID: "agent-1", Date: "2026-08-02", Overview: "前一天"}); err != nil {
		t.Fatalf("insert d2: %v", err)
	}
	list, err := db.ListDailyDiaries(ctx, "agent-1", "2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
	if list[0].Date != "2026-08-03" || list[1].Date != "2026-08-02" {
		t.Errorf("list order wrong: %s then %s", list[0].Date, list[1].Date)
	}

	// Per-agent isolation.
	if g, _ := db.GetDailyDiary(ctx, "agent-2", "2026-08-03"); g != nil {
		t.Errorf("agent-2 leaked into agent-1's diary")
	}
}
