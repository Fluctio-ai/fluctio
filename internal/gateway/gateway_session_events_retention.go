package gateway

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// runSessionEventsRetention periodically prunes session_events older than the
// retention window. session_events is the in-flight event stream a client
// replays to resume a turn mid-stream; once a turn is done the events have no
// replay value (history lives in session_messages), so old rows are safe to
// delete. Probes at boot so a long-standby instance clears its backlog
// immediately, then on an hourly cadence. retentionHours<=0 disables.
// See specs/2026-07-22-session-events-retention.md.
func (g *Gateway) runSessionEventsRetention(ctx context.Context) {
	hours := sessionEventsRetentionHours()
	if hours <= 0 {
		slog.Info("session_events retention disabled")
		return
	}
	slog.Info("session_events retention started", "retention_hours", hours, "interval", time.Hour)
	const interval = time.Hour
	g.pruneSessionEventsOnce(ctx, hours)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.pruneSessionEventsOnce(ctx, hours)
		}
	}
}

// pruneSessionEventsOnce runs one pruning pass and logs the outcome. Best-effort:
// a DB hiccup is logged, not fatal — the next tick retries. Reaches
// PruneSessionEvents via a *DBStore type assertion (the method is deliberately
// not on the store.Store interface, so no-op test stores don't have to stub it).
func (g *Gateway) pruneSessionEventsOnce(ctx context.Context, hours int) {
	db, ok := g.store.(*store.DBStore)
	if !ok || db == nil {
		return
	}
	before := time.Now().Add(-time.Duration(hours) * time.Hour)
	n, err := db.PruneSessionEvents(ctx, before, 1000)
	if err != nil {
		slog.Warn("session_events retention prune", "error", err)
		return
	}
	if n > 0 {
		slog.Info("session_events pruned",
			"deleted", n, "older_than", before.Format(time.RFC3339))
	}
}

// sessionEventsRetentionHours reads FLUCTIO_SESSION_EVENTS_RETENTION_HOURS
// (default 168 = 7 days; 0 disables). Returns -1 on parse error so the sweep
// stays disabled rather than running with a bogus window.
func sessionEventsRetentionHours() int {
	v := strings.TrimSpace(os.Getenv("FLUCTIO_SESSION_EVENTS_RETENTION_HOURS"))
	if v == "" {
		return 168
	}
	h, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("session_events retention: invalid env, disabling",
			"env", "FLUCTIO_SESSION_EVENTS_RETENTION_HOURS", "value", v)
		return -1
	}
	return h
}
