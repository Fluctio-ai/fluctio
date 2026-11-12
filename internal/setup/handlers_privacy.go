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
// piiScrubbing.enabled / piiScrubbing.entropy). Gated by the caller's
// agent ACL via requireAgentOwner: this row controls whether PII is
// scrubbed for the agent, so a shared/ACL'd apikey must not be able to
// flip it for an agent outside its grant (and the system-wide switch is
// admin-gated below).

func (s *Server) handleGetAgentPrivacy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	var priv config.PrivacyCfg
	if s.dataStore != nil {
		_ = scope.SettingInto(r.Context(), s.dataStore, "privacy", "", id, &priv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"privacy": priv})
}

func (s *Server) handleUpdateAgentPrivacy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	priv, ok := s.decodePrivacyBody(w, r)
	if !ok {
		return
	}
	if err := scope.SaveSetting(r.Context(), s.dataStore, "", id, "privacy", priv); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// The scrub gates are stamped at agent build time (see the manager's
	// privacy stamp); without a rebuild the toggle silently waits for
	// the next restart. Drop every UserSpace holding this agent so the
	// next access rebuilds with the new row.
	s.invalidateAgent(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- /api/privacy (GET/PUT) — system-level defaults ---
//
// Reads/writes the system-scope "privacy" row (agentID=""). Agents inherit
// these via scope.Setting merge when they don't override. PUT is gated to
// platform admins: the row is the global PII-scrubbing kill switch, and
// plain auth let any logged-in account turn scrubbing off for everyone.

func (s *Server) handleGetSystemPrivacy(w http.ResponseWriter, r *http.Request) {
	var priv config.PrivacyCfg
	if s.dataStore != nil {
		_ = scope.SettingInto(r.Context(), s.dataStore, "privacy", "", "", &priv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"privacy": priv})
}

func (s *Server) handleUpdateSystemPrivacy(w http.ResponseWriter, r *http.Request) {
	if ident, ok := authIdentity(r); !ok || !ident.CanAdminPlatform() {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "platform admin required to change system privacy settings"})
		return
	}
	priv, ok := s.decodePrivacyBody(w, r)
	if !ok {
		return
	}
	if err := scope.SaveSetting(r.Context(), s.dataStore, "", "", "privacy", priv); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// The system row shadows every agent — drop every cached UserSpace
	// (invalidateScope's System arm) so the toggle takes effect
	// without a daemon restart.
	s.invalidateScope(scope.System, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// decodePrivacyBody decodes a privacy PUT body and extracts the
// "privacy" object. Empty data means "delete row" in SaveSetting, so a
// `{}` body would silently reset the row — require the section
// explicitly. Returns (nil, false) after writing the error response.
func (s *Server) decodePrivacyBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return nil, false
	}
	priv, ok := body["privacy"].(map[string]any)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": `body must contain a "privacy" object`})
		return nil, false
	}
	return priv, true
}
