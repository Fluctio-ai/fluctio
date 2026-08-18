package tools

import (
	"testing"

	"github.com/fluctio-ai/fluctio/internal/store"
)

func mkSum(id int64) store.ConversationSummary { return store.ConversationSummary{ID: id} }

// TestFuseSummariesRRFDualLaneHitRanksFirst: a summary found by both the
// FTS and the vector lane must outrank summaries found by only one lane,
// regardless of its rank inside either lane.
func TestFuseSummariesRRFDualLaneHitRanksFirst(t *testing.T) {
	fts := []store.ConversationSummary{mkSum(1), mkSum(2), mkSum(3), mkSum(4)}
	vec := []store.ConversationSummary{mkSum(5), mkSum(6), mkSum(2), mkSum(7)}

	out := FuseSummariesRRF(fts, vec, 0)
	if len(out) == 0 || out[0].ID != 2 {
		t.Fatalf("first = %v, want id=2 (present in both lanes)", firstID(out))
	}
}

// TestFuseSummariesRRFKeepsFtsOrderWithinLane: single-lane input must
// degrade to that lane's own order (rank fusion of one list = its order).
func TestFuseSummariesRRFKeepsFtsOrderWithinLane(t *testing.T) {
	fts := []store.ConversationSummary{mkSum(10), mkSum(20), mkSum(30)}
	out := FuseSummariesRRF(fts, nil, 0)
	if len(out) != 3 || out[0].ID != 10 || out[2].ID != 30 {
		t.Fatalf("order = %v, want [10 20 30]", idList(out))
	}
}

// TestFuseSummariesRRFLimitCapsAndTieBreaksById: the cap applies after
// fusion, and equal scores (same ranks in disjoint lanes) tie-break by ID
// deterministically.
func TestFuseSummariesRRFLimitCapsAndTieBreaksById(t *testing.T) {
	fts := []store.ConversationSummary{mkSum(9), mkSum(1)}
	vec := []store.ConversationSummary{mkSum(3), mkSum(7)}
	out := FuseSummariesRRF(fts, vec, 3)
	if len(out) != 3 {
		t.Fatalf("len = %d, want cap 3", len(out))
	}
	// rank-1 items from both lanes (ids 9 and 3) tie; the smaller ID wins.
	if out[0].ID != 3 {
		t.Fatalf("tie-break first = %d, want 3", out[0].ID)
	}
}

// TestApplyRelativeFloorDropsTail: with a strong best hit, hits below
// alpha×best must drop; the best hit always survives.
func TestApplyRelativeFloorDropsTail(t *testing.T) {
	hits := []store.ConversationSummary{mkSum(1), mkSum(2), mkSum(3), mkSum(4)}
	scores := map[int64]float64{1: 0.90, 2: 0.61, 3: 0.30, 4: 0.68}
	// floor = 0.75 × 0.90 = 0.675 → keeps 1 (0.90) and 4 (0.68); drops 2, 3.
	out := applyRelativeFloor(hits, scores, 0.75)
	if len(out) != 2 || out[0].ID != 1 || out[1].ID != 4 {
		t.Fatalf("kept = %v, want [1 4]", idList(out))
	}
}

// TestApplyRelativeFloorKeepsUnscoredAndDegenerate: hits without a score
// stay (no evidence to drop), and a single hit / empty scores pass through.
func TestApplyRelativeFloorKeepsUnscoredAndDegenerate(t *testing.T) {
	hits := []store.ConversationSummary{mkSum(1), mkSum(2), mkSum(3)}
	scores := map[int64]float64{1: 0.9}
	out := applyRelativeFloor(hits, scores, 0.75)
	if len(out) != 3 {
		t.Fatalf("unscored hits dropped: kept = %v, want all 3", idList(out))
	}
	single := applyRelativeFloor(hits[:1], nil, 0.75)
	if len(single) != 1 {
		t.Fatalf("single hit = %d, want passthrough 1", len(single))
	}
}

func firstID(list []store.ConversationSummary) int64 {
	if len(list) == 0 {
		return -1
	}
	return list[0].ID
}

func idList(list []store.ConversationSummary) []int64 {
	out := make([]int64, 0, len(list))
	for _, s := range list {
		out = append(out, s.ID)
	}
	return out
}
