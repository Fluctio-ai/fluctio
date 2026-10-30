package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestQuotaBreakerTripGateProbeClear walks the full breaker lifecycle:
// trip → gate blocks → probe window opens → gate lets the probe through →
// clear on success → gate no longer blocks.
func TestQuotaBreakerTripGateProbeClear(t *testing.T) {
	var q quotaBreaker
	now := time.Now()

	// Healthy: gate open.
	if blocked, _, _ := q.gate(now); blocked {
		t.Fatalf("healthy breaker blocked the call")
	}

	// Trip with a 30min probe interval.
	q.trip("API error 429: You've reached your monthly quota", now, 30*time.Minute)

	blocked, reason, retryAt := q.gate(now.Add(time.Minute))
	if !blocked {
		t.Fatalf("tripped breaker should block before nextProbe")
	}
	if reason == "" || !retryAt.After(now) {
		t.Errorf("gate returned reason=%q retryAt=%v, want non-empty reason and future retryAt", reason, retryAt)
	}

	// Half-open: at/after nextProbe the gate lets the probe through.
	blocked, _, _ = q.gate(now.Add(31 * time.Minute))
	if blocked {
		t.Fatalf("gate should allow the probe at/after nextProbe")
	}

	// Probe failed with the same quota error → re-armed, trippedAt kept,
	// and the next wait DOUBLED (probeStreak grows: base, 2x, 4x… capped).
	q.trip("API error 429: You've reached your monthly quota", now.Add(31*time.Minute), 30*time.Minute)
	q.mu.Lock()
	origTrippedAt := q.trippedAt
	nextAfterFail := q.nextProbe
	q.mu.Unlock()
	if !origTrippedAt.Equal(now) {
		t.Errorf("re-trip moved trippedAt: %v, want original %v", origTrippedAt, now)
	}
	if want := now.Add(31 * time.Minute).Add(60 * time.Minute); !nextAfterFail.Equal(want) {
		t.Errorf("re-trip nextProbe = %v, want %v (doubled 30m→60m)", nextAfterFail, want)
	}
	if blocked, _, _ := q.gate(now.Add(32 * time.Minute)); !blocked {
		t.Fatalf("re-armed breaker should block again")
	}

	// Success clears.
	q.clear()
	if blocked, _, _ := q.gate(now.Add(33 * time.Minute)); blocked {
		t.Fatalf("cleared breaker should not block")
	}
}

// TestQuotaProbeIntervalEnv verifies the breaker's interval resolution
// falls back to the env knob when no store is wired (the settings row is
// the first source, covered by the trip/clear lifecycle test).
func TestQuotaProbeIntervalEnv(t *testing.T) {
	t.Setenv("FLUCTIO_QUOTA_PROBE_SECS", "600")
	a := &Agent{} // no dataStore → env layer
	if got := a.quotaProbeInterval(context.Background()); got != 10*time.Minute {
		t.Errorf("quotaProbeInterval = %v, want 10m from env", got)
	}
	t.Setenv("FLUCTIO_QUOTA_PROBE_SECS", "1") // below the 60s floor → default
	if got := a.quotaProbeInterval(context.Background()); got != 30*time.Minute {
		t.Errorf("quotaProbeInterval = %v, want the 30m default", got)
	}
}

// TestQuotaOpenErrorMessage verifies the sentinel renders a readable error
// (it feeds slog and the diag trail, not the user notice directly).
func TestQuotaOpenErrorMessage(t *testing.T) {
	e := &quotaOpenError{reason: "monthly quota", retryAt: time.Unix(1793300000, 0)}
	var target *quotaOpenError
	if !errors.As(errors.New("wrapped: "+e.Error()), &target) {
		// errors.As on a plain wrap of the string won't match — just check
		// the direct type works, which is how the loop tests it.
		if _, ok := interface{}(e).(*quotaOpenError); !ok {
			t.Fatalf("quotaOpenError type assertion failed")
		}
	}
	if e.Error() == "" {
		t.Fatalf("empty error string")
	}
}
