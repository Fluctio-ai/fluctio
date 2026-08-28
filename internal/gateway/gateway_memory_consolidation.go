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

// runMemoryConsolidation runs the deterministic memory-consolidation sweep
// (supersede purge, stale-episodic eviction, per-agent quota) on a daily
// cadence. Probes at boot so a long-standby instance clears its backlog
// immediately. Disabled when FLUCTIO_MEMORY_CONSOLIDATION_HOURS<=0
// (default 24). Pure pruning, no LLM — the knowledge lifecycle (value
// gate, supersedes) happens at write time.
//
// The MMR-lambda bandit stays frozen until explored recalls with feedback
// reach its 20-sample upgrade gate — consolidation deliberately doesn't
// touch tuning state.
func (g *Gateway) runMemoryConsolidation(ctx context.Context) {
	hours := memoryConsolidationHours()
	if hours <= 0 {
		slog.Info("memory consolidation disabled")
		return
	}
	slog.Info("memory consolidation started", "interval_hours", hours)
	g.runEvery(ctx, time.Duration(hours)*time.Hour, g.consolidateMemoriesOnce)
}

// consolidateMemoriesOnce runs one consolidation pass and logs the
// outcome. Best-effort: errors are logged, the next tick retries.
func (g *Gateway) consolidateMemoriesOnce(ctx context.Context) {
	db, ok := g.store.(*store.DBStore)
	if !ok || db == nil {
		return
	}
	stats, err := db.PruneConversationSummaries(ctx, store.DefaultMemoryConsolidationCfg)
	if err != nil {
		slog.Warn("memory consolidation pass", "error", err)
		return
	}
	if stats.SupersededPurged+stats.StaleEvicted+stats.QuotaEvicted > 0 {
		slog.Info("memory consolidated",
			"superseded_purged", stats.SupersededPurged,
			"stale_evicted", stats.StaleEvicted,
			"quota_evicted", stats.QuotaEvicted)
	}
}

// memoryConsolidationHours reads FLUCTIO_MEMORY_CONSOLIDATION_HOURS
// (default 24; 0 disables). Returns -1 on parse error so the sweep stays
// disabled rather than running with a bogus cadence.
func memoryConsolidationHours() int {
	v := strings.TrimSpace(os.Getenv("FLUCTIO_MEMORY_CONSOLIDATION_HOURS"))
	if v == "" {
		return 24
	}
	h, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("memory consolidation: invalid env, disabling",
			"env", "FLUCTIO_MEMORY_CONSOLIDATION_HOURS", "value", v)
		return -1
	}
	return h
}
