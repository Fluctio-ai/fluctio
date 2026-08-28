package store

import (
	"context"
	"testing"
)

// TestSearchConversationSummariesFTSScanShape guards the column-list/Scan
// alignment of the FTS search: every SELECT must list exactly what
// scanConversationSummaries reads (17 columns). A doubled append or a
// missed column list fails here with "expected N destination arguments"
// instead of silently returning zero hits in production (2026-08-18: a
// double `, kind, superseded_by` append made every memory recall return
// empty while all other tests stayed green).
func TestSearchConversationSummariesFTSScanShape(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	seed := []ConversationSummary{
		{AgentID: "a1", SessionKey: "s1", Topic: "SQLite WAL 模式原理",
			Summary: "SQLite WAL 模式的原理是修改写入独立 WAL 文件", Keywords: []string{"sqlite", "wal"},
			SeqStart: 1, SeqEnd: 5, Kind: "durable"},
		{AgentID: "a1", SessionKey: "s2", Topic: "无关主题",
			Summary: "完全无关的内容 rust async", Keywords: []string{"rust"},
			SeqStart: 1, SeqEnd: 2, Kind: "episodic"},
	}
	ids := make([]int64, 0, len(seed))
	for _, s := range seed {
		id, err := db.InsertConversationSummary(ctx, s)
		if err != nil {
			t.Fatalf("insert %s: %v", s.Topic, err)
		}
		ids = append(ids, id)
	}

	// Lexical match must surface the SQLite row (and not the unrelated one).
	hits, err := db.SearchConversationSummariesFTS(ctx, "a1", "SQLite WAL", 10)
	if err != nil {
		t.Fatalf("fts: %v", err)
	}
	if len(hits) != 1 || hits[0].Topic != "SQLite WAL 模式原理" {
		t.Fatalf("hits = %d (first topic %q), want 1 hit SQLite WAL 模式原理", len(hits), firstTopicOr(hits))
	}

	// Superseded rows drop out of FTS.
	if err := db.MarkSummarySuperseded(ctx, "a1", ids[0], ids[1]); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	hits, err = db.SearchConversationSummariesFTS(ctx, "a1", "SQLite WAL", 10)
	if err != nil {
		t.Fatalf("fts after supersede: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("superseded row still surfaced: %d hits", len(hits))
	}
}

func firstTopicOr(hits []ConversationSummary) string {
	if len(hits) == 0 {
		return ""
	}
	return hits[0].Topic
}

// TestListConversationSummariesNeedingVectorScanShape guards the sqlite
// JOIN branch of the vector-backfill picker the same way the FTS test
// above guards the search: its s.-prefixed column list must carry
// kind + superseded_by too. The 2026-08 migration added those columns to
// every other SELECT but this one, so every sqlite backfill pass died on
// Scan with 15 columns vs 17 destinations instead of picking up rows.
func TestListConversationSummariesNeedingVectorScanShape(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	id, err := db.InsertConversationSummary(ctx, ConversationSummary{
		AgentID: "a1", SessionKey: "s1", Topic: "回填目标",
		Summary: "无向量的摘要应被向量回填选中", Keywords: []string{"vec"},
		SeqStart: 1, SeqEnd: 2, Kind: "durable",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	pending, err := db.ListConversationSummariesNeedingVector(ctx, "", 10)
	if err != nil {
		t.Fatalf("needing vector: %v", err)
	}
	found := false
	for _, p := range pending {
		if p.ID == id {
			found = true
			if p.Kind != "durable" || p.Topic != "回填目标" {
				t.Fatalf("misaligned scan: kind=%q topic=%q", p.Kind, p.Topic)
			}
		}
	}
	if !found {
		t.Fatalf("inserted summary not returned by the backfill picker")
	}
}
