package setup

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fluctio-ai/fluctio/internal/auth"
	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/push"
	"github.com/fluctio-ai/fluctio/internal/store"
)

type savePushDeviceReq struct {
	Token       string `json:"token"`
	Platform    string `json:"platform"`
	Environment string `json:"environment"`
	BundleID    string `json:"bundleId"`
}

func (s *Server) handleSavePushDevice(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "read-only"})
		return
	}
	if s.dataStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "store not configured"})
		return
	}
	var req savePushDeviceReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "token required"})
		return
	}
	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	if platform == "" {
		platform = "ios"
	}
	if platform != "ios" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unsupported platform"})
		return
	}
	rec := &store.PushDeviceRecord{
		UserID:      ident.EffectiveUserID(),
		Token:       token,
		Platform:    platform,
		Environment: strings.ToLower(strings.TrimSpace(req.Environment)),
		BundleID:    strings.TrimSpace(req.BundleID),
	}
	if err := s.dataStore.SavePushDevice(r.Context(), rec); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeletePushDevice(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "read-only"})
		return
	}
	if s.dataStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "store not configured"})
		return
	}
	token := strings.TrimSpace(r.PathValue("token"))
	if token == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "token required"})
		return
	}
	if err := s.dataStore.DeletePushDevice(r.Context(), ident.EffectiveUserID(), token); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// chatReplyThrottle coalesces a multi-bubble turn's SendMessage fanout:
// one queue row per (agent, session) per window, or a 3-bubble reply
// rings three times.
var chatReplyThrottle = struct {
	sync.Mutex
	last map[string]time.Time
}{last: map[string]time.Time{}}

const chatReplyThrottleWindow = time.Minute

func (s *Server) handleWebPushOutbound(msg bus.OutboundMessage) {
	if s.dataStore == nil {
		return
	}
	if msg.Channel != "web" || strings.TrimSpace(msg.AgentID) == "" || strings.TrimSpace(msg.ChatID) == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	userID, err := s.dataStore.LookupSessionOwner(ctx, msg.AgentID, msg.ChatID)
	if err != nil || userID == "" {
		slog.Debug("push: session owner not found", "agent", msg.AgentID, "session", msg.ChatID, "error", err)
		return
	}

	title := "Fluctio"
	if rec, err := s.dataStore.GetAgent(ctx, msg.AgentID); err == nil && rec != nil && strings.TrimSpace(rec.Name) != "" {
		title = strings.TrimSpace(rec.Name)
	}

	// Notification queue (Android direct delivery, no FCM): enqueue even
	// without APNs — self-hosted single-user deployments have no APNs keys,
	// and the app's poll/SSE drains this. Throttled per session so a turn's
	// bubble burst lands as one ring.
	s.enqueueChatReply(ctx, msg, userID, title)

	if s.pushClient == nil || !s.pushClient.Enabled() {
		return
	}
	devices, err := s.dataStore.ListPushDevices(ctx, userID)
	if err != nil {
		slog.Warn("push: list devices failed", "user", userID, "error", err)
		return
	}
	if len(devices) == 0 {
		return
	}

	for _, d := range devices {
		if d.Platform != "" && d.Platform != "ios" {
			continue
		}
		if err := s.pushClient.Send(ctx, push.Notification{
			DeviceToken: d.Token,
			Title:       title,
			Body:        "你有一条新的 Agent 消息",
			AgentID:     msg.AgentID,
			SessionID:   msg.ChatID,
		}); err != nil {
			slog.Warn("push: APNs send failed", "user", userID, "error", err)
		}
	}
}

// enqueueChatReply best-effort writes one chat_reply row into the
// notification queue. The body carries the reply's first line — the queue
// has none of APNs' fixed-copy limits, so use them.
func (s *Server) enqueueChatReply(ctx context.Context, msg bus.OutboundMessage, userID, title string) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}
	key := msg.AgentID + "\x00" + msg.ChatID
	chatReplyThrottle.Lock()
	if time.Since(chatReplyThrottle.last[key]) < chatReplyThrottleWindow {
		chatReplyThrottle.Unlock()
		return
	}
	chatReplyThrottle.last[key] = time.Now()
	chatReplyThrottle.Unlock()

	line := text
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	if len(line) > 120 {
		line = line[:120] + "…"
	}
	if _, err := s.dataStore.SaveNotification(ctx, &store.NotificationRecord{
		UserID:   userID,
		AgentID:  msg.AgentID,
		Kind:     "chat_reply",
		Title:    title,
		Body:     line,
		DeepLink: "/agents/" + msg.AgentID + "/chat/" + msg.ChatID,
	}); err != nil {
		slog.Warn("push: enqueue chat reply failed", "agent", msg.AgentID, "error", err)
	}
}
