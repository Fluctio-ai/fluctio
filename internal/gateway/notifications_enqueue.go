package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/channels"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// The shared delivery policy of the four push sweeps (due todos, cards
// digest, shelved ledger, diary blindspots): the IM leg is optional — a
// missing/unbound channel never silences the push — the notification
// queue (mobile direct delivery, drained by the Android app's poll/SSE)
// always runs, and the session archive rides the IM session key. Either
// channel delivering is the caller's stamp gate.

// sweepOwnerUID resolves the notification queue's addressee once per
// sweep iteration — LookupSessionOwner is deployment-global under the
// single-user flatten (it ignores agentID/sessionKey), so callers hoist
// it out of per-item loops instead of paying a query per todo.
func sweepOwnerUID(ctx context.Context, dbs *store.DBStore, ag store.AgentRecord) string {
	uid, err := dbs.LookupSessionOwner(ctx, ag.ID, "")
	if err != nil || uid == "" {
		slog.Warn("notification enqueue: no owner", "agent", ag.ID, "error", err)
		return ""
	}
	return uid
}

// resolveSweepTarget resolves the optional IM leg: the channel name
// (empty → wechat) — returned normalized so callers can stamp it —, its
// registered instance (nil + warn when unbound or unregistered — the
// sweep proceeds queue-only), and the target session key for the archive
// leg (empty when no deliverable target).
func (g *Gateway) resolveSweepTarget(ctx context.Context, dbs *store.DBStore, agentID, channel, sweep string) (channels.Channel, string, string, string) {
	if channel == "" {
		channel = "wechat"
	}
	accountID, chatID, sessionKey, hasTarget := deliverableTargetForAgent(ctx, dbs, agentID, channel)
	if !hasTarget {
		return nil, "", "", channel
	}
	ch := g.chanMgr.Get(channel, accountID)
	if ch == nil {
		slog.Warn(sweep+": channel not registered", "agent", agentID, "channel", channel, "account", accountID)
	}
	return ch, chatID, sessionKey, channel
}

// deliverSweepNudge runs the shared policy block: IM best-effort, queue
// enqueue, archive-if-sessionKey. Returns whether any channel delivered.
// kind is the notification queue's kind; archiveKind is the session
// archive's tag (deliberately distinct for the blindspot sweep).
func (g *Gateway) deliverSweepNudge(ctx context.Context, dbs *store.DBStore, ag store.AgentRecord, uid string, ch channels.Channel, chatID, sessionKey, kind, title, body, deepLink, archiveKind, sweep string) bool {
	delivered := false
	if ch != nil {
		if err := ch.Send(chatID, body); err != nil {
			slog.Warn(sweep+" push failed", "agent", ag.ID, "error", err)
		} else {
			delivered = true
		}
	}
	if g.enqueueFor(ctx, dbs, ag, uid, kind, title, body, deepLink) {
		delivered = true
	}
	if delivered && sessionKey != "" {
		if err := archivePushNotice(ctx, dbs, ag.ID, sessionKey, body, archiveKind); err != nil {
			// A channel already delivered — log only: losing the web-side
			// bubble beats double-sending.
			slog.Warn(sweep+" push: archive notice", "agent", ag.ID, "error", err)
		}
	}
	return delivered
}

// enqueueFor writes one row into the notification queue for uid.
// Cursor+delete delivery model: the app polls GET /api/notifications?
// since=<id> and acks a watermark (POST /api/notifications/ack) which
// deletes up to it. Consequence: a second device that hasn't polled
// inside the ack window misses the row — accepted, the session archive
// is the durable copy.
func (g *Gateway) enqueueFor(ctx context.Context, dbs *store.DBStore, ag store.AgentRecord, uid, kind, title, body, deepLink string) bool {
	if uid == "" {
		return false // sweepOwnerUID already warned
	}
	// Per-agent kind gate: a disabled kind must not even cost a queue row
	// (the app would otherwise still ring — the drain has no other filter).
	if !notifyCfgFor(ag).KindEnabled(kind) {
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

// notifyCfgFor reads the agent's notification toggles from its config
// blob (AgentRecord.Config["notifications"]), mirroring reminderChannelFor:
// the field round-trips through interface{}, so marshal+unmarshal
// normalizes it into config.AgentNotifyCfg.
func notifyCfgFor(ag store.AgentRecord) *config.AgentNotifyCfg {
	cfgAny, ok := ag.Config["notifications"]
	if !ok || cfgAny == nil {
		return nil // nil receiver = every kind ON
	}
	var cfg config.AgentNotifyCfg
	if b, err := json.Marshal(cfgAny); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	return &cfg
}

// pushTimePassed reports whether today's HH:MM push gate (empty → 09:00,
// UTC+8) has passed — the sweeps' shared "land in the morning, not at
// boot" check. A malformed time parses to zero, which passes (fail open).
func pushTimePassed(pt string, nowCST time.Time) bool {
	pt = strings.TrimSpace(pt)
	if pt == "" {
		pt = "09:00"
	}
	pushAt := parseCronTimeToday(pt, nowCST)
	return pushAt.IsZero() || !nowCST.Before(pushAt)
}
