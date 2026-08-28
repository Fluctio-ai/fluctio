package gateway

import (
	"context"
	"time"
)

// runEvery runs cycle once at boot (so a freshly-enabled agent or a
// long-standby instance catches up immediately), then on every tick of a
// d interval until ctx ends. The shared pipe of the gateway's periodic
// sweeps (cards autogen, cards push, memory consolidation); each cycle
// keeps its own panic recovery and best-effort error handling.
func (g *Gateway) runEvery(ctx context.Context, d time.Duration, cycle func(context.Context)) {
	cycle(ctx)
	ticker := time.NewTicker(d)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cycle(ctx)
		}
	}
}
