package gateway

import (
	"context"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// runRecallEventRetention prunes memory_recall_events older than the
// retention window. Audit data only — the summaries themselves stay; the
// events exist so the recall-test page can show what was injected and carry
// 👍/👎. Volume grew once the [MEM] lane started recording an event per
// message, so an unbounded table was no longer acceptable.
// FLUCTIO_RECALL_EVENTS_RETENTION_HOURS (default 2160 = 90 days; <=0 disables).
func (g *Gateway) runRecallEventRetention(ctx context.Context) {
	g.runHourlyRetention(ctx, "recall_events",
		readRetentionHours("FLUCTIO_RECALL_EVENTS_RETENTION_HOURS", 2160),
		func(ctx context.Context, db *store.DBStore, before time.Time) (int64, error) {
			return db.PruneRecallEvents(ctx, before, 1000)
		})
}
