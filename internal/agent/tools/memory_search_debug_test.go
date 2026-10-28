package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// memMockEmbedder returns a fixed unit vector — every query embeds the same,
// so a summary's cosine against the query is decided entirely by its stored
// vector (aligned e0 → 1.0, orthogonal e1 → 0). Vectors are 1024-dim because
// conversation_summaries_vec is a vec0 table with a fixed dimension.
type memMockEmbedder struct{}

func memMockVec(axis int) []float32 {
	v := make([]float32, 1024)
	v[axis] = 1
	return v
}

func (memMockEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = memMockVec(0)
	}
	return out, nil
}
func (memMockEmbedder) Model() string   { return "mock" }
func (memMockEmbedder) Dim() int        { return 1024 }
func (memMockEmbedder) Available() bool { return true }

// TestSemanticMemRecallWritesDebugEvent verifies the [MEM] auto-injection
// lane records its funnel: candidates from the FTS pool, the below_floor
// drop, and the survivor's per-lane attribution — and that record=false
// (the recall test box) writes nothing.
func TestSemanticMemRecallWritesDebugEvent(t *testing.T) {
	ctx := context.Background()
	db, err := store.NewDBStore("sqlite", "file:memrecall_debug_test?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	seed := []struct {
		session string
		summary string
		vec     []float32
	}{
		{"s-foam", "milk foam steaming technique", memMockVec(0)},
		{"s-tea", "milk tea shop playlist", memMockVec(1)},
	}
	for _, s := range seed {
		id, err := db.InsertConversationSummary(ctx, store.ConversationSummary{
			AgentID: "a1", SessionKey: s.session, Topic: s.summary,
			Summary: s.summary, Keywords: []string{"milk"}, SeqStart: 1, SeqEnd: 3, Kind: "episodic",
		})
		if err != nil {
			t.Fatalf("insert summary: %v", err)
		}
		if err := db.InsertConversationSummaryVector(ctx, id, s.vec); err != nil {
			t.Fatalf("insert vector: %v", err)
		}
	}

	hits, err := SemanticMemRecall(ctx, db, memMockEmbedder{}, nil, "a1", "u1", "milk", 3, nil, true)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	dump := func() string {
		events, _ := db.ListRecallDebugEvents(ctx, "a1", "mem_inject", 10, 0)
		b, _ := json.Marshal(events)
		return string(b)
	}
	// cos 1.0 passes the floor (max(0.5, 0.75×1.0)); cos 0 dies at it.
	if len(hits) != 1 || hits[0].Summary != "milk foam steaming technique" {
		t.Fatalf("hits = %+v, want only the aligned summary; debug rows = %s", hits, dump())
	}

	type dbgRow struct {
		lane       string
		trigger    string
		candidates int
		gates      map[string]int
		hits       []store.RecallDebugHit
	}
	read := func() []dbgRow {
		events, err := db.ListRecallDebugEvents(ctx, "a1", "mem_inject", 10, 0)
		if err != nil {
			t.Fatalf("list debug: %v", err)
		}
		out := make([]dbgRow, 0, len(events))
		for _, e := range events {
			out = append(out, dbgRow{e.Lane, e.Trigger, e.Candidates, e.Gates, e.Hits})
		}
		return out
	}

	rows := read()
	if len(rows) != 1 {
		t.Fatalf("debug rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.trigger != "auto" || r.candidates != 2 {
		t.Errorf("trigger/candidates = %s/%d, want auto/2", r.trigger, r.candidates)
	}
	if r.gates["below_floor"] != 1 {
		raw, _ := json.Marshal(r.gates)
		t.Errorf("below_floor = %d, want 1 (gates %s)", r.gates["below_floor"], raw)
	}
	if len(r.hits) != 1 {
		t.Fatalf("debug hits = %+v, want 1", r.hits)
	}
	h := r.hits[0]
	if h.HitType != "vector" || h.Ranks["vector"] != 1 || h.Scores["cosine"] != 1.0 {
		t.Errorf("hit attribution wrong: %+v", h)
	}
	if h.Ranks["fts"] == 0 {
		t.Errorf("fts rank missing: %+v", h)
	}

	// Preview mode (record=false): no new row.
	if _, err := SemanticMemRecall(ctx, db, memMockEmbedder{}, nil, "a1", "u1", "milk", 3, nil, false); err != nil {
		t.Fatalf("recall preview: %v", err)
	}
	if rows := read(); len(rows) != 1 {
		t.Fatalf("debug rows after preview = %d, want still 1", len(rows))
	}
}
