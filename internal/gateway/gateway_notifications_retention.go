package gateway

import (
	"context"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// runNotificationsRetention prunes notification-queue rows older than the
// retention window. Delivery is cursor+delete, so this only bites when the
// app hasn't drained its queue (unopened for days) — acked rows are
// already gone. FLUCTIO_NOTIFICATIONS_RETENTION_HOURS (default 168 = 7
// days; <=0 disables).
func (g *Gateway) runNotificationsRetention(ctx context.Context) {
	g.runHourlyRetention(ctx, "notifications",
		readRetentionHours("FLUCTIO_NOTIFICATIONS_RETENTION_HOURS", 168),
		func(ctx context.Context, db *store.DBStore, before time.Time) (int64, error) {
			return db.PruneNotifications(ctx, before)
		})
}
