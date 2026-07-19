package setup

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/embedding"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// handleRecallFeedback records a thumbs-up/down against a recall_id and
// then checks whether accumulated feedback now justifies promoting the
// agent's MMR lambda (stage 2b bandit upgrade). The recall_id — minted
// by memory_search when it surfaces summaries — is the only routing key:
// the server resolves the agent from the recall event, so the client
// cannot forge an agent_id.
func (s *Server) handleRecallFeedback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RecallID string `json:"recall_id"`
		Up       bool   `json:"up"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if req.RecallID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "recall_id is required"})
		return
	}

	db, ok := s.dataStore.(*store.DBStore)
	if !ok || db == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "store not available"})
		return
	}

	ctx := r.Context()
	agentID, err := db.GetRecallEventAgentID(ctx, req.RecallID)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if agentID == "" {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown recall_id"})
		return
	}

	if err := db.InsertRecallFeedback(ctx, req.RecallID, req.Up); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Best-effort upgrade: feedback is recorded even if the upgrade check
	// errors, so a transient failure can't lose the signal.
	upgraded, lambda, err := db.TryUpgradeLambda(ctx, agentID)
	if err != nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "recorded": true, "upgrade_error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok": true, "recorded": true,
		"upgraded": upgraded, "lambda": lambda,
	})
}

// handleGetRecallTuning returns the agent's current bandit state for the
// memory-tuning panel: current MMR lambda, recall counts (total + how
// many were ε-greedy explorations), and per-lambda feedback stats. Makes
// the otherwise-black-box λ optimization observable.
func (s *Server) handleGetRecallTuning(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	db, ok := s.dataStore.(*store.DBStore)
	if !ok || db == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "store not available"})
		return
	}
	ctx := r.Context()
	lambda, err := db.GetAgentMMRLambda(ctx, id)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	minRelevance, err := db.GetAgentMinRelevance(ctx, id)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	stats, err := db.GetRecallStats(ctx, id)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	fbStats, err := db.GetLambdaFeedbackStats(ctx, id)
	if err != nil {
		fbStats = nil // best-effort: feedback stats are non-critical for display
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":               true,
		"mmr_lambda":       lambda,
		"min_relevance":    minRelevance,
		"total_recalls":    stats.TotalRecalls,
		"explored_recalls": stats.ExploredRecalls,
		"feedback_stats":   fbStats,
	})
}

// handleRecallTest runs a recall preview for the tuning panel's test box.
// When the agent has embedding enabled it reproduces the full memory_search
// path (FTS + vector recall + MMR with the agent's current lambda) so the
// user can see the lambda's diversity effect. Otherwise it falls back to a
// basic FTS + scoring preview.
func (s *Server) handleRecallTest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	var req struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "query is required"})
		return
	}
	db, ok := s.dataStore.(*store.DBStore)
	if !ok || db == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "store not available"})
		return
	}
	ctx := r.Context()
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}

	// Full path when embedding is configured + reachable.
	var mem config.MemoryCfg
	if err := scope.SettingInto(ctx, db, "memory", s.effectiveUserID(r), id, &mem); err == nil && mem.Embedding.Enabled {
		emb := embedding.ProbeEmbedder(ctx, embedding.NewOpenAICompatEmbedder(
			mem.Embedding.APIBase, mem.Embedding.APIKey, mem.Embedding.Model, mem.Embedding.Dim, mem.Embedding.DimEnabled))
		if emb.Available() {
			hits, err := db.SearchConversationSummariesFTS(ctx, id, req.Query, limit*3)
			if err == nil {
				if vecs, e := emb.Embed(ctx, []string{req.Query}); e == nil && len(vecs) == 1 {
					// Apply the same min_relevance vector-distance filter the
					// live memory_search tool uses, so the test box reflects reality.
					minRel, _ := db.GetAgentMinRelevance(ctx, id)
					if vecScored, ve := db.SearchConversationSummariesVectorScored(ctx, vecs[0], limit*3); ve == nil && len(vecScored) > 0 {
						var scopedIDs []int64
						for _, sh := range vecScored {
							if minRel > 0 && (1/(1+sh.Distance)) < minRel {
								continue
							}
							scopedIDs = append(scopedIDs, sh.ID)
						}
						if len(scopedIDs) > 0 {
							if vecHits, fe := db.GetConversationSummariesByIDs(ctx, scopedIDs); fe == nil {
								hits = mergeRecallPool(hits, vecHits, id, limit*3)
							}
						}
					}
					if len(hits) > limit {
						if embMap, ee := db.GetConversationSummaryEmbeddings(ctx, recallHitIDs(hits)); ee == nil && len(embMap) >= limit {
							lambda, _ := db.GetAgentMMRLambda(ctx, id)
							if mmr := tools.SelectMMR(hits, embMap, vecs[0], lambda, limit); len(mmr) >= limit {
								hits = mmr
							}
						}
					}
				}
			}
			jsonResponse(w, http.StatusOK, map[string]any{
				"ok": true, "results": formatRecallHits(hits), "mode": "full",
			})
			return
		}
	}

	// Fallback: basic FTS + scoring (no vector/MMR).
	hits, err := db.PreviewRecall(ctx, id, req.Query, limit)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok": true, "results": formatRecallHits(hits), "mode": "basic",
		"note": "embedding disabled; excludes vector recall and MMR",
	})
}

// mergeRecallPool dedups FTS + vector hits and re-scopes the global vector
// results to this agent (vec0 KNN is global). Mirrors memory_search's merge.
func mergeRecallPool(fts, vec []store.ConversationSummary, agentID string, limit int) []store.ConversationSummary {
	seen := make(map[int64]bool)
	var out []store.ConversationSummary
	for _, h := range fts {
		if !seen[h.ID] {
			seen[h.ID] = true
			out = append(out, h)
		}
	}
	for _, h := range vec {
		if h.AgentID == agentID && !seen[h.ID] {
			seen[h.ID] = true
			out = append(out, h)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func recallHitIDs(hits []store.ConversationSummary) []int64 {
	ids := make([]int64, len(hits))
	for i, h := range hits {
		ids[i] = h.ID
	}
	return ids
}

func formatRecallHits(hits []store.ConversationSummary) []map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, map[string]any{
			"id": h.ID, "summary": h.Summary, "topic": h.Topic,
			"keywords": h.Keywords, "created_at": h.CreatedAt,
			"importance": h.Importance, "access_count": h.AccessCount,
		})
	}
	return out
}

// handlePutRecallTuning lets the owner manually set the agent's MMR lambda.
// This overrides the bandit's current best as a fresh starting point — the
// bandit keeps exploring from there and may promote again once new feedback
// accumulates (i.e. a manual nudge, not a hard lock).
func (s *Server) handlePutRecallTuning(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	var req struct {
		MmrLambda   *float64 `json:"mmr_lambda,omitempty"`
		MinRelevance *float64 `json:"min_relevance,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if req.MmrLambda != nil && (*req.MmrLambda < 0 || *req.MmrLambda > 1) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "mmr_lambda must be in [0,1]"})
		return
	}
	if req.MinRelevance != nil && (*req.MinRelevance < 0 || *req.MinRelevance > 1) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "min_relevance must be in [0,1]"})
		return
	}
	db, ok := s.dataStore.(*store.DBStore)
	if !ok || db == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "store not available"})
		return
	}
	if req.MmrLambda != nil {
		if err := db.SetAgentMMRLambda(r.Context(), id, *req.MmrLambda); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}
	if req.MinRelevance != nil {
		if err := db.SetAgentMinRelevance(r.Context(), id, *req.MinRelevance); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "mmr_lambda": req.MmrLambda, "min_relevance": req.MinRelevance})
}

// handleListRecallEvents returns the agent's recent recalls with summary
// previews, for the tuning panel's manual-feedback section (👍/👎).
func (s *Server) handleListRecallEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	db, ok := s.dataStore.(*store.DBStore)
	if !ok || db == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "store not available"})
		return
	}
	ctx := r.Context()
	events, err := db.ListRecentRecallEvents(ctx, id, 20)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// Collect summary ids, fetch once, build a lookup.
	idSet := map[int64]bool{}
	for _, ev := range events {
		for _, sid := range ev.SummaryIDs {
			idSet[sid] = true
		}
	}
	ids := make([]int64, 0, len(idSet))
	for sid := range idSet {
		ids = append(ids, sid)
	}
	sumMap := map[int64]store.ConversationSummary{}
	if len(ids) > 0 {
		if sums, err := db.GetConversationSummariesByIDs(ctx, ids); err == nil {
			for _, sm := range sums {
				sumMap[sm.ID] = sm
			}
		}
	}
	out := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		sums := make([]map[string]any, 0, len(ev.SummaryIDs))
		for _, sid := range ev.SummaryIDs {
			if sm, ok := sumMap[sid]; ok {
				sums = append(sums, map[string]any{"id": sm.ID, "summary": sm.Summary, "topic": sm.Topic})
			}
		}
		out = append(out, map[string]any{
			"recall_id": ev.RecallID, "lambda": ev.Lambda, "explored": ev.Explored,
			"created_at": ev.CreatedAt, "summaries": sums,
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "events": out})
}
