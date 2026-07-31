package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/embedding"
	"github.com/fluctio-ai/fluctio/internal/memoryindex"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// --- /api/agents/{id}/memory (GET/PUT) ---
//
// Reads/writes the agent-scope "memory" override row (MemoryCfg JSON).
// The web Wiki page uses these to persist memory.wikiAutoGen (the background
// auto-generation toggle + interval). Single-user target: no owner check.

func (s *Server) handleGetAgentMemory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var mem config.MemoryCfg
	if s.dataStore != nil {
		_ = scope.SettingInto(r.Context(), s.dataStore, "memory", "", id, &mem)
	}
	writeJSON(w, http.StatusOK, map[string]any{"memory": mem})
}

func (s *Server) handleUpdateAgentMemory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	mem, _ := body["memory"].(map[string]any)
	if err := scope.SaveSetting(r.Context(), s.dataStore, "", id, "memory", mem); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- /api/memory/test-embedding + /api/memory/test-reranker ---
//
// These take the form's inline credentials (not the saved row) so an
// operator can verify a half-entered config before saving — mirrors the
// Models page's /api/test-provider. A short request-scoped timeout keeps
// a hung endpoint from wedging the call.

type testEmbeddingRequest struct {
	APIBase    string `json:"apiBase"`
	APIKey     string `json:"apiKey"`
	Model      string `json:"model"`
	Dim        int    `json:"dim"`
	DimEnabled bool   `json:"dimEnabled"`
}

type testRerankerRequest struct {
	APIBase string `json:"apiBase"`
	APIKey  string `json:"apiKey"`
	Model   string `json:"model"`
}

func (s *Server) handleTestEmbedding(w http.ResponseWriter, r *http.Request) {
	var req testEmbeddingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	emb := embedding.NewOpenAICompatEmbedder(req.APIBase, req.APIKey, req.Model, req.Dim, req.DimEnabled)
	if !emb.Available() {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "apiBase and apiKey are required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	vecs, err := emb.Embed(ctx, []string{"hello world"})
	if err != nil || len(vecs) != 1 {
		msg := "embedding call failed"
		if err != nil {
			msg = err.Error()
		}
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": msg})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "dim": len(vecs[0])})
}

func (s *Server) handleTestReranker(w http.ResponseWriter, r *http.Request) {
	var req testRerankerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	rr := embedding.NewJinaReranker(req.APIBase, req.APIKey, req.Model)
	if !rr.Available() {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "apiBase and apiKey are required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	scored, err := rr.Rerank(ctx, "hello", []string{"hello world", "totally unrelated"}, 1)
	if err != nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "results": len(scored)})
}

// --- /api/agents/{id}/memory/reindex (POST) ---
//
// Force re-vectorize every conversation summary for one agent: clears
// the agent's existing vectors, re-embeds all summaries, returns counts.
// Owner-only. Runs synchronously but the per-call pacing keeps it from
// hammering the embedding API; for huge backlogs an operator should
// rely on the background loop instead.
func (s *Server) handleReindexAgentMemory(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	db, ok := s.dataStore.(*store.DBStore)
	if !ok || db == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "vector store not available"})
		return
	}

	// Resolve the agent's effective embedding config (system→owner→agent).
	var vec config.VectorCfg
	if err := scope.SettingInto(r.Context(), db, "vectorization", s.effectiveUserID(r), id, &vec); err != nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if !vec.Embedding.Enabled {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "embedding not enabled for this agent"})
		return
	}
	ec := vec.Embedding
	emb := embedding.NewOpenAICompatEmbedder(ec.APIBase, ec.APIKey, ec.Model, ec.Dim, ec.DimEnabled)
	if !emb.Available() {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "apiBase and apiKey are required"})
		return
	}

	// Generous timeout — a full re-embed of many summaries + per-call
	// pacing can take a while.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	res, err := memoryindex.Reindex(ctx, db, emb, id, true, 200*time.Millisecond)
	if err != nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":        true,
		"processed": res.Processed,
		"failed":    res.Failed,
	})
}
