package gateway

import (
	"context"
	"log/slog"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// enqueueNotification best-effort writes one row into the notification
// queue (mobile direct-delivery channel, drained by the Android app's
// polling/SSE — no FCM). The owner is resolved via LookupSessionOwner:
// single-user flatten made every agent owner-addressed, and APNs routing
// already resolves the same way. Returns false (and logs) on failure so
// callers can treat "notified" independently of IM delivery.
//
// Delivery model is cursor+delete: the app polls GET
// /api/notifications?since=<id> and acks a watermark (POST
// /api/notifications/ack) which deletes up to it. Consequence: a second
// device that hasn't polled inside the ack window misses the row. That
// race is accepted — notifications are morning-digest flavor, low-stakes,
// and every kind is also archived into the web session
// (archivePushNotice), so the bubble is the durable copy.
func (g *Gateway) enqueueNotification(ctx context.Context, dbs *store.DBStore, ag store.AgentRecord, sessionKey, kind, title, body, deepLink string) bool {
	uid, err := dbs.LookupSessionOwner(ctx, ag.ID, sessionKey)
	if err != nil || uid == "" {
		slog.Warn("notification enqueue: no owner", "agent", ag.ID, "error", err)
		return false
	}
	if _, err := dbs.SaveNotification(ctx, &store.NotificationRecord{
		UserID:   uid,
		AgentID:  ag.ID,
		Kind:     kind,
		Title:    title,
		Body:     body,
		DeepLink: deepLink,
	}); err != nil {
		slog.Warn("notification enqueue failed", "agent", ag.ID, "kind", kind, "error", err)
		return false
	}
	return true
}
