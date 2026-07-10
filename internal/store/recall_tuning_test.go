package store

import (
	"context"
	"fmt"
	"testing"
)

func TestAgentMMRLambdaDefaultAndRoundTrip(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// No row yet → default.
	got, err := db.GetAgentMMRLambda(ctx, "agent-1")
	if err != nil {
		t.Fatalf("get default: %v", err)
	}
	if got != DefaultMMRLambda {
		t.Errorf("default lambda = %v, want %v", got, DefaultMMRLambda)
	}

	// Set then get.
	if err := db.SetAgentMMRLambda(ctx, "agent-1", 0.35); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err = db.GetAgentMMRLambda(ctx, "agent-1")
	if err != nil {
		t.Fatalf("get after set: %v", err)
	}
	if got != 0.35 {
		t.Errorf("lambda after set = %v, want 0.35", got)
	}

	// Upsert (same agent, new value) replaces the row.
	if err := db.SetAgentMMRLambda(ctx, "agent-1", 0.8); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ = db.GetAgentMMRLambda(ctx, "agent-1")
	if got != 0.8 {
		t.Errorf("lambda after upsert = %v, want 0.8", got)
	}

	// A different agent is unaffected — still default.
	got, _ = db.GetAgentMMRLambda(ctx, "agent-2")
	if got != DefaultMMRLambda {
		t.Errorf("agent-2 lambda = %v, want default", got)
	}
}

func TestInsertRecallEvent(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	ev := RecallEvent{
		RecallID:   "recall-abc",
		AgentID:    "agent-1",
		Lambda:     0.6,
		Explored:   true,
		SummaryIDs: []int64{10, 20, 30},
	}
	if err := db.InsertRecallEvent(ctx, ev); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Verify the row landed with the right payload (incl. the JSON-encoded IDs).
	var (
		recallID string
		lambda   float64
		explored int
		idsJSON  string
	)
	err := db.db.QueryRowContext(ctx,
		`SELECT recall_id, lambda, explored, summary_ids FROM memory_recall_events WHERE recall_id = ?`,
		ev.RecallID).Scan(&recallID, &lambda, &explored, &idsJSON)
	if err != nil {
		t.Fatalf("query back: %v", err)
	}
	if recallID != "recall-abc" || lambda != 0.6 || explored != 1 {
		t.Errorf("row = %q %v %v, want recall-abc 0.6 1", recallID, lambda, explored)
	}
	if idsJSON != "[10,20,30]" {
		t.Errorf("summary_ids json = %q, want [10,20,30]", idsJSON)
	}
}

func TestTryUpgradeLambdaPromotesStrongExploration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Seed explored recalls: lambda=0.7 gets strong feedback (22 up / 3
	// down → 88%), lambda=0.5 gets weak (3 up / 22 down → 12%). Current
	// default lambda is 0.6 with no feedback.
	mustEvent := func(id string, lambda float64, up bool) {
		t.Helper()
		if err := db.InsertRecallEvent(ctx, RecallEvent{
			RecallID: id, AgentID: "a1", Lambda: lambda, Explored: true,
			SummaryIDs: []int64{1},
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.InsertRecallFeedback(ctx, id, up); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 25; i++ {
		mustEvent(fmt.Sprintf("r7-%d", i), 0.7, i >= 3) // 22 up
	}
	for i := 0; i < 25; i++ {
		mustEvent(fmt.Sprintf("r5-%d", i), 0.5, i < 3) // 3 up
	}

	upgraded, newLambda, err := db.TryUpgradeLambda(ctx, "a1")
	if err != nil {
		t.Fatalf("try upgrade: %v", err)
	}
	if !upgraded || newLambda != 0.7 {
		t.Errorf("upgrade = %v %v, want true 0.7", upgraded, newLambda)
	}
	got, _ := db.GetAgentMMRLambda(ctx, "a1")
	if got != 0.7 {
		t.Errorf("persisted lambda = %v, want 0.7", got)
	}
}

func TestTryUpgradeLambdaNoUpgradeBelowThreshold(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Only 5 samples at lambda=0.7 — below minSamples (20), no upgrade.
	for i := 0; i < 5; i++ {
		_ = db.InsertRecallEvent(ctx, RecallEvent{
			RecallID: fmt.Sprintf("x-%d", i), AgentID: "a1", Lambda: 0.7,
			Explored: true, SummaryIDs: []int64{1},
		})
		_ = db.InsertRecallFeedback(ctx, fmt.Sprintf("x-%d", i), true)
	}

	upgraded, newLambda, err := db.TryUpgradeLambda(ctx, "a1")
	if err != nil {
		t.Fatalf("try upgrade: %v", err)
	}
	if upgraded || newLambda != DefaultMMRLambda {
		t.Errorf("upgrade = %v %v, want false 0.6 (insufficient samples)", upgraded, newLambda)
	}
}
