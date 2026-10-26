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

// TestRollShelvedLedger covers the ledger-carry rules: resolved entries
// retire, open entries past the window are tacitly dropped, fresh open
// entries carry verbatim. A bad Since date is dropped rather than kept
// forever.
func TestRollShelvedLedger(t *testing.T) {
	today := cstDay("2026-10-26")
	prev := []store.DiaryShelved{
		{Topic: "新鲜", Since: "2026-10-25", Status: "open"},
		{Topic: "边界-正好7天", Since: "2026-10-19", Status: "open"},
		{Topic: "过期-8天", Since: "2026-10-18", Status: "open"},
		{Topic: "已捡起", Since: "2026-10-20", Status: "resolved"},
		{Topic: "坏日期", Since: "not-a-date", Status: "open"},
	}
	got := rollShelvedLedger(prev, today)
	if len(got) != 2 {
		t.Fatalf("rolled len = %d, want 2: %+v", len(got), got)
	}
	if got[0].Topic != "新鲜" || got[1].Topic != "边界-正好7天" {
		t.Errorf("kept wrong entries: %+v", got)
	}
	// Verbatim carry — the push sweep's change detection keys on topic text.
	if got[0].Since != "2026-10-25" {
		t.Errorf("since rewritten: %q", got[0].Since)
	}
	if got := rollShelvedLedger(nil, today); got != nil {
		t.Errorf("nil ledger rolled to %v, want nil", got)
	}
}

// TestShelvedLedgerChanged covers the push gate: only a fresh shelve
// (Since == the source diary's date) or a pickup counts as movement.
func TestShelvedLedgerChanged(t *testing.T) {
	date := "2026-10-25"
	cases := []struct {
		name    string
		entries []store.DiaryShelved
		want    bool
	}{
		{"empty", nil, false},
		{"carried only", []store.DiaryShelved{{Topic: "a", Since: "2026-10-20", Status: "open"}}, false},
		{"fresh shelve", []store.DiaryShelved{{Topic: "a", Since: date, Status: "open"}}, true},
		{"pickup", []store.DiaryShelved{{Topic: "a", Since: "2026-10-20", Status: "resolved"}}, true},
	}
	for _, c := range cases {
		if got := ShelvedLedgerChanged(c.entries, date); got != c.want {
			t.Errorf("%s: changed = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestAssembleShelved covers Pass-3 output assembly: carried entries keep
// fields verbatim, resolved trail as closed loop, new entries need a valid
// topic + transcript anchor, duplicates and out-of-range ids/idxs drop.
func TestAssembleShelved(t *testing.T) {
	ledger := []store.DiaryShelved{
		{Topic: "老话题A", Context: "ctx-a", Since: "2026-10-20", Session: "s1", Seq: 5, Status: "open"},
		{Topic: "老话题B", Context: "ctx-b", Since: "2026-10-21", Session: "s2", Seq: 9, Status: "open"},
	}
	refs := []idxRef{{Session: "s9", Seq: 42}}
	var out llmShelvedOut
	out.ResolvedIDs = []int{1, 7, -1} // 7/-1 out of range → ignored
	out.New = []llmShelvedNew{
		{Topic: "新搁浅", Context: "说到一半", SourceIdx: 0},
		{Topic: "老话题A", Context: "重复报", SourceIdx: 0}, // dup → dropped
		{Topic: "无锚点", Context: "", SourceIdx: 3},         // bad idx → dropped
		{Topic: "  ", Context: "", SourceIdx: 0},             // blank → dropped
	}
	got := assembleShelved(ledger, out, refs, "2026-10-26")
	if len(got) != 3 {
		t.Fatalf("assembled len = %d, want 3: %+v", len(got), got)
	}
	// Order: carried-open, new, resolved.
	if got[0].Topic != "老话题A" || got[0].Status != "open" || got[0].Seq != 5 {
		t.Errorf("carried entry mangled: %+v", got[0])
	}
	if got[1].Topic != "新搁浅" || got[1].Since != "2026-10-26" ||
		got[1].Session != "s9" || got[1].Seq != 42 || got[1].Status != "open" {
		t.Errorf("new entry wrong: %+v", got[1])
	}
	if got[2].Topic != "老话题B" || got[2].Status != "resolved" || got[2].Seq != 9 {
		t.Errorf("resolved entry wrong: %+v", got[2])
	}
}
