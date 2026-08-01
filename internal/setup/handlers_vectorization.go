package setup

import (
	"encoding/json"
	"net/http"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/scope"
)

// --- /api/agents/{id}/vectorization (GET/PUT) ---
//
// Reads/writes the agent-scope "vectorization" override row (VectorCfg
// JSON: embedding / reranker / kbEmbedding / wikiEmbedding). Split from
// the "memory" config since KB and wiki also consume these settings —
// the "memory" namespace they used to live under was a misnomer.

func (s *Server) handleGetAgentVectorization(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var vec config.VectorCfg
	if s.dataStore != nil {
		_ = scope.SettingInto(r.Context(), s.dataStore, "vectorization", "", id, &vec)
	}
	writeJSON(w, http.StatusOK, map[string]any{"vectorization": vec})
}

func (s *Server) handleUpdateAgentVectorization(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	vec, _ := body["vectorization"].(map[string]any)
	if err := scope.SaveSetting(r.Context(), s.dataStore, "", id, "vectorization", vec); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- /api/vectorization (GET/PUT) — system-level defaults ---
//
// Reads/writes the system-scope "vectorization" row (agentID=""). Agents
// inherit these via scope.Setting merge when they don't override.
func (s *Server) handleGetSystemVectorization(w http.ResponseWriter, r *http.Request) {
	var vec config.VectorCfg
	if s.dataStore != nil {
		_ = scope.SettingInto(r.Context(), s.dataStore, "vectorization", "", "", &vec)
	}
	writeJSON(w, http.StatusOK, map[string]any{"vectorization": vec})
}

func (s *Server) handleUpdateSystemVectorization(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	vec, _ := body["vectorization"].(map[string]any)
	if err := scope.SaveSetting(r.Context(), s.dataStore, "", "", "vectorization", vec); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
