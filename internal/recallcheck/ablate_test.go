package recallcheck

import (
	"strconv"
	"testing"
)

func TestAblateGridScoring(t *testing.T) {
	ctx, db, ks := setup(t)
	defer db.Close()
	seedFixtures(t, ctx, db)

	var sumID int64
	if err := db.DB().QueryRowContext(ctx, `SELECT id FROM conversation_summaries WHERE agent_id='a1' LIMIT 1`).Scan(&sumID); err != nil {
		t.Fatalf("lookup summary: %v", err)
	}
	cases := []GoldenCase{
		{Schema: GoldenCaseSchema, ID: "m1", Query: "milk foam", Lane: "mem_inject", ExpectIDs: []string{strconv.FormatInt(sumID, 10)}},
	}
	grid := AblGrid{
		RRFK:    []int{20, 60},
		TopK:    []int{5, 10},
		Alphas:  []float64{0, 0.75},
		PoolCap: 30,
	}
	results := Ablate(ctx, Deps{DB: db, KB: ks, AgentID: "a1", Embedder: mockEmb{axis: 0}}, cases, grid)
	if len(results) != 8 {
		t.Fatalf("results = %d, want 2×2×2", len(results))
	}
	// Sorted by MRR desc.
	for i := 1; i < len(results); i++ {
		if results[i-1].MRR < results[i].MRR {
			t.Errorf("not sorted by MRR at %d: %f < %f", i, results[i-1].MRR, results[i].MRR)
		}
	}
	// The aligned fixture should be found by every config here.
	if results[0].MRR != 1.0 {
		t.Errorf("best MRR = %f, want 1.0 (fixture trivially findable)", results[0].MRR)
	}
	// Production config flagged exactly once.
	prod := 0
	for _, r := range results {
		if r.Production {
			prod++
		}
	}
	if prod != 1 {
		t.Errorf("production flag on %d rows, want 1", prod)
	}
	// Cases counted.
	if results[0].Cases != 1 {
		t.Errorf("cases = %d, want 1", results[0].Cases)
	}
}

func TestAblateEmptyWhenNoMemCases(t *testing.T) {
	ctx, db, ks := setup(t)
	defer db.Close()
	cases := []GoldenCase{
		{Schema: GoldenCaseSchema, ID: "w1", Query: "x", Lane: "wiki", ExpectIDs: []string{"entity:y"}},
	}
	if got := Ablate(ctx, Deps{DB: db, KB: ks, AgentID: "a1", Embedder: mockEmb{axis: 0}}, cases, DefaultAblGrid()); len(got) != 0 {
		t.Errorf("ablate over non-mem cases = %d results, want 0", len(got))
	}
}
