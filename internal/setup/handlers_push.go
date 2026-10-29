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
	"github.com/fluctio-ai/fluctio/internal/config"
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

// handleWebPushOutbound is the web-channel entry: WebChannel invokes it
// only when a (agent, session) reply has no live SSE subscriber, so the
// "someone is watching" check is already done upstream. Session key ==
// ChatID on this channel.
func (s *Server) handleWebPushOutbound(msg bus.OutboundMessage) {
	if s.dataStore == nil || msg.Channel != "web" {
		return
	}
	if strings.TrimSpace(msg.AgentID) == "" || strings.TrimSpace(msg.ChatID) == "" {
		return
	}
	s.pushReply(msg, msg.ChatID, "chat_reply")
}

// imChannels is the allowlist of chat surfaces whose replies are mirrored
// into the mobile queue. Deliberately explicit: plugin channels, "api"
// (programmatic completions) and "heartbeat" are not conversations the
// phone should ring for.
var imChannels = map[string]bool{
	"telegram": true, "wechat": true, "qq": true,
	"discord": true, "slack": true, "feishu": true, "line": true,
}

// OutboundObserver is the callback the composition root installs as the
// channel manager's outbound observer (channels.Manager.SetOutboundObserver).
// One entry point so the dispatch goroutine never blocks on a store write.
func (s *Server) OutboundObserver(msg bus.OutboundMessage) {
	go s.handleIMPushOutbound(msg)
}

// handleIMPushOutbound mirrors an IM conversation's reply into the
// notification queue so the phone is told even though the conversation
// lives in a chat app. Suppressed when the user has that same session
// open in the dashboard/app (an SSE subscriber exists) — reading it there
// already counts as "seen".
//
// Outbound bus messages carry the IM chat id, not the session key, so the
// key is resolved from the sessions table: it is both the deep-link target
// and the suppression check's key. An unresolvable chat (no session row
// yet) falls back to skipping — there is nothing to link to.
func (s *Server) handleIMPushOutbound(msg bus.OutboundMessage) {
	if s.dataStore == nil || !imChannels[msg.Channel] {
		return
	}
	if strings.TrimSpace(msg.AgentID) == "" || strings.TrimSpace(msg.ChatID) == "" {
		return
	}
	if strings.TrimSpace(msg.Text) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sessionKey, err := s.dataStore.SessionKeyForChat(ctx, msg.AgentID, msg.Channel, msg.AccountID, msg.ChatID)
	if err != nil || sessionKey == "" {
		slog.Debug("push: IM session not resolvable", "agent", msg.AgentID,
			"channel", msg.Channel, "chat", msg.ChatID, "error", err)
		return
	}
	if s.webChan != nil && s.webChan.SubscriberCount(msg.AgentID, sessionKey) > 0 {
		return // the user is looking at this conversation right now
	}
	s.pushReply(msg, sessionKey, "im_reply")
}

// pushReply runs the shared delivery core for one conversation reply:
// owner + agent-name lookup, per-kind config gate, throttled queue
// enqueue, then APNs when configured. sessionKey is the dashboard/app
// session key (== ChatID on the web channel).
func (s *Server) pushReply(msg bus.OutboundMessage, sessionKey, kind string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	userID, err := s.dataStore.LookupSessionOwner(ctx, msg.AgentID, sessionKey)
	if err != nil || userID == "" {
		slog.Debug("push: session owner not found", "agent", msg.AgentID, "session", sessionKey, "error", err)
		return
	}

	title := "Fluctio"
	var notifyCfg *config.AgentNotifyCfg
	if rec, err := s.dataStore.GetAgent(ctx, msg.AgentID); err == nil && rec != nil {
		if strings.TrimSpace(rec.Name) != "" {
			title = strings.TrimSpace(rec.Name)
		}
		if cfgAny, ok := rec.Config["notifications"]; ok && cfgAny != nil {
			var nc config.AgentNotifyCfg
			if b, merr := json.Marshal(cfgAny); merr == nil && json.Unmarshal(b, &nc) == nil {
				notifyCfg = &nc
			}
		}
	}
	if !notifyCfg.KindEnabled(kind) {
		return
	}

	// Notification queue (Android direct delivery, no FCM): enqueue even
	// without APNs — self-hosted single-user deployments have no APNs keys,
	// and the app's poll/SSE drains this. Throttled per session so a turn's
	// bubble burst lands as one ring.
	s.enqueueChatReply(ctx, msg, sessionKey, userID, title, kind)

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
			SessionID:   sessionKey,
		}); err != nil {
			slog.Warn("push: APNs send failed", "user", userID, "error", err)
		}
	}
}

// enqueueChatReply best-effort writes one reply row into the notification
// queue. The body carries the reply's first line — the queue has none of
// APNs' fixed-copy limits, so use them.
func (s *Server) enqueueChatReply(ctx context.Context, msg bus.OutboundMessage, sessionKey, userID, title, kind string) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}
	key := msg.AgentID + "\x00" + sessionKey
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
		Kind:     kind,
		Title:    title,
		Body:     line,
		DeepLink: "/agents/" + msg.AgentID + "/chat/" + sessionKey,
	}); err != nil {
		slog.Warn("push: enqueue chat reply failed", "agent", msg.AgentID, "error", err)
	}
}
