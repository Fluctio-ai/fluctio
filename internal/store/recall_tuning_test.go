package store

import (
	"context"
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
