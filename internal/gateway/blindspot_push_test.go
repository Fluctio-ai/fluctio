package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// TestBlindspotPushStampAndNudge covers the once-per-diary stamp
// round-trip (keyed on the diary's date, not the push day) and the nudge
// body: count header, point — reason pairing, cap, no link without a
// public base URL.
func TestBlindspotPushStampAndNudge(t *testing.T) {
	dbs, err := store.NewDBStore("sqlite", "file::memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer dbs.Close()
	if err := dbs.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	diaryDate := time.Now().In(diaryCST).AddDate(0, 0, -1).Format("2006-01-02")

	if dbs.HasDailyRun(ctx, "diary_blindspot_push_runs", "a1", diaryDate) {
		t.Fatalf("nothing pushed yet but flagged pushed")
	}
	if err := dbs.StampBlindspotPushRun(ctx, "a1", diaryDate, 2, "telegram"); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if !dbs.HasDailyRun(ctx, "diary_blindspot_push_runs", "a1", diaryDate) {
		t.Fatalf("stamp not readable back")
	}
	if dbs.HasDailyRun(ctx, "diary_blindspot_push_runs", "a2", diaryDate) {
		t.Fatalf("stamp leaked across agents")
	}
	if dbs.HasDailyRun(ctx, "diary_blindspot_push_runs", "a1", "2020-01-01") {
		t.Fatalf("stamp leaked across dates")
	}

	blinds := []store.DiaryBlindspot{
		{Point: "想重构推送架构", Reason: "说周五定方案，一周没回头"},
		{Point: "体检报告没看", Reason: ""},
	}
	msg := formatBlindspotNudge("agt_x", blinds)
	if !strings.Contains(msg, "可能漏了的事（2 条）") {
		t.Fatalf("nudge missing count header: %q", msg)
	}
	if !strings.Contains(msg, "· 想重构推送架构 — 说周五定方案，一周没回头") {
		t.Fatalf("nudge missing point-reason pairing: %q", msg)
	}
	if !strings.Contains(msg, "· 体检报告没看") || strings.Contains(msg, "体检报告没看 —") {
		t.Fatalf("empty reason must not leave a dangling dash: %q", msg)
	}
	if strings.Contains(msg, "/knowledge/diary") {
		t.Fatalf("no public base URL configured — link should be omitted: %q", msg)
	}

	// Cap: six blindspots list at most four.
	var many []store.DiaryBlindspot
	for i := 0; i < 6; i++ {
		many = append(many, store.DiaryBlindspot{Point: "P" + string(rune('0'+i))})
	}
	msg2 := formatBlindspotNudge("agt_x", many)
	if !strings.Contains(msg2, "P3") || strings.Contains(msg2, "P4") {
		t.Fatalf("cap 4 not enforced: %q", msg2)
	}
}
