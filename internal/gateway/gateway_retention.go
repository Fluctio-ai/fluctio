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

// runHourlyRetention is the shared loop for the single-table retention
// sweeps (session_events, llm_call_diag, memory_recall_events): probe at
// boot so a long-standby instance clears its backlog immediately, then an
// hourly tick. hours<=0 disables. prune runs one batched pass; a DB hiccup
// is logged, not fatal — the next tick retries. The *store.DBStore
// assertion lives here so each sweep's prune stays a one-liner (the Prune*
// methods are deliberately not on the store.Store interface, so no-op test
// stores don't have to stub them).
func (g *Gateway) runHourlyRetention(ctx context.Context, name string, hours int, prune func(ctx context.Context, db *store.DBStore, before time.Time) (int64, error)) {
	if hours <= 0 {
		slog.Info(name + " retention disabled")
		return
	}
	slog.Info(name+" retention started", "retention_hours", hours, "interval", time.Hour)
	sweep := func() {
		db, ok := g.store.(*store.DBStore)
		if !ok || db == nil {
			return
		}
		before := time.Now().Add(-time.Duration(hours) * time.Hour)
		n, err := prune(ctx, db, before)
		if err != nil {
			slog.Warn(name+" retention prune", "error", err)
			return
		}
		if n > 0 {
			slog.Info(name+" pruned",
				"deleted", n, "older_than", before.Format(time.RFC3339))
		}
	}
	sweep()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// readRetentionHours reads an hours-valued sweep env: empty = def, a bogus
// value disables (returns -1) rather than running with a garbage window,
// and <=0 disables that sweep.
func readRetentionHours(env string, def int) int {
	v := strings.TrimSpace(os.Getenv(env))
	if v == "" {
		return def
	}
	h, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("invalid env, disabling sweep", "env", env, "value", v)
		return -1
	}
	return h
}
