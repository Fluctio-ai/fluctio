package store

import (
	"context"
	"testing"
	"time"
)

// TestShelvedCandidatesLifecycle covers the summary extractor's write
// path: idempotent upsert on the anchor key, last_seen_at refresh, open
// listing, and settlement restricted to open rows.
func TestShelvedCandidatesLifecycle(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	c := ShelvedCandidate{
		AgentID: "agent-1", Kind: "deferred", Topic: "整理周报模板",
		Context: "聊周报时提到", Reason: "说了回头再弄",
		SessionKey: "sess-1", Seq: 11, Since: "2026-11-01",
		LastSeenAt: time.Now(), Source: "summary", Extractor: "m1@v1",
		ActionPrompt: "补完周报模板",
	}
	if err := db.UpsertShelvedCandidates(ctx, []ShelvedCandidate{c}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	open, err := db.ListOpenShelvedCandidates(ctx, "agent-1", 20)
	if err != nil || len(open) != 1 {
		t.Fatalf("list: %v len=%d", err, len(open))
	}
	first := open[0]
	if first.Topic != "整理周报模板" || first.Since != "2026-11-01" || first.Status != "open" {
		t.Errorf("row fields wrong: %+v", first)
	}

	// Same anchor again refreshes in place; no duplicate.
	c.Topic = "整理周报模板（重提）"
	later := time.Now().Add(time.Hour)
	c.LastSeenAt = later
	if err := db.UpsertShelvedCandidates(ctx, []ShelvedCandidate{c}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	open, _ = db.ListOpenShelvedCandidates(ctx, "agent-1", 20)
	if len(open) != 1 || open[0].Topic != "整理周报模板（重提）" {
		t.Fatalf("upsert must be idempotent: %+v", open)
	}

	// A different kind on the same line is a distinct open loop.
	c.Kind = "commitment"
	if err := db.UpsertShelvedCandidates(ctx, []ShelvedCandidate{c}); err != nil {
		t.Fatalf("upsert other kind: %v", err)
	}
	if open, _ = db.ListOpenShelvedCandidates(ctx, "agent-1", 20); len(open) != 2 {
		t.Fatalf("want 2 kinds, got %d", len(open))
	}

	// Resolve only open rows, only for this agent.
	n, err := db.MarkShelvedCandidatesResolved(ctx, "agent-1", []int64{first.ID, 9999}, "summary")
	if err != nil || n != 1 {
		t.Fatalf("resolve: n=%d err=%v", n, err)
	}
	if open, _ = db.ListOpenShelvedCandidates(ctx, "agent-1", 20); len(open) != 1 {
		t.Fatalf("resolved row must leave the open list, got %d", len(open))
	}
	// Resolving an already-resolved row is a no-op.
	if n, _ = db.MarkShelvedCandidatesResolved(ctx, "agent-1", []int64{first.ID}, "summary"); n != 0 {
		t.Errorf("double resolve changed %d rows", n)
	}
	// Other agents see nothing.
	if other, _ := db.ListOpenShelvedCandidates(ctx, "agent-2", 20); len(other) != 0 {
		t.Errorf("agent isolation broken: %+v", other)
	}

	// A later upsert must not reopen the resolved row.
	c.Kind = "deferred"
	c.Topic = "又提了一次"
	if err := db.UpsertShelvedCandidates(ctx, []ShelvedCandidate{c}); err != nil {
		t.Fatalf("upsert after resolve: %v", err)
	}
	open, _ = db.ListOpenShelvedCandidates(ctx, "agent-1", 20)
	for _, row := range open {
		if row.Topic == "又提了一次" {
			t.Errorf("resolved row was reopened")
		}
	}

	// Resolved-between only sees rows settled inside the window.
	now := time.Now()
	resolved, err := db.ListShelvedCandidatesResolvedBetween(ctx, "agent-1", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(resolved) != 1 || resolved[0].ID != first.ID {
		t.Fatalf("resolved between: err=%v rows=%+v", err, resolved)
	}
	if got, _ := db.ListShelvedCandidatesResolvedBetween(ctx, "agent-1", now.Add(time.Hour), now.Add(2*time.Hour)); len(got) != 0 {
		t.Errorf("future window must be empty: %+v", got)
	}

	// Count includes every status (the diary's "extractor ever wrote" probe).
	if has, err := db.HasShelvedCandidates(ctx, "agent-1"); err != nil || !has {
		t.Errorf("has: %v err=%v, want true", has, err)
	}
	if has, _ := db.HasShelvedCandidates(ctx, "agent-2"); has {
		t.Errorf("agent isolation broken on has")
	}
}

// TestShelvedExtractRetryQueue covers the malformed-section queue:
// one row per window, attempts counted only by real retries, drop on
// settle.
func TestShelvedExtractRetryQueue(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	if err := db.EnqueueShelvedExtractRetry(ctx, "agent-1", "sess-1", 10, 20, "bad json"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Same window again refreshes, never duplicates.
	if err := db.EnqueueShelvedExtractRetry(ctx, "agent-1", "sess-1", 10, 20, "bad json again"); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	rows, err := db.ListShelvedExtractRetries(ctx, "agent-1", "sess-1", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: err=%v rows=%+v", err, rows)
	}
	if rows[0].Attempts != 0 || rows[0].LastError != "bad json again" {
		t.Errorf("enqueue must not bump attempts: %+v", rows[0])
	}
	if other, _ := db.ListShelvedExtractRetries(ctx, "agent-1", "sess-2", 10); len(other) != 0 {
		t.Errorf("session isolation broken: %+v", other)
	}

	if err := db.BumpShelvedExtractRetry(ctx, rows[0].ID, "still bad"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	rows, _ = db.ListShelvedExtractRetries(ctx, "agent-1", "sess-1", 10)
	if len(rows) != 1 || rows[0].Attempts != 1 || rows[0].LastError != "still bad" {
		t.Fatalf("bump not reflected: %+v", rows)
	}

	if err := db.DeleteShelvedExtractRetry(ctx, rows[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if rows, _ = db.ListShelvedExtractRetries(ctx, "agent-1", "sess-1", 10); len(rows) != 0 {
		t.Errorf("delete left rows: %+v", rows)
	}
}

// TestBackfillShelvedCandidates covers the switchover import: the latest
// diary ledger's open entries become candidates once per agent, and an
// agent that already has candidates is left alone.
func TestBackfillShelvedCandidates(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	if err := db.InsertDailyDiary(ctx, DailyDiary{
		AgentID: "agent-1", Date: "2026-11-03",
		Shelved: []DiaryShelved{
			{Topic: "旧账本-未闭环", Context: "聊到一半", Since: "2026-11-01", Session: "s-1", Seq: 7, Status: "open"},
			{Topic: "旧账本-已捡起", Since: "2026-10-30", Session: "s-1", Seq: 5, Status: "resolved"},
		},
	}); err != nil {
		t.Fatalf("insert diary: %v", err)
	}
	if err := db.backfillShelvedCandidates(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	open, err := db.ListOpenShelvedCandidates(ctx, "agent-1", 20)
	if err != nil || len(open) != 1 {
		t.Fatalf("backfill open: err=%v rows=%+v", err, open)
	}
	if open[0].Topic != "旧账本-未闭环" || open[0].Source != "legacy_backfill" ||
		open[0].Since != "2026-11-01" || open[0].SessionKey != "s-1" || open[0].Seq != 7 {
		t.Errorf("backfilled row wrong: %+v", open[0])
	}

	// A second run is a no-op (the agent already has candidates).
	if err := db.backfillShelvedCandidates(ctx); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if open, _ = db.ListOpenShelvedCandidates(ctx, "agent-1", 20); len(open) != 1 {
		t.Errorf("backfill must be idempotent, got %d rows", len(open))
	}
}
