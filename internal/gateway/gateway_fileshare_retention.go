package gateway

import (
	"context"
	"log/slog"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// runFileShareRetention prunes expired file_shares rows (public /f/{token}
// links past their expires_at). Unlike the other retention sweeps there is
// no config knob: expiry is per-row (chosen when each link was minted), the
// sweep just deletes what already declared itself dead. Permanent links
// (expires_at NULL) are untouched. Hourly is plenty — an expired link 404s
// correctly on read even before its row is pruned.
func (g *Gateway) runFileShareRetention(ctx context.Context) {
	g.runEvery(ctx, time.Hour, func(ctx context.Context) {
		// Deliberately a *DBStore assertion, not store.Store: prune is
		// maintenance SQL, not part of the data-access interface (same
		// call as PruneSessionEvents — see gateway_retention.go).
		db, ok := g.store.(*store.DBStore)
		if !ok || db == nil {
			return
		}
		n, err := db.PruneExpiredFileShares(ctx, time.Now().UTC(), 500)
		if err != nil {
			slog.Warn("file_shares retention prune", "error", err)
			return
		}
		if n > 0 {
			slog.Info("file_shares retention pruned", "deleted", n)
		}
	})
}
