package setup

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/embedding"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// handleRecallFeedback records a thumbs-up/down against a recall_id and
// then checks whether accumulated feedback now justifies promoting the
// agent's MMR lambda (stage 2b bandit upgrade). Votes are always kept as
// audit state; only votes on ε-greedy-EXPLORED tool-lane recalls count
// toward λ (GetLambdaFeedbackStats filters explored=1) — the [MEM]
// injection lane has no λ, so its votes are pure audit marks. The
// recall_id is the only routing key: the server resolves the agent from
// the recall event, so the client cannot forge an agent_id.
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

// handleGetRecallTuning is the recall-test page's availability probe. The
// page renders no stats anymore (semantic floors, not knobs, decide
// relevance), so this only confirms the store is up — no COUNT/JOIN queries
// behind a probe.
func (s *Server) handleGetRecallTuning(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	if db, ok := s.dataStore.(*store.DBStore); !ok || db == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "store not available"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRecallTest previews what a message would inject by running the
// production [MEM] lane itself — tools.SemanticMemRecall with recording
// off — so the test box can never drift from what chat actually does
// (it used to be a third hand-copied pipeline: KNN + min_relevance + MMR,
// none of which the injection lane has). Rerank mirrors production: only
// when a reranker is configured AND kb.memoryRerank is on.
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

	// Same construction the agent manager does: probe the configured
	// embedder once, reranker only under the opt-in toggle.
	var emb embedding.Embedder
	semantic := false
	var vec config.VectorCfg
	if err := scope.SettingInto(ctx, db, "vectorization", s.effectiveUserID(r), id, &vec); err == nil && vec.Embedding.Enabled {
		emb = embedding.ProbeEmbedder(ctx, embedding.NewOpenAICompatEmbedder(
			vec.Embedding.APIBase, vec.Embedding.APIKey, vec.Embedding.Model, vec.Embedding.Dim, vec.Embedding.DimEnabled))
		semantic = emb.Available()
	}
	var rr embedding.Reranker
	if semantic && vec.Reranker.Enabled && agentKBRerankEnabled(rec) {
		rr = embedding.NewJinaReranker(vec.Reranker.APIBase, vec.Reranker.APIKey, vec.Reranker.Model)
	}

	hits, err := tools.SemanticMemRecall(ctx, db, emb, rr, id, s.effectiveUserID(r), req.Query, limit, false)
	if err != nil {
		if errors.Is(err, tools.ErrEmbeddingFailed) {
			// Embedding endpoint is down with vectorization on: say so —
			// don't silently preview keyword-only results as if the full
			// path ran.
			fts, ferr := db.PreviewRecall(ctx, id, req.Query, limit)
			if ferr != nil {
				jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": ferr.Error()})
				return
			}
			jsonResponse(w, http.StatusOK, map[string]any{
				"ok": true, "results": formatRecallHits(fts), "mode": "degraded",
				"note": "embedding call failed — semantic recall excluded (keyword-only preview): " + err.Error(),
			})
			return
		}
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if !semantic {
		jsonResponse(w, http.StatusOK, map[string]any{
			"ok": true, "results": formatRecallHits(hits), "mode": "basic",
			"note": "embedding disabled; plain FTS order, no semantic floors",
		})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok": true, "results": formatRecallHits(hits), "mode": "full",
	})
}

// agentKBRerankEnabled reads the agent's kb.memoryRerank toggle from its
// config blob (round-trip through JSON into the typed config, same pattern
// as the KB insight settings read).
func agentKBRerankEnabled(rec *store.AgentRecord) bool {
	if rec == nil || rec.Config == nil {
		return false
	}
	raw, ok := rec.Config["kb"]
	if !ok || raw == nil {
		return false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return false
	}
	var cfg config.AgentKBCfg
	if err := json.Unmarshal(b, &cfg); err != nil {
		return false
	}
	return cfg.MemoryRerank
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
		MmrLambda *float64 `json:"mmr_lambda,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if req.MmrLambda != nil && (*req.MmrLambda < 0 || *req.MmrLambda > 1) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "mmr_lambda must be in [0,1]"})
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
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "mmr_lambda": req.MmrLambda})
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
	// ?limit= caps the audit list (default 100 — the page filters by time
	// client-side too); ?days=0 shows all time, >0 only the last N days.
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}
	days := 0
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	events, err := db.ListRecentRecallEvents(ctx, id, limit, days)
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
				item := map[string]any{"id": sm.ID, "summary": sm.Summary, "topic": sm.Topic}
				// Per-hit relevance when recorded (vector path); absent on
				// FTS-only recalls.
				if s, ok := ev.Scores[sid]; ok {
					item["relevance"] = s
				}
				sums = append(sums, item)
			}
		}
		item := map[string]any{
			"recall_id": ev.RecallID, "lambda": ev.Lambda, "bandit_explored": ev.Explored,
			"query": ev.Query, "session_key": ev.SessionKey, "consumed": ev.Consumed,
			"created_at": ev.CreatedAt, "summaries": sums,
		}
		// Current vote state (latest 👍/👎), so the UI can mark voted rows.
		if ev.Vote != nil {
			if *ev.Vote {
				item["vote"] = "up"
			} else {
				item["vote"] = "down"
			}
		}
		out = append(out, item)
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "events": out})
}
