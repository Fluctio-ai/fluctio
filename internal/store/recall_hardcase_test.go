package store

import (
	"context"
	"testing"
	"time"
)

func TestMineHardcasesAggregatesZeroHitEvents(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	agent := "hc-mine-a"

	// Two zero-hit debug events for the SAME query (different lanes) and
	// one zero-hit for another query; a hit-bearing event must be ignored.
	must := func(ev RecallDebugEvent) {
		t.Helper()
		if err := db.InsertRecallDebugEvent(ctx, ev); err != nil {
			t.Fatalf("insert debug: %v", err)
		}
	}
	must(RecallDebugEvent{AgentID: agent, Lane: "wiki", Trigger: "auto", Query: "milk recipes", Candidates: 5, Gates: map[string]int{"below_threshold": 5}})
	must(RecallDebugEvent{AgentID: agent, Lane: "mem_inject", Trigger: "auto", Query: "Milk recipes", Candidates: 0, Gates: map[string]int{"no_pool": 1}})
	must(RecallDebugEvent{AgentID: agent, Lane: "wiki", Trigger: "auto", Query: "obscure thing", Candidates: 3, Gates: map[string]int{"below_threshold": 3}})
	must(RecallDebugEvent{AgentID: agent, Lane: "wiki", Trigger: "auto", Query: "good query", Candidates: 2, Hits: []RecallDebugHit{{ID: "entity:x", Score: 0.9, HitType: "vector"}}})

	n, err := db.MineHardcases(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("mine: %v", err)
	}
	if n != 2 {
		t.Fatalf("mined groups = %d, want 2", n)
	}

	hs, err := db.ListHardcases(ctx, agent, "", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hs) != 2 {
		t.Fatalf("hardcases = %d, want 2: %+v", len(hs), hs)
	}
	// "milk recipes" (normalized) aggregated both lanes and count=2. The
	// stored query text is whichever SQL group upserted first, so match by
	// hash, not exact text.
	var milk *Hardcase
	wantHash := HardcaseQueryHash("milk recipes")
	for i := range hs {
		if HardcaseQueryHash(hs[i].Query) == wantHash {
			milk = &hs[i]
		}
		if hs[i].Query == "good query" {
			t.Error("hit-bearing query must not be mined")
		}
	}
	if milk == nil {
		t.Fatalf("milk hardcase missing: %+v", hs)
	}
	if milk.Count != 2 {
		t.Errorf("milk count = %d, want 2 (normalized query merged lanes)", milk.Count)
	}
	if milk.Lanes != "wiki,mem_inject" && milk.Lanes != "mem_inject,wiki" {
		t.Errorf("milk lanes = %q, want wiki+mem_inject union", milk.Lanes)
	}

	// Re-mine with no new events: watermark skips the window, count stays.
	n, err = db.MineHardcases(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("re-mine: %v", err)
	}
	if n != 0 {
		t.Errorf("re-mined %d groups, want 0 (watermark)", n)
	}
	hs, _ = db.ListHardcases(ctx, agent, "", 10)
	for _, h := range hs {
		if h.Query == "milk recipes" && h.Count != 2 {
			t.Errorf("double count after re-mine: %d", h.Count)
		}
	}
}

func TestHardcaseStatusTransitions(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	agent := "hc-status-a"
	if err := db.InsertRecallDebugEvent(ctx, RecallDebugEvent{AgentID: agent, Lane: "wiki", Query: "q"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.MineHardcases(ctx, 24*time.Hour); err != nil {
		t.Fatalf("mine: %v", err)
	}
	hs, _ := db.ListHardcases(ctx, agent, "pending", 10)
	if len(hs) != 1 {
		t.Fatalf("pending = %d, want 1", len(hs))
	}
	if err := db.SetHardcaseStatus(ctx, agent, HardcaseQueryHash("q"), "golden"); err != nil {
		t.Fatalf("status: %v", err)
	}
	hs, _ = db.ListHardcases(ctx, agent, "pending", 10)
	if len(hs) != 0 {
		t.Errorf("pending after golden = %d, want 0", len(hs))
	}
	hs, _ = db.ListHardcases(ctx, agent, "golden", 10)
	if len(hs) != 1 {
		t.Errorf("golden list = %d, want 1", len(hs))
	}
}

func TestHardcaseQueryHashNormalizes(t *testing.T) {
	a := HardcaseQueryHash("  Milk Recipes ")
	b := HardcaseQueryHash("milk recipes")
	if a != b {
		t.Errorf("normalization broke: %q vs %q", a, b)
	}
	if HardcaseQueryHash("milk recipes") == HardcaseQueryHash("milk  recipes") {
		t.Error("different queries must hash differently")
	}
}
