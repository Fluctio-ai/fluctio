package recallcheck

import (
	"context"
	"math"
	"sort"
	"strconv"
)

// Offline fusion ablation: one retrieval pass per case records the full
// per-lane ranked lists (FTS + vector); then a grid of fusion configs is
// replayed over those lists alone — no re-embedding, no re-querying. This
// is the MimirQ hybrid_runner pattern: tuning RRF/top_k/floor against the
// golden set costs one embed call per case instead of one per config.
//
// The replay mirrors the memory_search tool's Path A fusion stage
// (FuseSummariesRRF + relative floor) without the tool's cited-dedup /
// reranker / MMR layers, so results are a what-if on the fusion knobs, not
// a byte-exact reproduction of the tool.

// AblGrid enumerates the configs to sweep.
type AblGrid struct {
	RRFK    []int     // reciprocal-rank damping constant (production: 60)
	TopK    []int     // final cut (production tool default: limit arg)
	Alphas  []float64 // relative floor = alpha × best cosine; 0 = no floor (production: 0.75)
	PoolCap int       // per-lane candidates to record (0 = 30)
}

// DefaultAblGrid brackets the production constants (rrf_k=60, floor 0.75)
// wide enough to reveal cliffs without exploding the run count.
func DefaultAblGrid() AblGrid {
	return AblGrid{
		RRFK:    []int{20, 40, 60, 100},
		TopK:    []int{3, 5, 10},
		Alphas:  []float64{0, 0.6, 0.75, 0.9},
		PoolCap: 30,
	}
}

// AblResult is one config's aggregate score.
type AblResult struct {
	RRFK       int     `json:"rrf_k"`
	TopK       int     `json:"top_k"`
	Alpha      float64 `json:"alpha"`
	Hit1       int     `json:"hit1"`
	Hit3       int     `json:"hit3"`
	Hit5       int     `json:"hit5"`
	MRR        float64 `json:"mrr"`
	Cases      int     `json:"cases"`
	Production bool    `json:"production,omitempty"` // the shipped constants
}

// laneLists is one case's recorded per-lane rankings.
type laneLists struct {
	ftsRanks  []int64 // summary ids in FTS order
	vecRanks  []int64 // in vector-KNN order
	cosine    map[int64]float64
}

// recordLaneLists runs the two-lane recall once and captures full rankings.
func recordLaneLists(ctx context.Context, deps Deps, query string, cap int) (*laneLists, error) {
	if cap <= 0 {
		cap = 30
	}
	ll := &laneLists{cosine: map[int64]float64{}}
	fts, err := deps.DB.SearchConversationSummariesFTS(ctx, deps.AgentID, query, cap)
	if err != nil {
		return nil, err
	}
	for _, h := range fts {
		ll.ftsRanks = append(ll.ftsRanks, h.ID)
	}
	if deps.Embedder != nil && deps.Embedder.Available() {
		vecs, err := deps.Embedder.Embed(ctx, []string{query})
		if err != nil || len(vecs) != 1 {
			return ll, nil // lexical-only what-if
		}
		vecHits, err := deps.DB.SearchConversationSummariesVectorScored(ctx, vecs[0], cap)
		if err != nil {
			return ll, nil
		}
		scoped := make([]int64, 0, len(vecHits))
		for _, vh := range vecHits {
			scoped = append(scoped, vh.ID)
		}
		active, err := deps.DB.GetActiveSummariesByIDs(ctx, deps.AgentID, scoped)
		if err != nil {
			return ll, nil
		}
		activeSet := map[int64]bool{}
		for _, a := range active {
			activeSet[a.ID] = true
		}
		for _, id := range scoped {
			if activeSet[id] {
				ll.vecRanks = append(ll.vecRanks, id)
			}
		}
		// Cosines for the floor what-ifs: reuse stored embeddings.
		if len(ll.vecRanks) > 0 {
			embMap, err := deps.DB.GetConversationSummaryEmbeddings(ctx, ll.vecRanks)
			if err == nil {
				for id, v := range embMap {
					ll.cosine[id] = cosF64(vecs[0], v)
				}
			}
		}
	}
	return ll, nil
}

func cosF64(a, b []float32) float64 {
	var dot, na, nb float64
	for i := 0; i < len(a) && i < len(b); i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// fuseRRF re-ranks with the given damping constant, mirroring
// tools.FuseSummariesRRF: score = Σ 1/(k+rank) across lanes, ties by id.
func fuseRRF(ll *laneLists, k int) []int64 {
	score := map[int64]float64{}
	add := func(list []int64) {
		for i, id := range list {
			score[id] += 1 / (float64(k) + float64(i+1))
		}
	}
	add(ll.ftsRanks)
	add(ll.vecRanks)
	ids := make([]int64, 0, len(score))
	for id := range score {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool {
		if score[ids[a]] != score[ids[b]] {
			return score[ids[a]] > score[ids[b]]
		}
		return ids[a] < ids[b]
	})
	return ids
}

// Ablate runs the grid over the mem_inject-lane golden cases and returns
// results sorted by MRR descending. Cases whose lanes recorded no vector
// list still contribute (lexical what-if).
func Ablate(ctx context.Context, deps Deps, cases []GoldenCase, grid AblGrid) []AblResult {
	type scored struct {
		expect []string
		ll     *laneLists
	}
	var items []scored
	for _, c := range cases {
		if c.Unanswerable || c.Lane != "mem_inject" {
			continue
		}
		ll, err := recordLaneLists(ctx, deps, c.Query, grid.PoolCap)
		if err != nil || ll == nil {
			continue
		}
		items = append(items, scored{expect: c.ExpectIDs, ll: ll})
	}
	if len(items) == 0 {
		return nil // no applicable cases: an empty grid report is noise
	}
	var out []AblResult
	for _, k := range grid.RRFK {
		for _, topK := range grid.TopK {
			for _, alpha := range grid.Alphas {
				r := AblResult{RRFK: k, TopK: topK, Alpha: alpha, Cases: len(items), Production: k == 60 && topK == 10 && alpha == 0.75}
				var recip float64
				for _, it := range items {
					rank := 0
					ids := fuseRRF(it.ll, k)
					if alpha > 0 {
						ids = applyFloor(ids, it.ll.cosine, alpha)
					}
					if len(ids) > topK {
						ids = ids[:topK]
					}
					for i, id := range ids {
						if contains(it.expect, strconv.FormatInt(id, 10)) {
							rank = i + 1
							break
						}
					}
					if rank == 1 {
						r.Hit1++
					}
					if rank >= 1 && rank <= 3 {
						r.Hit3++
					}
					if rank >= 1 && rank <= 5 {
						r.Hit5++
					}
					if rank > 0 {
						recip += 1 / float64(rank)
					}
				}
				if r.Cases > 0 {
					r.MRR = recip / float64(r.Cases)
				}
				out = append(out, r)
			}
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].MRR != out[b].MRR {
			return out[a].MRR > out[b].MRR
		}
		if out[a].Hit3 != out[b].Hit3 {
			return out[a].Hit3 > out[b].Hit3
		}
		return out[a].RRFK < out[b].RRFK
	})
	return out
}

// applyFloor mirrors tools.applyRelativeFloor: drop hits whose cosine is
// below alpha × the best cosine in the set; hits without a score survive.
func applyFloor(ids []int64, cosine map[int64]float64, alpha float64) []int64 {
	if len(ids) <= 1 || alpha <= 0 || alpha >= 1 {
		return ids
	}
	best := 0.0
	for _, id := range ids {
		if c, ok := cosine[id]; ok && c > best {
			best = c
		}
	}
	if best <= 0 {
		return ids
	}
	floor := alpha * best
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if c, ok := cosine[id]; !ok || c >= floor {
			out = append(out, id)
		}
	}
	return out
}
