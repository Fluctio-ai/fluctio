package setup

import (
	"encoding/json"
	"net/http"
	"strings"

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
		"total_recalls":    stats.TotalRecalls,
		"explored_recalls": stats.ExploredRecalls,
		"feedback_stats":   fbStats,
	})
}

// handleRecallTest runs a basic recall preview (FTS + scoring) for the
// tuning panel's test box. Owner-only. NOTE: excludes vector recall,
// reranker, and MMR — a coverage preview, not a lambda-effect test.
func (s *Server) handleRecallTest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.requireAgentOwner(w, r, id) == nil {
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
	hits, err := db.PreviewRecall(r.Context(), id, req.Query, req.Limit)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, map[string]any{
			"id": h.ID, "summary": h.Summary, "topic": h.Topic,
			"keywords": h.Keywords, "created_at": h.CreatedAt,
			"importance": h.Importance, "access_count": h.AccessCount,
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok": true, "results": out,
		"note": "basic recall preview; excludes vector recall, reranker, and MMR",
	})
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
		MmrLambda float64 `json:"mmr_lambda"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if req.MmrLambda < 0 || req.MmrLambda > 1 {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "mmr_lambda must be in [0,1]"})
		return
	}
	db, ok := s.dataStore.(*store.DBStore)
	if !ok || db == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "store not available"})
		return
	}
	if err := db.SetAgentMMRLambda(r.Context(), id, req.MmrLambda); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "mmr_lambda": req.MmrLambda})
}
