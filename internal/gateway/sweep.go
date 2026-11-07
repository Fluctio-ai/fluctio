package gateway

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"
)

// runEvery runs cycle once at boot (so a freshly-enabled agent or a
// long-standby instance catches up immediately), then on every tick of a
// d interval until ctx ends. The shared pipe of the gateway's periodic
// sweeps (cards autogen, cards push, memory consolidation). Each cycle is
// expected to keep its own fine-grained panic recovery; the belt-and-
// suspenders recover here guarantees a cycle that forgot its own cannot
// kill the runEvery goroutine — a panic in a bare goroutine takes down
// the whole process.
func (g *Gateway) runEvery(ctx context.Context, d time.Duration, cycle func(context.Context)) {
	if d <= 0 {
		// time.NewTicker panics on non-positive intervals; keep the
		// "interval must be positive" contract in one place instead of
		// relying on every caller to clamp its config-driven value.
		slog.Warn("sweep interval must be positive, sweep disabled", "interval", d)
		return
	}
	runOnce := func(ctx context.Context) {
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("sweep cycle panic recovered",
					"error", r, "stack", string(debug.Stack()))
			}
		}()
		cycle(ctx)
	}
	runOnce(ctx)
	ticker := time.NewTicker(d)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce(ctx)
		}
	}
}
