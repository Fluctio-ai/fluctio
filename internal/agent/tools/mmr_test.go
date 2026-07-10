package tools

import (
	"math"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/store"
)

func TestCosineSim(t *testing.T) {
	// identical → 1
	if got := cosineSim([]float32{1, 0, 0}, []float32{1, 0, 0}); math.Abs(got-1) > 1e-9 {
		t.Errorf("identical = %v, want 1", got)
	}
	// orthogonal → 0
	if got := cosineSim([]float32{1, 0}, []float32{0, 1}); math.Abs(got) > 1e-9 {
		t.Errorf("orthogonal = %v, want 0", got)
	}
	// opposite → -1
	if got := cosineSim([]float32{1, 0}, []float32{-1, 0}); math.Abs(got-(-1)) > 1e-9 {
		t.Errorf("opposite = %v, want -1", got)
	}
	// mismatched length → 0
	if got := cosineSim([]float32{1, 0}, []float32{1}); got != 0 {
		t.Errorf("mismatched = %v, want 0", got)
	}
	// empty → 0
	if got := cosineSim(nil, nil); got != 0 {
		t.Errorf("empty = %v, want 0", got)
	}
}

func TestSelectMMR(t *testing.T) {
	// query along dim0; A close to query, B a near-duplicate of A
	// (redundant), C orthogonal (diverse). Diversity-aware MMR should
	// prefer C over B as the second pick.
	query := []float32{1, 0}
	emb := map[int64][]float32{
		1: {1, 0},        // A: maximally relevant
		2: {0.99, 0.01},  // B: near-duplicate of A
		3: {0, 1},        // C: orthogonal, diverse
	}
	cands := []store.ConversationSummary{{ID: 1}, {ID: 2}, {ID: 3}}

	// lambda=1: pure relevance → A first.
	got := selectMMR(cands, emb, query, 1.0, 3)
	if len(got) != 3 || got[0].ID != 1 {
		t.Fatalf("lambda=1 first = %v, want 1", mmrIDs(got))
	}

	// lambda=0.3: diversity matters → second pick should be C (3), not B.
	got = selectMMR(cands, emb, query, 0.3, 3)
	if len(got) < 2 || got[1].ID != 3 {
		t.Errorf("lambda=0.3 second = %d, want 3 (diverse); order=%v", got[1].ID, mmrIDs(got))
	}

	// topK larger than pool → returns all available.
	got = selectMMR(cands, emb, query, 0.5, 10)
	if len(got) != 3 {
		t.Errorf("topK>pool = %d, want 3", len(got))
	}

	// candidate missing a vector is skipped.
	noVec := []store.ConversationSummary{{ID: 1}, {ID: 99}}
	got = selectMMR(noVec, emb, query, 0.5, 5)
	if len(got) != 1 || got[0].ID != 1 {
		t.Errorf("missing-vector skip = %v, want [1]", mmrIDs(got))
	}

	// empty input → nil.
	if got := selectMMR(nil, emb, query, 0.5, 5); got != nil {
		t.Errorf("nil candidates = %v, want nil", mmrIDs(got))
	}
}

func mmrIDs(s []store.ConversationSummary) []int64 {
	out := make([]int64, len(s))
	for i, v := range s {
		out[i] = v.ID
	}
	return out
}
