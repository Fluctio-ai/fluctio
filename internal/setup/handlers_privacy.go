package setup

import (
	"encoding/json"
	"net/http"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/scope"
)

// --- /api/agents/{id}/privacy (GET/PUT) ---
//
// Reads/writes the agent-scope "privacy" override row (PrivacyCfg JSON:
// piiScrubbing.enabled / piiScrubbing.entropy). Single-user target: no
// owner check — mirrors handleGetAgentMemory / handleGetAgentVectorization.

func (s *Server) handleGetAgentPrivacy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var priv config.PrivacyCfg
	if s.dataStore != nil {
		_ = scope.SettingInto(r.Context(), s.dataStore, "privacy", "", id, &priv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"privacy": priv})
}

func (s *Server) handleUpdateAgentPrivacy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	priv, _ := body["privacy"].(map[string]any)
	if err := scope.SaveSetting(r.Context(), s.dataStore, "", id, "privacy", priv); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- /api/privacy (GET/PUT) — system-level defaults ---
//
// Reads/writes the system-scope "privacy" row (agentID=""). Agents inherit
// these via scope.Setting merge when they don't override.

func (s *Server) handleGetSystemPrivacy(w http.ResponseWriter, r *http.Request) {
	var priv config.PrivacyCfg
	if s.dataStore != nil {
		_ = scope.SettingInto(r.Context(), s.dataStore, "privacy", "", "", &priv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"privacy": priv})
}

func (s *Server) handleUpdateSystemPrivacy(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	priv, _ := body["privacy"].(map[string]any)
	if err := scope.SaveSetting(r.Context(), s.dataStore, "", "", "privacy", priv); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
