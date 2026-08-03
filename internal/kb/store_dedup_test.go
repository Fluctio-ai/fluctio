package kb

import (
	"context"
	"testing"
)

// TestCheckDuplicateL1L3 walks the always-on dedup layers: L1 (same session +
// seq) blocks a rewrite even with a different body, L3 (normalized title)
// blocks a different-session write with the same title, and a genuinely
// distinct title is allowed through. L2 (vector) needs an embedder and is
// exercised by the vector-integration tests, not here.
func TestCheckDuplicateL1L3(t *testing.T) {
	db := setupKBVectorTestDB(t)
	store := NewKBStore(db, "sqlite")
	ctx := context.Background()
	const agent = "agt_dup"

	// Seed a flash captured from session s1 at seq 5.
	ctxA := WithSourceOrigin(ctx, SourceOrigin{SessionID: "s1", Seq: 5})
	id1, err := store.SaveFlash(ctxA, agent, "我的想法是关于X的笔记")
	if err != nil {
		t.Fatalf("SaveFlash: %v", err)
	}
	// Resolve the actual stored title (SaveFlash derives it from the first line).
	var seedTitle string
	srcs, _ := store.ListSources(ctx, agent, 50, 0)
	for _, s := range srcs {
		if s.ID == id1 {
			seedTitle = s.Title
		}
	}
	if seedTitle == "" {
		t.Fatalf("seed title not found for %s", id1)
	}

	// L1: same session s1 + seq 5 again → blocked even with different body.
	res := store.CheckDuplicate(ctxA, agent, "flash", "irrelevant title", "全新的不同内容正文", 0)
	if !res.Duplicate || res.Reason != "l1-origin" {
		t.Errorf("L1: got %+v, want l1-origin duplicate", res)
	}
	if res.SourceID != id1 {
		t.Errorf("L1 source: got %s want %s", res.SourceID, id1)
	}

	// L3: different session/seq but same normalized title → blocked.
	ctxB := WithSourceOrigin(ctx, SourceOrigin{SessionID: "s2", Seq: 1})
	res2 := store.CheckDuplicate(ctxB, agent, "flash", seedTitle, "完全不同的正文", 0)
	if !res2.Duplicate || res2.Reason != "l3-title" {
		t.Errorf("L3: got %+v, want l3-title duplicate", res2)
	}

	// Clean: distinct title → not blocked.
	res3 := store.CheckDuplicate(ctxB, agent, "flash", "完全不同的另一个独立标题", "body", 0)
	if res3.Duplicate {
		t.Errorf("distinct title: got unexpected block %+v", res3)
	}
}

// TestSeqRangesOverlap checks the L1 interval-overlap primitive.
func TestSeqRangesOverlap(t *testing.T) {
	cases := []struct {
		name string
		a, b []SeqRange
		want bool
	}{
		{"both nil", nil, nil, false},
		{"point inside range", []SeqRange{{1, 10}}, []SeqRange{{5, 5}}, true},
		{"point outside range", []SeqRange{{1, 10}}, []SeqRange{{15, 15}}, false},
		{"adjacent ranges touch", []SeqRange{{1, 10}}, []SeqRange{{10, 20}}, true},
		{"disjoint ranges", []SeqRange{{1, 5}}, []SeqRange{{10, 20}}, false},
		{"multi-range overlap", []SeqRange{{1, 5}, {18, 23}}, []SeqRange{{14, 14}, {20, 21}}, true},
	}
	for _, c := range cases {
		if got := SeqRangesOverlap(c.a, c.b); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// TestCheckDuplicateNoOrigin simulates the HTTP path (no SourceOrigin on ctx):
// SaveFlash seeds with no origin, then CheckDuplicate with the same title and
// no origin should still hit L3. Guards against a regression where only the
// loop (origin-bearing) path deduplicates.
func TestCheckDuplicateNoOrigin(t *testing.T) {
	db := setupKBVectorTestDB(t)
	store := NewKBStore(db, "sqlite")
	ctx := context.Background() // no SourceOrigin, like the HTTP handler
	const agent = "agt_noorigin"
	content := "smoke dedup v2 独特标记 pP8kX3mQ"
	id1, err := store.SaveFlash(ctx, agent, content)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	var stored string
	srcs, _ := store.ListSources(ctx, agent, 50, 0)
	for _, s := range srcs {
		if s.ID == id1 {
			stored = s.Title
		}
	}
	t.Logf("stored title=%q derive=%q", stored, DeriveTitle(content))
	t.Logf("norm stored=%q norm derive=%q", normalizeTitle(stored), normalizeTitle(DeriveTitle(content)))
	res := store.CheckDuplicate(ctx, agent, "flash", DeriveTitle(content), content, 0)
	t.Logf("result=%+v", res)
	if !res.Duplicate || res.Reason != "l3-title" {
		t.Errorf("got %+v, want l3-title duplicate", res)
	}
}

// differences and strips common CJK stop runes.
func TestNormalizeTitle(t *testing.T) {
	cases := []struct {
		a, b string
		want bool // whether normalize(a) == normalize(b)
	}{
		{"我的想法", "这想法", true},          // stop runes 我/的/这 stripped
		{"Hello World", "helloworld", true},   // spaces + case
		{"测试-标题!", "测试标题", true},        // punct stripped
		{"标题一", "标题二", false},            // distinct
		{"", "", true},                        // both empty
	}
	for _, c := range cases {
		na, nb := normalizeTitle(c.a), normalizeTitle(c.b)
		if (na == nb) != c.want {
			t.Errorf("normalize(%q)=%q vs normalize(%q)=%q: want match=%v",
				c.a, na, c.b, nb, c.want)
		}
	}
}
