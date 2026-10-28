package gateway

import (
	"context"
	"log/slog"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// runHardcaseMiningSweep aggregates zero-hit recall events into the
// hardcase review queue. Runs hourly; the mining query is a single grouped
// scan over the retention window, and the watermark in recall_sweep_meta
// makes re-runs idempotent. Disabled when
// FLUCTIO_RECALL_HARDCASE_MINING_HOURS<=0.
func (g *Gateway) runHardcaseMiningSweep(ctx context.Context) {
	const interval = time.Hour
	g.mineHardcases(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.mineHardcases(ctx)
		}
	}
}

func (g *Gateway) mineHardcases(ctx context.Context) {
	// Env kill switch, consistent with the other sweeps:
	// FLUCTIO_RECALL_HARDCASE_MINING_HOURS<=0 disables (default on).
	if readRetentionHours("FLUCTIO_RECALL_HARDCASE_MINING_HOURS", 24) <= 0 {
		return
	}
	db, ok := g.store.(*store.DBStore)
	if !ok || db == nil {
		return
	}
	n, err := db.MineHardcases(ctx, 24*time.Hour)
	if err != nil {
		slog.Debug("hardcase mining", "error", err)
		return
	}
	if n > 0 {
		slog.Info("hardcase mining", "groups", n)
	}
}
