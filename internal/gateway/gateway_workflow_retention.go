package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// runWorkflowRetention periodically prunes finished workflow_runs past their
// retention window (spec decision 11): succeeded runs older than the success
// window, and failed / needs_intervention runs older than the (longer) failed
// window. It also flips two non-terminal stragglers: waiting runs past the M6
// form timeout become needs_intervention, and 'running' runs whose last sign
// of life is older than the stale window become failed (crash orphans — the
// only way a failed run reaches the resume path). Probes at boot so a
// long-standby instance clears its backlog immediately, then on an hourly
// cadence. Mirrors runSessionEventsRetention. Either env ≤ 0 disables that
// action; all four ≤ 0 disables the sweep entirely.
func (g *Gateway) runWorkflowRetention(ctx context.Context) {
	successH, failedH := workflowRetentionHours()
	formH := workflowFormTimeoutHours()
	staleH := workflowStaleRunningHours()
	if successH <= 0 && failedH <= 0 && formH <= 0 && staleH <= 0 {
		slog.Info("workflow retention disabled",
			"success_hours", successH, "failed_hours", failedH, "form_timeout_hours", formH, "stale_running_hours", staleH)
		return
	}
	slog.Info("workflow retention started",
		"success_hours", successH, "failed_hours", failedH, "form_timeout_hours", formH, "stale_running_hours", staleH, "interval", time.Hour)
	const interval = time.Hour
	g.pruneWorkflowRunsOnce(ctx, successH, failedH, formH, staleH)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.pruneWorkflowRunsOnce(ctx, successH, failedH, formH, staleH)
		}
	}
}

// pruneWorkflowRunsOnce runs one pruning pass and logs the outcome. Best-effort:
// a DB hiccup is logged, not fatal — the next tick retries. A zero cutoff for a
// state (env ≤ 0) skips that state. Reaches PruneWorkflowRuns via a *DBStore
// type assertion (the method is deliberately not on the store.Store interface,
// so no-op test stores don't have to stub it). Besides pruning it also flips
// the two non-terminal stragglers: waiting runs past the M6 form timeout
// become needs_intervention (an unanswered form is a human decision, never a
// silent failure), and running runs whose last node output predates the stale
// window become failed — crash orphans, surfaced so the failed-run resume
// path can pick them up.
func (g *Gateway) pruneWorkflowRunsOnce(ctx context.Context, successHours, failedHours, formTimeoutHours, staleRunningHours int) {
	db, ok := g.store.(*store.DBStore)
	if !ok || db == nil {
		return
	}
	if formTimeoutHours > 0 {
		if n, err := db.TimeoutWaitingRuns(ctx, time.Now().Add(-time.Duration(formTimeoutHours)*time.Hour),
			fmt.Sprintf("form wait timed out after %dh (needs_intervention)", formTimeoutHours)); err != nil {
			slog.Warn("workflow form timeout", "error", err)
		} else if n > 0 {
			slog.Info("workflow form waits timed out", "marked_needs_intervention", n)
		}
	}
	if staleRunningHours > 0 {
		if n, err := db.FailStaleRunningRuns(ctx, time.Now().Add(-time.Duration(staleRunningHours)*time.Hour),
			fmt.Sprintf("runner lost after %dh without node progress (failed by stale sweep)", staleRunningHours)); err != nil {
			slog.Warn("workflow stale running sweep", "error", err)
		} else if n > 0 {
			slog.Info("workflow stale running runs failed", "marked_failed", n)
		}
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

// workflowFormTimeoutHours reads the M6 form-wait timeout env:
// FLUCTIO_WORKFLOW_FORM_TIMEOUT_HOURS (default 24). A waiting run older than
// the window is flipped to needs_intervention. ≤ 0 disables the timeout
// (a form may wait forever).
func workflowFormTimeoutHours() int {
	return readRetentionHours("FLUCTIO_WORKFLOW_FORM_TIMEOUT_HOURS", 24)
}

// workflowStaleRunningHours reads the crash-orphan reaper env:
// FLUCTIO_WORKFLOW_STALE_RUNNING_HOURS (default 24). A 'running' run whose
// newest node-output row is older than the window is flipped to failed so the
// failed-run resume path can pick it up. ≤ 0 disables the reaper.
func workflowStaleRunningHours() int {
	return readRetentionHours("FLUCTIO_WORKFLOW_STALE_RUNNING_HOURS", 24)
}
