package setup

import (
	"encoding/json"
	"net/http"

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
