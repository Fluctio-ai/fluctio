package recallcheck

import (
	"context"
	"fmt"
	"strconv"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
	"github.com/fluctio-ai/fluctio/internal/embedding"
	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// Lane thresholds mirror the auto-recall defaults (kb hook + [MEM] lane).
// Kept as constants here — the point of the harness is comparing runs
// against each other, so the runner pins its knobs rather than drifting
// with config.
const (
	wikiLimit      = 5
	wikiThreshold  = 0.45
	ftLimit        = 5
	ftThreshold    = 0.6
	memLimit       = 3
	bookmarkLimit  = 5
	bookmarkThresh = 0.45
)

// Deps wires the runner to one agent's recall chain. Embedder may be nil —
// vector lanes then report "degraded" and only lexical behavior is covered.
type Deps struct {
	DB       *store.DBStore
	KB       *kb.KBStore
	AgentID  string
	Embedder embedding.Embedder
}

// CaseResult is one replayed case: the ids the lane actually surfaced, in
// order, plus the 1-based rank of the first expected id (0 = miss).
type CaseResult struct {
	Case      GoldenCase `json:"case"`
	Hits      []string   `json:"hits,omitempty"`
	Rank      int        `json:"rank,omitempty"`
	Violation bool       `json:"violation,omitempty"`
	Err       string     `json:"err,omitempty"`
}

// LaneStat aggregates one lane's run.
type LaneStat struct {
	Cases      int     `json:"cases"`
	Hit1       int     `json:"hit1"`
	Hit3       int     `json:"hit3"`
	Hit5       int     `json:"hit5"`
	RecipSum   float64 `json:"-"` // MRR numerator
	MRR        float64 `json:"mrr"`
	Violations int     `json:"violations"`
}

func (s *LaneStat) add(rank int, violated bool) {
	s.Cases++
	if violated {
		s.Violations++
		return
	}
	if rank == 1 {
		s.Hit1++
	}
	if rank >= 1 && rank <= 3 {
		s.Hit3++
	}
	if rank >= 1 && rank <= 5 {
		s.Hit5++
	}
	if rank > 0 {
		s.RecipSum += 1 / float64(rank)
	}
	s.MRR = s.RecipSum / float64(s.Cases)
}

// Report is the full run output: per-case results plus per-lane stats.
type Report struct {
	AgentID string              `json:"agent_id"`
	Cases   []CaseResult        `json:"cases"`
	ByLane  map[string]*LaneStat `json:"by_lane"`
	// Degraded is true when no embedder was wired — vector lanes ran
	// lexical-only and the numbers must not be compared with a semantic run.
	Degraded bool `json:"degraded,omitempty"`
}

// Run replays every case through its lane's production entry point and
// returns the report. Recall behavior is never modified — the runner only
// reads (InsertRecallDebugEvent is best-effort observability; SemanticMemRecall
// is called with record=false so replays never pollute the audit stream).
func Run(ctx context.Context, deps Deps, cases []GoldenCase) *Report {
	rep := &Report{AgentID: deps.AgentID, ByLane: map[string]*LaneStat{}}
	rep.Degraded = deps.Embedder == nil || !deps.Embedder.Available()
	laneStat := func(lane string) *LaneStat {
		s, ok := rep.ByLane[lane]
		if !ok {
			s = &LaneStat{}
			rep.ByLane[lane] = s
		}
		return s
	}
	for _, c := range cases {
		res := CaseResult{Case: c}
		if c.Unanswerable {
			hits := runAllLanes(ctx, deps, c.Query)
			res.Hits = hits
			res.Violation = len(hits) > 0
			laneStat("unanswerable").add(0, res.Violation)
		} else {
			hits, err := runLane(ctx, deps, c.Lane, c.Query)
			if err != nil {
				res.Err = err.Error()
			} else {
				res.Hits = hits
				for i, id := range hits {
					if contains(c.ExpectIDs, id) {
						res.Rank = i + 1
						break
					}
				}
			}
			laneStat(c.Lane).add(res.Rank, false)
		}
		rep.Cases = append(rep.Cases, res)
	}
	return rep
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// runLane dispatches to the lane's production entry point, returning the
// surfaced ids in rank order.
func runLane(ctx context.Context, deps Deps, lane, query string) ([]string, error) {
	switch lane {
	case "wiki":
		results, err := deps.KB.SearchSplit(ctx, deps.AgentID, query, wikiLimit, wikiThreshold, 0, ftThreshold)
		return kbResultIDs(results, "wiki", err)
	case "flash_todo":
		results, err := deps.KB.SearchSplit(ctx, deps.AgentID, query, 0, wikiThreshold, ftLimit, ftThreshold)
		return kbResultIDs(results, "kb", err)
	case "bookmark":
		results, err := deps.KB.Search(ctx, deps.AgentID, query, bookmarkLimit, 30, 0.5, bookmarkThresh)
		if err != nil {
			return nil, err
		}
		var ids []string
		for _, r := range results {
			if r.ContentType == "bookmark" {
				ids = append(ids, r.SourceID)
			}
		}
		return ids, nil
	case "mem_inject":
		hits, err := tools.SemanticMemRecall(ctx, deps.DB, deps.Embedder, nil, deps.AgentID, "", query, memLimit, nil, false)
		if err != nil {
			return nil, err
		}
		ids := make([]string, len(hits))
		for i, h := range hits {
			ids[i] = strconv.FormatInt(h.ID, 10)
		}
		return ids, nil
	default:
		return nil, fmt.Errorf("unknown lane %q", lane)
	}
}

// kbResultIDs flattens SearchSplit results of the wanted kind ("wiki" pages
// or "kb" flash/todo sources) into id lists.
func kbResultIDs(results []kb.KBResult, wantKind string, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, r := range results {
		if r.SourceKind == wantKind {
			ids = append(ids, r.SourceID)
		}
	}
	return ids, nil
}

// runAllLanes runs every lane (unanswerable traps) and concatenates hits.
func runAllLanes(ctx context.Context, deps Deps, query string) []string {
	var all []string
	for _, lane := range []string{"wiki", "flash_todo", "bookmark", "mem_inject"} {
		if ids, err := runLane(ctx, deps, lane, query); err == nil {
			all = append(all, ids...)
		}
	}
	return all
}
