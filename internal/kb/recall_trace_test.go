package kb

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
)

// wireTestTracer bridges TraceEvents straight into the test schema's
// recall_debug_events via raw SQL — production's agent layer does the same
// bridge to the store insert.
func wireTestTracer(ks *KBStore, db *sql.DB) {
	ks.SetRecallTracer(func(tctx context.Context, ev TraceEvent) {
		marshal := func(v any) string {
			if v == nil {
				return ""
			}
			b, _ := json.Marshal(v)
			return string(b)
		}
		_, _ = db.ExecContext(tctx,
			`INSERT INTO recall_debug_events (agent_id, lane, trigger_kind, query, candidates, gates, params, hits) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			ev.AgentID, ev.Lane, ev.Trigger, ev.Query, ev.Candidates, marshal(ev.Gates), marshal(ev.Params), marshal(ev.Hits))
	})
}

// recall_debug_events is migrated by the store package in production; the kb
// test schema mirrors it (the insert path itself is best-effort, so the other
// kb tests keep passing without the table).
const recallDebugEventsDDL = `CREATE TABLE IF NOT EXISTS recall_debug_events (
	id INTEGER PRIMARY KEY, agent_id TEXT NOT NULL, lane TEXT NOT NULL,
	trigger_kind TEXT NOT NULL DEFAULT '', query TEXT NOT NULL DEFAULT '',
	candidates INTEGER NOT NULL DEFAULT 0, gates TEXT NOT NULL DEFAULT '',
	params TEXT NOT NULL DEFAULT '', hits TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`

type recallDebugRow struct {
	lane       string
	trigger    string
	candidates int
	gates      map[string]int
	hits       []TraceHit
}

func readRecallDebugRows(t *testing.T, db *sql.DB, agentID string) []recallDebugRow {
	t.Helper()
	rows, err := db.Query(`SELECT lane, trigger_kind, candidates, gates, hits FROM recall_debug_events WHERE agent_id = ? ORDER BY id`, agentID)
	if err != nil {
		t.Fatalf("read debug rows: %v", err)
	}
	defer rows.Close()
	var out []recallDebugRow
	for rows.Next() {
		var r recallDebugRow
		var gates, hits string
		if err := rows.Scan(&r.lane, &r.trigger, &r.candidates, &gates, &hits); err != nil {
			t.Fatalf("scan: %v", err)
		}
		_ = json.Unmarshal([]byte(gates), &r.gates)
		_ = json.Unmarshal([]byte(hits), &r.hits)
		out = append(out, r)
	}
	return out
}

// TestSearchSplitWritesRecallTrace verifies the auto-recall path records one
// funnel row per lane: candidates scanned, per-gate drop counts, and the
// survivor's pre-gate cosine rank.
func TestSearchSplitWritesRecallTrace(t *testing.T) {
	db := setupKBVectorTestDB(t)
	defer db.Close()
	if _, err := db.Exec(recallDebugEventsDDL); err != nil {
		t.Fatalf("create debug table: %v", err)
	}
	ctx := context.Background()

	// Two wiki pages: one identical to the query vector (cos 1.0 → passes
	// the 0.45 threshold), one orthogonal (cos 0 → dropped by the gate).
	pages := []struct {
		id, title string
		vec       []float32
	}{
		{"entity:foo", "Foo", []float32{1, 0, 0}},
		{"entity:bar", "Bar", []float32{0, 1, 0}},
	}
	for _, p := range pages {
		if _, err := db.ExecContext(ctx, `INSERT INTO wiki_pages (id, agent_id, page_type, slug, title, body, summary, source_ids, tags) VALUES (?, 'a1', 'entity', ?, ?, ?, ?, '[]', '[]')`,
			p.id, p.title, p.title, "body "+p.title, p.title+" summary"); err != nil {
			t.Fatalf("insert page: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO wiki_page_embeddings (page_id, agent_id, embedding, dim, model) VALUES (?, 'a1', ?, 3, 'mock')`,
			p.id, vecToBlob(p.vec)); err != nil {
			t.Fatalf("insert emb: %v", err)
		}
	}

	ks := NewKBStore(db, "sqlite")
	ks.SetRetriever(mockEmbedder{}, nil)
	wireTestTracer(ks, db)

	results, err := ks.SearchSplit(ctx, "a1", "foo query", 5, 0.45, 0, 0.6)
	if err != nil {
		t.Fatalf("searchsplit: %v", err)
	}
	if len(results) != 1 || results[0].SourceID != "entity:foo" {
		t.Fatalf("results = %+v, want only entity:foo", results)
	}

	rows := readRecallDebugRows(t, db, "a1")
	if len(rows) != 1 {
		t.Fatalf("debug rows = %d, want 1 (wiki lane only; ftLimit=0)", len(rows))
	}
	r := rows[0]
	if r.lane != "wiki" || r.trigger != "auto" {
		t.Errorf("lane/trigger = %s/%s, want wiki/auto", r.lane, r.trigger)
	}
	if r.candidates != 2 {
		t.Errorf("candidates = %d, want 2", r.candidates)
	}
	if r.gates["below_threshold"] != 1 {
		t.Errorf("below_threshold = %d, want 1 (gates %+v)", r.gates["below_threshold"], r.gates)
	}
	if len(r.hits) != 1 || r.hits[0].ID != "entity:foo" {
		t.Fatalf("hits = %+v, want entity:foo", r.hits)
	}
	h := r.hits[0]
	if h.Ranks["vector"] != 1 || h.Scores["vector"] != 1.0 || h.HitType != "vector" {
		t.Errorf("hit attribution wrong: %+v", h)
	}
}

// TestSearchSplitFlashLaneTrace verifies the flash/todo lane records a row
// even when every candidate dies at the threshold — the "silent lane" case
// the counters exist to explain.
func TestSearchSplitFlashLaneTrace(t *testing.T) {
	db := setupKBVectorTestDB(t)
	defer db.Close()
	if _, err := db.Exec(recallDebugEventsDDL); err != nil {
		t.Fatalf("create debug table: %v", err)
	}
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `INSERT INTO kb_sources (id, agent_id, title, source_type, source_ref, type) VALUES ('s1', 'a1', 'flash one', 'manual', '', 'flash')`); err != nil {
		t.Fatalf("insert source: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_entries (uuid, source_id, chunk_index, content, agent_id) VALUES ('u1', 's1', 0, 'unrelated note', 'a1')`); err != nil {
		t.Fatalf("insert entry: %v", err)
	}
	// Orthogonal to the mock query vector → cos 0 → below the 0.6 ft floor.
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_entry_embeddings (entry_id, agent_id, embedding, dim, model) VALUES (1, 'a1', ?, 3, 'mock')`, vecToBlob([]float32{0, 1, 0})); err != nil {
		t.Fatalf("insert emb: %v", err)
	}

	ks := NewKBStore(db, "sqlite")
	ks.SetRetriever(mockEmbedder{}, nil)
	wireTestTracer(ks, db)

	results, err := ks.SearchSplit(ctx, "a1", "query", 0, 0.45, 5, 0.6)
	if err != nil {
		t.Fatalf("searchsplit: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %+v, want empty (all below threshold)", results)
	}

	rows := readRecallDebugRows(t, db, "a1")
	if len(rows) != 1 {
		t.Fatalf("debug rows = %d, want 1 (flash_todo lane)", len(rows))
	}
	r := rows[0]
	if r.lane != "flash_todo" || r.trigger != "auto" {
		t.Errorf("lane/trigger = %s/%s, want flash_todo/auto", r.lane, r.trigger)
	}
	if r.candidates != 1 {
		t.Errorf("candidates = %d, want 1", r.candidates)
	}
	if r.gates["below_threshold"] != 1 {
		t.Errorf("below_threshold = %d, want 1 (gates %+v)", r.gates["below_threshold"], r.gates)
	}
	if len(r.hits) != 0 {
		t.Errorf("hits = %+v, want empty", r.hits)
	}
}

// TestRecallTraceBestEffortWithoutTable proves the trace insert degrades to
// a debug log when recall_debug_events is missing (unit-test DBs that don't
// create it), never failing the search itself.
func TestRecallTraceBestEffortWithoutTable(t *testing.T) {
	db := setupKBVectorTestDB(t)
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `INSERT INTO wiki_pages (id, agent_id, page_type, slug, title, body, summary, source_ids, tags) VALUES ('entity:foo', 'a1', 'entity', 'foo', 'Foo', 'body foo', 'Foo summary', '[]', '[]')`); err != nil {
		t.Fatalf("insert page: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO wiki_page_embeddings (page_id, agent_id, embedding, dim, model) VALUES ('entity:foo', 'a1', ?, 3, 'mock')`, vecToBlob([]float32{1, 0, 0})); err != nil {
		t.Fatalf("insert emb: %v", err)
	}

	ks := NewKBStore(db, "sqlite")
	ks.SetRetriever(mockEmbedder{}, nil)
	results, err := ks.SearchSplit(ctx, "a1", "foo", 5, 0.45, 0, 0.6)
	if err != nil {
		t.Fatalf("searchsplit: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v, want 1 (trace failure must not affect recall)", results)
	}
}
