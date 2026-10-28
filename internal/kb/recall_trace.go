package kb

import (
	"context"
	"math"
)

// Recall funnel tracing — pure observability for the KB recall lanes
// (wiki / flash_todo / bookmark). Each vector search fills one laneTrace:
// how many candidates it scanned, how many each gate dropped, and which
// hits survived with their pre-gate cosine rank/score. SearchSplit emits
// trigger="auto", Search emits trigger="tool". The point is answering
// "why was this recalled / why was this missed" from data instead of
// intuition (the 2026-10 [MEM] lane regression was found by hand).
//
// Gate names are consts, not literals: the gates column is a schemaless
// JSON map, so a typo at a call site would silently mint a new key in the
// very data this instrumentation exists to be trusted. Vocabulary:
// dim_mismatch (stale vector from another model), embed_failed /
// query_failed (lane died before scanning), pool_cap (truncated for the
// reranker), rerank_dropped (reranker didn't return it), below_threshold,
// over_limit.

const (
	gateDimMismatch = "dim_mismatch"
	gateEmbedFailed = "embed_failed"
	gateQueryFailed = "query_failed"
	gatePoolCap     = "pool_cap"
	gateRerankDrop  = "rerank_dropped"
	gateBelowThresh = "below_threshold"
	gateOverLimit   = "over_limit"
)

// round4 rounds to 4 decimals for JSON-stored scores. One helper so the
// precision convention lives in exactly one place.
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// TraceHit is one surfaced candidate with per-lane attribution. Ranks and
// Scores are keyed by retrieval lane ("vector", "rerank") so offline
// analysis can replay per-lane rankings without re-running retrieval.
type TraceHit struct {
	ID      string             `json:"id"`
	Score   float64            `json:"score"` // final score on the gate's scale
	HitType string             `json:"hit_type"`
	Ranks   map[string]int     `json:"ranks,omitempty"`
	Scores  map[string]float64 `json:"scores,omitempty"`
}

// TraceEvent is one lane's completed funnel, handed to the RecallTracer.
// AgentID/Lane/Trigger are filled by emitTrace; the rest by the search.
type TraceEvent struct {
	AgentID    string
	Lane       string
	Trigger    string
	Query      string
	Candidates int
	Gates      map[string]int
	Params     map[string]any
	Hits       []TraceHit
}

// RecallTracer receives completed funnels. Wired once by the agent layer
// to the store insert — kb must not import the store package (the agent
// wires the two, same boundary as MemoryAutoSearcher), so the recall_debug
// SQL has a single owner instead of two hand-synced INSERTs.
type RecallTracer func(ctx context.Context, ev TraceEvent)

// laneTrace accumulates one lane's funnel for a single recall call.
type laneTrace struct {
	candidates int
	gates      map[string]int
	params     map[string]any
	hits       []TraceHit
}

func newLaneTrace(threshold float64, limit int) *laneTrace {
	return &laneTrace{
		gates:  map[string]int{},
		params: map[string]any{"threshold": threshold, "limit": limit},
	}
}

// drop records n candidates dying at a gate. Nil- and zero-safe.
func (t *laneTrace) drop(gate string, n int) {
	if t == nil || n <= 0 {
		return
	}
	t.gates[gate] += n
}

// addHit appends one survivor's attribution. rrRank == 0 means no
// reranker ran; cosRank/cosScore are the pre-gate (pre-rerank) values.
func (t *laneTrace) addHit(id string, final, cos float64, cosRank, rrRank int) {
	if t == nil {
		return
	}
	h := TraceHit{
		ID:      id,
		Score:   round4(final),
		HitType: "vector",
		Ranks:   map[string]int{"vector": cosRank},
		Scores:  map[string]float64{"vector": round4(cos)},
	}
	if rrRank > 0 {
		h.HitType = "rerank"
		h.Ranks["rerank"] = rrRank
	}
	t.hits = append(t.hits, h)
}

// emitTrace hands one lane's funnel to the wired tracer. No tracer
// (transient stores, unit tests) or nil funnel = silent no-op, so
// observability can never break a recall. Hits rides the struct; the
// store's insert owns the JSON encoding and its empty-set guard, so an
// empty funnel lands as '' (the mining query treats '' / '[]' as
// zero-hit — "null" would not match).
func (s *KBStore) emitTrace(ctx context.Context, agentID, lane, trigger, query string, tr *laneTrace) {
	if tr == nil || s.recallTracer == nil {
		return
	}
	s.recallTracer(ctx, TraceEvent{
		AgentID: agentID, Lane: lane, Trigger: trigger, Query: query,
		Candidates: tr.candidates, Gates: tr.gates, Params: tr.params,
		Hits: tr.hits,
	})
}
