package kb

import (
	"context"
	"encoding/json"
	"log/slog"
)

// Recall funnel tracing — pure observability for the KB recall lanes
// (wiki / flash_todo / bookmark). Each vector search fills one laneTrace:
// how many candidates it scanned, how many each gate dropped, and which
// hits survived with their pre-gate cosine rank/score. SearchSplit writes
// trigger="auto", Search writes trigger="tool". The point is answering
// "why was this recalled / why was this missed" from data instead of
// intuition (the 2026-10 [MEM] lane regression was found by hand).
//
// kb must not import the store package (the agent layer wires the two), so
// this writes recall_debug_events through the shared *sql.DB handle — the
// table itself is migrated by store.migrateRecallDebugEvents at boot. Keep
// the column list in sync with store.recallDebugColumns. Writes are
// best-effort: a missing table (unit tests) or a locked DB downgrades to a
// debug log, never a recall failure.

// traceHit mirrors store.RecallDebugHit.
type traceHit struct {
	ID      string             `json:"id"`
	Score   float64            `json:"score"`
	HitType string             `json:"hit_type"`
	Ranks   map[string]int     `json:"ranks,omitempty"`
	Scores  map[string]float64 `json:"scores,omitempty"`
}

// laneTrace accumulates one lane's funnel for a single recall call.
type laneTrace struct {
	candidates int
	gates      map[string]int
	params     map[string]any
	hits       []traceHit
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

// insertRecallTrace persists one lane's funnel to recall_debug_events.
// Best-effort: errors are logged at debug level and swallowed.
func (s *KBStore) insertRecallTrace(ctx context.Context, agentID, lane, trigger, query string, tr *laneTrace) {
	if tr == nil {
		return
	}
	marshal := func(v any) string {
		if v == nil {
			return ""
		}
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
	q := `INSERT INTO recall_debug_events (agent_id, lane, trigger_kind, query, candidates, gates, params, hits) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if s.dialect == "postgres" {
		q = `INSERT INTO recall_debug_events (agent_id, lane, trigger_kind, query, candidates, gates, params, hits) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	}
	if _, err := s.db.ExecContext(ctx, q, agentID, lane, trigger, query, tr.candidates, marshal(tr.gates), marshal(tr.params), marshal(tr.hits)); err != nil {
		slog.Debug("kb recall trace insert failed", "lane", lane, "err", err)
	}
}
