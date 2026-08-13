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

// runWorkflowRetention periodically prunes finished workflow_runs past their
// retention window (spec decision 11): succeeded runs older than the success
// window, and failed / needs_intervention runs older than the (longer) failed
// window. Probes at boot so a long-standby instance clears its backlog
// immediately, then on an hourly cadence. Mirrors runSessionEventsRetention.
// Either env ≤ 0 disables pruning for that state; both ≤ 0 disables the sweep
// entirely.
func (g *Gateway) runWorkflowRetention(ctx context.Context) {
	successH, failedH := workflowRetentionHours()
	if successH <= 0 && failedH <= 0 {
		slog.Info("workflow retention disabled",
			"success_hours", successH, "failed_hours", failedH)
		return
	}
	slog.Info("workflow retention started",
		"success_hours", successH, "failed_hours", failedH, "interval", time.Hour)
	const interval = time.Hour
	g.pruneWorkflowRunsOnce(ctx, successH, failedH)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.pruneWorkflowRunsOnce(ctx, successH, failedH)
		}
	}
}

// pruneWorkflowRunsOnce runs one pruning pass and logs the outcome. Best-effort:
// a DB hiccup is logged, not fatal — the next tick retries. A zero cutoff for a
// state (env ≤ 0) skips that state. Reaches PruneWorkflowRuns via a *DBStore
// type assertion (the method is deliberately not on the store.Store interface,
// so no-op test stores don't have to stub it).
func (g *Gateway) pruneWorkflowRunsOnce(ctx context.Context, successHours, failedHours int) {
	db, ok := g.store.(*store.DBStore)
	if !ok || db == nil {
		return
	}
	var successBefore, failedBefore time.Time
	if successHours > 0 {
		successBefore = time.Now().Add(-time.Duration(successHours) * time.Hour)
	}
	if failedHours > 0 {
		failedBefore = time.Now().Add(-time.Duration(failedHours) * time.Hour)
	}
	if successBefore.IsZero() && failedBefore.IsZero() {
		return
	}
	n, err := db.PruneWorkflowRuns(ctx, successBefore, failedBefore, 1000)
	if err != nil {
		slog.Warn("workflow retention prune", "error", err)
		return
	}
	if n > 0 {
		slog.Info("workflow runs pruned", "deleted", n)
	}
}

// workflowRetentionHours reads the two retention envs:
//   - FLUCTIO_WORKFLOW_RETENTION_SUCCESS_HOURS (default 168 = 7d)
//   - FLUCTIO_WORKFLOW_RETENTION_FAILED_HOURS  (default 720 = 30d)
//
// ≤ 0 disables that state's pruning. A bogus value disables (returns -1)
// rather than running with a garbage window.
func workflowRetentionHours() (success, failed int) {
	return readRetentionHours("FLUCTIO_WORKFLOW_RETENTION_SUCCESS_HOURS", 168),
		readRetentionHours("FLUCTIO_WORKFLOW_RETENTION_FAILED_HOURS", 720)
}

func readRetentionHours(env string, def int) int {
	v := strings.TrimSpace(os.Getenv(env))
	if v == "" {
		return def
	}
	h, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("workflow retention: invalid env, disabling",
			"env", env, "value", v)
		return -1
	}
	return h
}
