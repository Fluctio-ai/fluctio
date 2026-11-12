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
//
// API keys are masked on the way out and never overwritten by masked or
// empty values on the way in (see mergeVectorKeys): the config row used
// to round-trip the plaintext key through the browser, leaking it to any
// authenticated caller and letting a stale/masked save silently destroy
// the stored secret.

func (s *Server) handleGetAgentVectorization(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	var vec config.VectorCfg
	if s.dataStore != nil {
		_ = scope.SettingInto(r.Context(), s.dataStore, "vectorization", id, &vec)
	}
	maskVectorKeys(&vec)
	writeJSON(w, http.StatusOK, map[string]any{"vectorization": vec})
}

func (s *Server) handleUpdateAgentVectorization(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// A missing/non-object "vectorization" key must be a 400: SaveSetting
	// treats empty data as a row delete, so `{"vectorization": null}`
	// would silently wipe the agent's embedding config.
	vec, ok := body["vectorization"].(map[string]any)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": `body must contain a "vectorization" object`})
		return
	}
	if s.dataStore != nil {
		var stored config.VectorCfg
		_ = scope.SettingInto(r.Context(), s.dataStore, "vectorization", id, &stored)
		mergeVectorKeys(vec, stored)
	}
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
		_ = scope.SettingInto(r.Context(), s.dataStore, "vectorization", "", &vec)
	}
	maskVectorKeys(&vec)
	writeJSON(w, http.StatusOK, map[string]any{"vectorization": vec})
}

func (s *Server) handleUpdateSystemVectorization(w http.ResponseWriter, r *http.Request) {
	if ident, ok := authIdentity(r); !ok || !ident.CanAdminPlatform() {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "platform admin required to change system vectorization settings"})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// Same guard as the agent-scope PUT: an absent/non-object section
	// would delete the system vectorization row instead of erroring.
	vec, ok := body["vectorization"].(map[string]any)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": `body must contain a "vectorization" object`})
		return
	}
	if s.dataStore != nil {
		var stored config.VectorCfg
		_ = scope.SettingInto(r.Context(), s.dataStore, "vectorization", "", &stored)
		mergeVectorKeys(vec, stored)
	}
	if err := scope.SaveSetting(r.Context(), s.dataStore, "", "", "vectorization", vec); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// maskVectorKeys masks the embedding/reranker API keys on a VectorCfg
// bound for the client. The stored row keeps the plaintext.
func maskVectorKeys(vec *config.VectorCfg) {
	vec.Embedding.APIKey = maskAPIKey(vec.Embedding.APIKey)
	vec.Reranker.APIKey = maskAPIKey(vec.Reranker.APIKey)
}

// mergeVectorKeys applies "masked/empty = keep stored" semantics for the
// embedding/reranker API keys on an inbound update body. The client works
// against masked values (see maskVectorKeys), so an untouched form save
// must not clobber the real key with "****" — and a truly new key arrives
// as a non-masked plaintext string.
func mergeVectorKeys(incoming map[string]any, stored config.VectorCfg) {
	mergeCfgKey := func(m map[string]any, key string, storedKey string) {
		if m == nil {
			return
		}
		cfg, _ := m[key].(map[string]any)
		if cfg == nil {
			return
		}
		cur, _ := cfg["apiKey"].(string)
		if cur == "" || cur == "****" || cur == maskAPIKey(storedKey) {
			if storedKey != "" {
				cfg["apiKey"] = storedKey
			} else {
				delete(cfg, "apiKey")
			}
		}
	}
	mergeCfgKey(incoming, "embedding", stored.Embedding.APIKey)
	mergeCfgKey(incoming, "reranker", stored.Reranker.APIKey)
}
