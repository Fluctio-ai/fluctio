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

// runLLMCallDiagRetention periodically prunes llm_call_diag older than the
// retention window. Diagnostic rows are short-lived — they exist to attribute
// recent failures, not as a permanent record (billing lives in token_usage_log,
// which this never touches). Probes at boot so a backlog clears immediately,
// then on an hourly cadence. retentionHours<=0 disables.
// See specs/2026-07-22-llm-call-observability.md.
func (g *Gateway) runLLMCallDiagRetention(ctx context.Context) {
	hours := llmCallDiagRetentionHours()
	if hours <= 0 {
		slog.Info("llm_call_diag retention disabled")
		return
	}
	slog.Info("llm_call_diag retention started", "retention_hours", hours, "interval", time.Hour)
	const interval = time.Hour
	g.pruneLLMCallDiagOnce(ctx, hours)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.pruneLLMCallDiagOnce(ctx, hours)
		}
	}
}

// pruneLLMCallDiagOnce runs one pruning pass and logs the outcome. Best-effort:
// a DB hiccup is logged, not fatal — the next tick retries. Reaches
// PruneLLMCallDiag via a *DBStore type assertion (same pattern as the session
// events sweep).
func (g *Gateway) pruneLLMCallDiagOnce(ctx context.Context, hours int) {
	db, ok := g.store.(*store.DBStore)
	if !ok || db == nil {
		return
	}
	before := time.Now().Add(-time.Duration(hours) * time.Hour)
	n, err := db.PruneLLMCallDiag(ctx, before, 1000)
	if err != nil {
		slog.Warn("llm_call_diag retention prune", "error", err)
		return
	}
	if n > 0 {
		slog.Info("llm_call_diag pruned",
			"deleted", n, "older_than", before.Format(time.RFC3339))
	}
}

// llmCallDiagRetentionHours reads FLUCTIO_LLM_CALL_DIAG_RETENTION_HOURS
// (default 72 = 3 days; 0 disables). Returns -1 on parse error so the sweep
// stays disabled rather than running with a bogus window.
func llmCallDiagRetentionHours() int {
	v := strings.TrimSpace(os.Getenv("FLUCTIO_LLM_CALL_DIAG_RETENTION_HOURS"))
	if v == "" {
		return 72
	}
	h, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("llm_call_diag retention: invalid env, disabling",
			"env", "FLUCTIO_LLM_CALL_DIAG_RETENTION_HOURS", "value", v)
		return -1
	}
	return h
}
