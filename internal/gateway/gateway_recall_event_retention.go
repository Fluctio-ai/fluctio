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

// runRecallEventRetention periodically prunes memory_recall_events older
// than the retention window. Audit data only — the summaries themselves
// stay; the events exist so the recall-test page can show what was
// injected and carry 👍/👎. Volume grew once the [MEM] lane started
// recording an event per message, so an unbounded table was no longer
// acceptable. Probes at boot, then hourly. retentionHours<=0 disables.
func (g *Gateway) runRecallEventRetention(ctx context.Context) {
	hours := recallEventRetentionHours()
	if hours <= 0 {
		slog.Info("recall_events retention disabled")
		return
	}
	slog.Info("recall_events retention started", "retention_hours", hours, "interval", time.Hour)
	const interval = time.Hour
	g.pruneRecallEventsOnce(ctx, hours)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.pruneRecallEventsOnce(ctx, hours)
		}
	}
}

// pruneRecallEventsOnce runs one capped pass, best-effort like the
// session-events sweep (a DB hiccup is logged; the next tick retries).
func (g *Gateway) pruneRecallEventsOnce(ctx context.Context, hours int) {
	db, ok := g.store.(*store.DBStore)
	if !ok || db == nil {
		return
	}
	before := time.Now().Add(-time.Duration(hours) * time.Hour)
	n, err := db.PruneRecallEvents(ctx, before, 1000)
	if err != nil {
		slog.Warn("recall_events retention prune", "error", err)
		return
	}
	if n > 0 {
		slog.Info("recall_events pruned",
			"deleted", n, "older_than", before.Format(time.RFC3339))
	}
}

// recallEventRetentionHours reads FLUCTIO_RECALL_EVENTS_RETENTION_HOURS
// (default 2160 = 90 days; 0 disables). -1 on parse error keeps the sweep
// off rather than running with a bogus window.
func recallEventRetentionHours() int {
	v := strings.TrimSpace(os.Getenv("FLUCTIO_RECALL_EVENTS_RETENTION_HOURS"))
	if v == "" {
		return 2160
	}
	h, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("recall_events retention: invalid env, disabling",
			"env", "FLUCTIO_RECALL_EVENTS_RETENTION_HOURS", "value", v)
		return -1
	}
	return h
}
