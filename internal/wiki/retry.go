package wiki

import (
	"context"
	"log/slog"
	"time"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// InvokeWithRetry wraps an LLMInvoker with exponential-backoff retries so
// a flaky provider (rate limit, transient TCP reset, brief outage) doesn't
// immediately fail a wiki generation run.
//
// It tries up to maxAttempts times TOTAL (i.e. first try plus maxAttempts-1
// retries), waiting 2s, 4s, 8s… between attempts — the delay doubles each
// retry. The context is honored between attempts so a cancelled run exits
// promptly. On exhaustion it returns the last error from the invoke fn.
func InvokeWithRetry(
	ctx context.Context,
	invoke LLMInvoker,
	messages []provider.Message,
	maxAttempts int,
) (string, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := time.Duration(2<<(attempt-1)) * time.Second // 2s, 4s, 8s…
			slog.Warn("wiki invoke retry",
				"attempt", attempt, "delay", delay.String(), "prev_error", lastErr)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		s, err := invoke(ctx, messages)
		if err == nil {
			return s, nil
		}
		lastErr = err
	}
	return "", lastErr
}
