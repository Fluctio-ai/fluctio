package setup

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// --- GET /api/chat/sessions/{key}/export?agentId= — raw events dump ---
//
// Downloads the session's session_events stream as unstyled JSON. This
// is a debugging primitive (modeled on Octop's /history/export): it
// separates "the record exists but the UI didn't render it" from "the
// record is gone", independent of the history projection the chat page
// reads. Not a backup — the full DB snapshot lives under /api/backup.
func (s *Server) handleSessionExport(w http.ResponseWriter, r *http.Request) {
	ag := s.resolveAgent(r, r.URL.Query().Get("agentId"))
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	if s.dataStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "no data store"})
		return
	}
	key := r.PathValue("key")
	// seq > -1 = every row for the session, oldest first.
	events, err := s.dataStore.ListSessionEventsSince(r.Context(), ag.Name(), key, -1)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if events == nil {
		events = []store.SessionEventRecord{}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition",
		"attachment; filename=\"session-"+url.PathEscape(key)+".json\"")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"agent":      ag.Name(),
		"sessionKey": key,
		"exportedAt": time.Now().UTC().Format(time.RFC3339),
		"eventCount": len(events),
		"events":     events,
	})
}
