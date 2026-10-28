package store

import (
	"context"
	"testing"
	"time"
)

func TestInsertAndListRecallDebugEvents(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	ev := RecallDebugEvent{
		AgentID:    "a1",
		Lane:       "wiki",
		Trigger:    "auto",
		Query:      "milk",
		Candidates: 7,
		Gates:      map[string]int{"below_threshold": 5, "pool_cap": 1},
		Params:     map[string]any{"threshold": 0.45, "limit": 5},
		Hits: []RecallDebugHit{
			{
				ID:      "entity:foo",
				Score:   0.91,
				HitType: "vector",
				Ranks:   map[string]int{"vector": 1},
				Scores:  map[string]float64{"vector": 0.91},
			},
		},
	}
	if err := db.InsertRecallDebugEvent(ctx, ev); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// A second lane's event for the same agent.
	if err := db.InsertRecallDebugEvent(ctx, RecallDebugEvent{
		AgentID: "a1", Lane: "mem_inject", Trigger: "auto", Query: "milk",
		Candidates: 0, Gates: map[string]int{"no_pool": 1},
	}); err != nil {
		t.Fatalf("insert second: %v", err)
	}

	got, err := db.ListRecallDebugEvents(ctx, "a1", "", 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %d events, want 2", len(got))
	}
	// Lane filter.
	got, err = db.ListRecallDebugEvents(ctx, "a1", "wiki", 10, 0)
	if err != nil {
		t.Fatalf("list lane: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("listed %d wiki events, want 1", len(got))
	}
	w := got[0]
	if w.Candidates != 7 || w.Gates["below_threshold"] != 5 || w.Gates["pool_cap"] != 1 {
		t.Errorf("wiki event gates/candidates wrong: %+v %+v", w.Candidates, w.Gates)
	}
	if len(w.Hits) != 1 || w.Hits[0].Ranks["vector"] != 1 || w.Hits[0].HitType != "vector" {
		t.Errorf("wiki event hits wrong: %+v", w.Hits)
	}
	if w.Params["threshold"] != 0.45 {
		t.Errorf("params wrong: %+v", w.Params)
	}
}

func TestPruneRecallDebugEvents(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	for _, agent := range []string{"a1", "a1"} {
		if err := db.InsertRecallDebugEvent(ctx, RecallDebugEvent{
			AgentID: agent, Lane: "wiki", Trigger: "auto", Query: "q",
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	// Age one of the two rows past the retention window.
	old := time.Now().Add(-48 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	if _, err := db.db.ExecContext(ctx,
		`UPDATE recall_debug_events SET created_at = ? WHERE id = (SELECT MIN(id) FROM recall_debug_events)`, old); err != nil {
		t.Fatalf("age row: %v", err)
	}

	n, err := db.PruneRecallDebugEvents(ctx, time.Now().Add(-24*time.Hour), 1000)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want 1", n)
	}
	left, err := db.recallDebugEventCount(ctx, "a1")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 1 {
		t.Fatalf("left %d rows, want 1", left)
	}
}
