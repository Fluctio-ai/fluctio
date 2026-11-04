package diary

import (
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

func cstDay(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", s, CST)
	if err != nil {
		panic(err)
	}
	return t
}

// TestAssembleShelvedLedger covers the candidate-sourced ledger: only
// deferred rows surface, the open window drops old and future rows, and
// resolved rows trail as the closed loop.
func TestAssembleShelvedLedger(t *testing.T) {
	day := cstDay("2026-11-03")
	open := []store.ShelvedCandidate{
		{Kind: "deferred", Topic: "新鲜", Since: "2026-11-02", SessionKey: "s-1", Seq: 11},
		{Kind: "deferred", Topic: "边界-正好7天", Since: "2026-10-27", SessionKey: "s-1", Seq: 12},
		{Kind: "deferred", Topic: "过期-8天", Since: "2026-10-26", SessionKey: "s-1", Seq: 13},
		{Kind: "deferred", Topic: "未来日期", Since: "2026-11-04", SessionKey: "s-1", Seq: 14},
		{Kind: "commitment", Topic: "承诺类先不展示", Since: "2026-11-02", SessionKey: "s-1", Seq: 15},
	}
	resolved := []store.ShelvedCandidate{
		{Kind: "deferred", Topic: "捡起了", Since: "2026-10-30", SessionKey: "s-2", Seq: 7},
		{Kind: "awaiting_user", Topic: "等回答不展示", Since: "2026-11-01", SessionKey: "s-2", Seq: 8},
	}
	got := assembleShelvedLedger(open, resolved, day)
	if len(got) != 3 {
		t.Fatalf("ledger len = %d, want 3: %+v", len(got), got)
	}
	if got[0].Topic != "新鲜" || got[0].Status != "open" || got[0].Session != "s-1" || got[0].Seq != 11 {
		t.Errorf("first entry wrong: %+v", got[0])
	}
	if got[1].Topic != "边界-正好7天" || got[1].Status != "open" {
		t.Errorf("window boundary wrong: %+v", got[1])
	}
	if got[2].Topic != "捡起了" || got[2].Status != "resolved" {
		t.Errorf("resolved must trail: %+v", got[2])
	}
}
