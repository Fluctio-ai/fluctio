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

// TestShelvedSettleAndRetire covers the manual settlement endpoint's
// store leg and the retire sweep: resolution records the settlement
// provenance while source keeps its creation value, repeats are
// no-ops, and the sweep drops only over-age open rows across agents.
func TestShelvedSettleAndRetire(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	mk := func(agent, since string, seq int) ShelvedCandidate {
		return ShelvedCandidate{
			AgentID: agent, Kind: "deferred", Topic: "话题-" + since,
			SessionKey: "sess-1", Seq: seq, Since: since,
			LastSeenAt: time.Now(), Source: "summary", Extractor: "m1@v1",
		}
	}
	rows := []ShelvedCandidate{
		mk("agent-1", "2026-01-01", 1), // over-age
		mk("agent-1", "2026-11-09", 2), // fresh
		mk("agent-2", "2026-01-02", 3), // over-age, other agent
	}
	if err := db.UpsertShelvedCandidates(ctx, rows); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	open, _ := db.ListOpenShelvedCandidates(ctx, "agent-1", 20)
	if len(open) != 2 {
		t.Fatalf("open: %v", open)
	}
	var oldID, freshID int64
	if open[0].Since == "2026-01-01" {
		oldID, freshID = open[0].ID, open[1].ID
	} else {
		oldID, freshID = open[1].ID, open[0].ID
	}

	// Manual drop: settled once, idempotent after, provenance kept apart.
	if settled, err := db.MarkShelvedCandidateStatus(ctx, "agent-1", oldID, "dropped", "manual"); err != nil || !settled {
		t.Fatalf("drop: settled=%v err=%v", settled, err)
	}
	if settled, _ := db.MarkShelvedCandidateStatus(ctx, "agent-1", oldID, "dropped", "manual"); settled {
		t.Errorf("second drop must be a no-op")
	}
	var status, resolution, source string
	if err := db.db.QueryRow(`SELECT status, resolution, source FROM shelved_candidates WHERE id=?`, oldID).
		Scan(&status, &resolution, &source); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "dropped" || resolution != "manual" || source != "summary" {
		t.Errorf("dropped row wrong: status=%s resolution=%s source=%s", status, resolution, source)
	}

	// Manual resolve lands in the picked-back-up window with the same
	// provenance split.
	if settled, err := db.MarkShelvedCandidateStatus(ctx, "agent-1", freshID, "resolved", "manual"); err != nil || !settled {
		t.Fatalf("resolve: settled=%v err=%v", settled, err)
	}
	now := time.Now()
	between, err := db.ListShelvedCandidatesResolvedBetween(ctx, "agent-1", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(between) != 1 || between[0].ID != freshID || between[0].Resolution != "manual" {
		t.Fatalf("resolved between: err=%v rows=%+v", err, between)
	}

	// Retire sweep: over-age open rows go dropped/expired on every agent;
	// fresh and already-settled rows are untouched. Rerun matches zero.
	n, err := db.RetireStaleShelvedCandidates(ctx, "2026-06-01")
	if err != nil || n != 1 {
		t.Fatalf("retire: n=%d err=%v", n, err)
	}
	if n, _ = db.RetireStaleShelvedCandidates(ctx, "2026-06-01"); n != 0 {
		t.Errorf("retire rerun matched %d rows", n)
	}
	other, _ := db.ListOpenShelvedCandidates(ctx, "agent-2", 20)
	if len(other) != 0 {
		t.Errorf("other agent's stale row must retire: %+v", other)
	}
	if err := db.db.QueryRow(`SELECT status, resolution FROM shelved_candidates WHERE agent_id='agent-2'`).
		Scan(&status, &resolution); err != nil {
		t.Fatalf("read back other: %v", err)
	}
	if status != "dropped" || resolution != "expired" {
		t.Errorf("retired row wrong: status=%s resolution=%s", status, resolution)
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
