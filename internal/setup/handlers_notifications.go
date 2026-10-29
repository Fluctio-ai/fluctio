package setup

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Notifications are the mobile direct-delivery channel (Gotify-style, no
// FCM): the proactive sweeps enqueue rows, and the Android app drains
// them two ways — backgrounded, it polls handleNotificationsList on a
// WorkManager cadence; foregrounded, it holds handleNotificationsStream
// open for sub-second delivery. Delivery is cursor+delete: acking a
// watermark deletes up to it, so `since` cursors live on the client.

// sinceQuery parses the ?since= cursor (0 when absent/malformed).
func sinceQuery(r *http.Request) int64 {
	if v, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64); err == nil {
		return v
	}
	return 0
}

// handleNotificationsList is the polling endpoint: pending rows with
// id > since, oldest first, plus lastId (max id in the page, == since when
// empty) so the client advances its cursor in one round-trip. limit is
// clamped server-side (default 50, max 200 — pass 0 for the default).
func (s *Server) handleNotificationsList(w http.ResponseWriter, r *http.Request) {
	uid := s.effectiveUserID(r)
	if uid == "" {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	if s.dataStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "no data store"})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.dataStore.ListNotifications(r.Context(), uid, sinceQuery(r), limit)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	lastID := sinceQuery(r)
	if len(rows) > 0 {
		lastID = rows[len(rows)-1].ID
	}
	jsonResponse(w, http.StatusOK, map[string]any{"notifications": rows, "lastId": lastID})
}

// handleNotificationsAck takes a cursor watermark — the client has fully
// rendered/notified everything up to lastId — and deletes those rows.
func (s *Server) handleNotificationsAck(w http.ResponseWriter, r *http.Request) {
	uid := s.effectiveUserID(r)
	if uid == "" {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	if s.dataStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "no data store"})
		return
	}
	var body struct {
		LastID int64 `json:"lastId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	if body.LastID < 0 {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "lastId must be >= 0"})
		return
	}
	if err := s.dataStore.AckNotificationsTo(r.Context(), uid, body.LastID); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleNotificationsStream is the foreground SSE channel. Notifications
// arrive at human pace (a handful per day), so instead of a fanout hub the
// stream polls the queue on a 5s tick — one indexed query per open client,
// zero cross-package wiring, and it keeps working unchanged under
// multi-replica deploys. Frames follow the same shape as the chat stream:
// `id:<nid>` + `data:{notification json}`; `: ping` keeps intermediaries
// from idling the connection out every 30s. The first poll runs before
// the loop so backlog past `since` replays on connect, matching the
// contract doc.
func (s *Server) handleNotificationsStream(w http.ResponseWriter, r *http.Request) {
	uid := s.effectiveUserID(r)
	if uid == "" {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	if s.dataStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "no data store"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	// Initial flush so the client fires `open` immediately.
	fmt.Fprintf(w, ": ok\n\n")
	flusher.Flush()

	since := sinceQuery(r)
	poll := func() {
		rows, err := s.dataStore.ListNotifications(r.Context(), uid, since, 200)
		if err != nil {
			slog.Warn("notifications stream poll", "user", uid, "error", err)
			return
		}
		for _, n := range rows {
			b, _ := json.Marshal(n) // all-string/int fields; cannot fail
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", n.ID, b)
			since = n.ID
		}
		flusher.Flush()
	}
	poll()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case <-tick.C:
			poll()
		}
	}
}
