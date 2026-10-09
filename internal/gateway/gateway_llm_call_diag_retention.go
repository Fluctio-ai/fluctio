package gateway

import (
	"context"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// runLLMCallDiagRetention prunes llm_call_diag older than the retention
// window. Diagnostic rows are short-lived — they exist to attribute recent
// failures, not as a permanent record (billing lives in token_usage_log,
// which this never touches).
// FLUCTIO_LLM_CALL_DIAG_RETENTION_HOURS (default 72 = 3 days; <=0 disables).
// See specs/2026-07-22-llm-call-observability.md.
func (g *Gateway) runLLMCallDiagRetention(ctx context.Context) {
	g.runHourlyRetention(ctx, "llm_call_diag",
		readRetentionHours("FLUCTIO_LLM_CALL_DIAG_RETENTION_HOURS", 72),
		func(ctx context.Context, db *store.DBStore, before time.Time) (int64, error) {
			return db.PruneLLMCallDiag(ctx, before, 1000)
		})
}
