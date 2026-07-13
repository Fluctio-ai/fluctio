package setup

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// handleCreateIMClaim mints a one-time verification code so the agent
// owner can bind their IM platform ID via `/claim <code>`. The caller is
// authenticated as the agent owner (requireAgentOwner); rec.UserID is the
// owner UUID stamped on the claim row.
func (s *Server) handleCreateIMClaim(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	var req struct {
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if !isIMChannel(req.Channel) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "IM channel required (e.g. wechat, discord, telegram)"})
		return
	}
	code, err := s.dataStore.CreateIMClaim(r.Context(), id, req.Channel, rec.UserID, store.IMClaimIntentAdd)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"code":      code,
		"channel":   req.Channel,
		"expiresAt": time.Now().Add(store.IMClaimTTL),
	})
}

// handleGetIMClaim returns the active (unused, unexpired) claim for a
// channel, if any. The web UI uses it to show the pending code + countdown.
func (s *Server) handleGetIMClaim(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	channel := r.PathValue("channel")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	active, err := s.dataStore.GetActiveIMClaim(r.Context(), id, channel)
	if err != nil {
		if err == store.ErrNotFound {
			jsonResponse(w, http.StatusOK, map[string]any{"active": false})
			return
		}
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"active":    true,
		"code":      active.Code,
		"channel":   channel,
		"expiresAt": active.ExpiresAt,
	})
}

// handleUnbindIM removes one claimed admin platform ID for a channel. If it
// was the last one the channel goes fail-closed (no admin) — flagged via
// lastUnbind so the UI can warn that IM write access is lost until re-claim.
func (s *Server) handleUnbindIM(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	var req struct {
		Channel    string `json:"channel"`
		PlatformID string `json:"platformId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.Channel == "" || req.PlatformID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "channel + platformId required"})
		return
	}
	cfg, err := s.loadAgentFileConfig(r, id)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var kept []string
	for _, x := range cfg.Admins[req.Channel] {
		if x != req.PlatformID {
			kept = append(kept, x)
		}
	}
	if len(kept) == 0 {
		delete(cfg.Admins, req.Channel)
	} else {
		cfg.Admins[req.Channel] = kept
	}
	if err := s.saveAgentFileConfig(r, id, cfg); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"channel":    req.Channel,
		"remaining":  len(kept),
		"lastUnbind": len(kept) == 0,
	})
}

// handleRebindIM is the one-click rebind: void ALL admin IDs for the
// channel (a stolen/retired account loses admin power instantly — this is
// the security value of rebind over add) then mint a fresh claim code so
// the owner can bind a new account.
func (s *Server) handleRebindIM(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	var req struct {
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if !isIMChannel(req.Channel) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "IM channel required"})
		return
	}
	cfg, err := s.loadAgentFileConfig(r, id)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if cfg.Admins != nil {
		delete(cfg.Admins, req.Channel)
	}
	if err := s.saveAgentFileConfig(r, id, cfg); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	code, err := s.dataStore.CreateIMClaim(r.Context(), id, req.Channel, rec.UserID, store.IMClaimIntentReplace)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"code":      code,
		"channel":   req.Channel,
		"expiresAt": time.Now().Add(store.IMClaimTTL),
	})
}

// isIMChannel rejects the web/api pseudo-channels (those carry fluctio
// UUIDs directly and never need a claim) plus the empty string.
func isIMChannel(channel string) bool {
	return channel != "" && channel != "web" && channel != "api"
}
