package agent

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/scope"
)

// quotaBreaker is the agent-level circuit breaker for exhausted provider
// quotas. Once a quota-shaped 429 ("You've reached your monthly quota",
// "insufficient balance") gets past llmRetry, trip records it and every
// subsequent turn is short-circuited at callLLMWithPTLRecovery BEFORE
// touching the provider — the user gets an immediate, actionable reason
// ("quota exhausted, auto-retry at HH:MM") instead of re-burning the retry
// ladder on every message. Recovery is a half-open probe: after probeEvery
// elapses, ONE real call is allowed through; success clears the breaker,
// another quota 429 re-arms it.
//
// State is process-local (not persisted): a daemon restart simply loses the
// tripped state and lets the next real call re-discover it — at one wasted
// call, that's cheaper than a durable marker plus its invalidation logic.
type quotaBreaker struct {
	mu        sync.Mutex
	tripped   bool
	reason    string // provider error summary captured at trip time
	trippedAt time.Time
	nextProbe time.Time
	// probeStreak counts consecutive failed probes. Each re-trip doubles
	// the wait before the next probe (base, 2x, 4x… capped at 64x) — a
	// quota that stays dead for hours doesn't need 30-minute polling, and
	// a success (clear) resets it. The doubling factor cap keeps the
	// shift bounded so a long outage converges instead of overflowing.
	probeStreak int
}

// trip arms the breaker. probeEvery is the FIRST probe interval; further
// consecutive trips double it (see probeStreak). The caller resolves it
// per trip (Agent.quotaProbeInterval: settings row → env → default).
func (q *quotaBreaker) trip(reason string, now time.Time, probeEvery time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	// Re-trips while already tripped (a failed probe) slide nextProbe
	// forward but keep the original trippedAt — the age of the outage is
	// the useful diagnostic.
	if !q.tripped {
		q.trippedAt = now
	}
	q.tripped = true
	q.reason = reason
	shift := q.probeStreak
	if shift > 6 {
		shift = 6 // 64x cap
	}
	q.nextProbe = now.Add(probeEvery << shift)
	q.probeStreak++
}

// gate reports whether calls should be short-circuited. blocked=true means
// don't call the provider; retryAt is when the next probe fires (for the
// user-facing "auto-retry at" line). blocked=false either means healthy or
// probe-due — the caller makes the real call and clear()/trip() on its
// outcome.
func (q *quotaBreaker) gate(now time.Time) (blocked bool, reason string, retryAt time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.tripped {
		return false, "", time.Time{}
	}
	if !now.Before(q.nextProbe) {
		// Half-open: let this call through as the probe.
		return false, q.reason, q.nextProbe
	}
	return true, q.reason, q.nextProbe
}

// clear marks the quota healthy again (the probe — or any call — succeeded).
func (q *quotaBreaker) clear() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.tripped {
		slog.Info("quota breaker cleared — provider call succeeded again",
			"tripped_at", q.trippedAt.UTC().Format(time.RFC3339),
			"failed_probes", q.probeStreak)
	}
	q.tripped = false
	q.reason = ""
	q.nextProbe = time.Time{}
	q.probeStreak = 0
}

// quotaProbeInterval resolves the breaker's first probe interval for this
// agent: system-scope "failure" setting row first (dashboard-adjustable,
// hot — read per trip, no restart), then FLUCTIO_QUOTA_PROBE_SECS, then
// the 30min default.
func (a *Agent) quotaProbeInterval(ctx context.Context) time.Duration {
	if a.dataStore != nil {
		var c config.FailureRetryCfg
		if err := scope.SettingInto(ctx, a.dataStore, config.FailureNamespace, "", "", &c); err == nil && c.QuotaProbeMin >= 1 {
			return time.Duration(c.QuotaProbeMin) * time.Minute
		}
	}
	return resolveQuotaProbeInterval()
}

// resolveQuotaProbeInterval reads FLUCTIO_QUOTA_PROBE_SECS (min 60s,
// default 1800 = 30min). Kept as a function so tests can bypass the env.
func resolveQuotaProbeInterval() time.Duration {
	if v := os.Getenv("FLUCTIO_QUOTA_PROBE_SECS"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 60 {
			return time.Duration(secs) * time.Second
		}
	}
	return 30 * time.Minute
}

// quotaOpenError is returned by callLLMWithPTLRecovery when the breaker is
// tripped and the call was short-circuited (no provider contact). The loop's
// error branch renders its user-facing "quota tripped, auto-retry at …"
// notice from it instead of a generic failure string.
type quotaOpenError struct {
	reason  string
	retryAt time.Time
}

func (e *quotaOpenError) Error() string {
	return "provider quota exhausted (breaker open until " + e.retryAt.Format(time.RFC3339) + "): " + e.reason
}
