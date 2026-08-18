package store

import (
	"context"
	"testing"
	"time"
)

// TestPruneConversationSummaries covers the three deterministic prune
// rules: superseded rows past the grace window die (fresh ones stay),
// stale never-recalled episodic rows die (recalled + durable + fresh
// stay), and the per-agent quota evicts oldest episodic first.
func TestPruneConversationSummaries(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	old := time.Now().AddDate(0, 0, -120)
	fresh := time.Now().Add(-time.Hour)
	ins := func(topic, kind string, createdAt time.Time, supersededBy int64, access int) int64 {
		t.Helper()
		id, err := db.InsertConversationSummary(ctx, ConversationSummary{
			AgentID: "a1", SessionKey: "s-" + topic, Topic: topic,
			Summary: topic + " summary", Keywords: []string{topic},
			SeqStart: 1, SeqEnd: 2, Kind: kind,
		})
		if err != nil {
			t.Fatalf("insert %s: %v", topic, err)
		}
		_, err = db.db.ExecContext(ctx, `UPDATE conversation_summaries
			SET created_at = ?, superseded_by = ?, access_count = ? WHERE id = ?`,
			createdAt, supersededBy, access, id)
		if err != nil {
			t.Fatalf("stamp %s: %v", topic, err)
		}
		return id
	}
	supOld := ins("superseded-old", "durable", old, 99, 0)
	supFresh := ins("superseded-fresh", "durable", fresh, 99, 0)
	staleDead := ins("stale-episodic-dead", "episodic", old, 0, 0)
	staleUsed := ins("stale-episodic-used", "episodic", old, 0, 3)
	durableOld := ins("durable-old", "durable", old, 0, 0)
	_ = ins("fresh-episodic", "episodic", fresh, 0, 0)

	stats, err := db.PruneConversationSummaries(ctx, MemoryConsolidationCfg{
		SupersededGraceDays: 14, StaleEpisodicDays: 90, QuotaCap: 500, BatchLimit: 100,
	})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if stats.SupersededPurged != 1 || stats.StaleEvicted != 1 || stats.QuotaEvicted != 0 {
		t.Fatalf("stats = %+v, want superseded=1 stale=1 quota=0", stats)
	}

	exists := func(id int64) bool {
		var n int
		if err := db.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM conversation_summaries WHERE id = ?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if exists(supOld) {
		t.Error("superseded-old should be purged")
	}
	if !exists(supFresh) {
		t.Error("superseded-fresh should survive the grace window")
	}
	if exists(staleDead) {
		t.Error("stale never-recalled episodic should be evicted")
	}
	if !exists(staleUsed) {
		t.Error("recalled episodic should survive (access_count>0)")
	}
	if !exists(durableOld) {
		t.Error("durable rows are never evicted by age")
	}
}

// TestPruneConversationSummariesQuota evicts oldest episodic first when an
// agent is over the active-row cap.
func TestPruneConversationSummariesQuota(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Cap 2, agent has 2 episodic + 1 durable (oldest overall is an
	// episodic). Quota pass should evict the oldest episodic only.
	ins := func(topic, kind string, ageDays int) int64 {
		t.Helper()
		id, err := db.InsertConversationSummary(ctx, ConversationSummary{
			AgentID: "a1", SessionKey: "s-" + topic, Topic: topic,
			Summary: topic, SeqStart: 1, SeqEnd: 2, Kind: kind,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.db.ExecContext(ctx, `UPDATE conversation_summaries SET created_at = ? WHERE id = ?`,
			time.Now().AddDate(0, 0, -ageDays), id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldest := ins("epi-oldest", "episodic", 30)
	newer := ins("epi-newer", "episodic", 10)
	durable := ins("dur", "durable", 40)

	stats, err := db.PruneConversationSummaries(ctx, MemoryConsolidationCfg{
		SupersededGraceDays: 14, StaleEpisodicDays: 90, QuotaCap: 2, BatchLimit: 100,
	})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if stats.QuotaEvicted != 1 {
		t.Fatalf("quota evicted = %d, want 1", stats.QuotaEvicted)
	}
	var n int
	db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_summaries WHERE id IN (?,?,?)`,
		oldest, newer, durable).Scan(&n)
	if n != 2 {
		t.Fatalf("survivors = %d, want 2 (oldest episodic evicted, durable + newer kept)", n)
	}
}
