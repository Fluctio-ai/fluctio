package gateway

import (
	"context"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// runSessionEventsRetention prunes session_events older than the retention
// window. session_events is the in-flight event stream a client replays to
// resume a turn mid-stream; once a turn is done the events have no replay
// value (history lives in session_messages), so old rows are safe to delete.
// FLUCTIO_SESSION_EVENTS_RETENTION_HOURS (default 168 = 7 days; <=0 disables).
// See specs/2026-07-22-session-events-retention.md.
func (g *Gateway) runSessionEventsRetention(ctx context.Context) {
	g.runHourlyRetention(ctx, "session_events",
		readRetentionHours("FLUCTIO_SESSION_EVENTS_RETENTION_HOURS", 168),
		func(ctx context.Context, db *store.DBStore, before time.Time) (int64, error) {
			return db.PruneSessionEvents(ctx, before, 1000)
		})
}
