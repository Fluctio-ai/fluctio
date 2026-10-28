package gateway

import (
	"context"
	"log/slog"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// runHardcaseMiningSweep aggregates zero-hit recall events into the
// hardcase review queue. Interval comes from
// FLUCTIO_RECALL_HARDCASE_MINING_HOURS (default 24 = daily; the value is
// both the tick interval and the first-run lookback; <=0 disables).
func (g *Gateway) runHardcaseMiningSweep(ctx context.Context) {
	hours := readRetentionHours("FLUCTIO_RECALL_HARDCASE_MINING_HOURS", 24)
	if hours <= 0 {
		return
	}
	g.runEvery(ctx, time.Duration(hours)*time.Hour, g.mineHardcases)
}

func (g *Gateway) mineHardcases(ctx context.Context) {
	db, ok := g.store.(*store.DBStore)
	if !ok || db == nil {
		return
	}
	hours := readRetentionHours("FLUCTIO_RECALL_HARDCASE_MINING_HOURS", 24)
	n, err := db.MineHardcases(ctx, time.Duration(hours)*time.Hour)
	if err != nil {
		slog.Debug("hardcase mining", "error", err)
		return
	}
	if n > 0 {
		slog.Info("hardcase mining", "groups", n)
	}
}
