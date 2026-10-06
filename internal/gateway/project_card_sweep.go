package gateway

import (
	"context"
	"log/slog"
	"time"
)

// projectCardTicker is the hourly safety net that realigns every
// project's shared PROJECT.md with the sessions table: backfills index
// rows for sessions predating the card, converges renames (manual and
// auto title), drops rows for deleted / moved-away sessions, and
// archives overflow. Immediate reconcile hooks on delete / rename /
// move keep the UX tight; this sweep catches everything those miss.
func (g *Gateway) projectCardTicker(ctx context.Context) {
	g.runEvery(ctx, time.Hour, g.runProjectCardCycle)
}

// runProjectCardCycle walks every loaded UserSpace and reconciles its
// agents' project cards. One cycle failure (panic) is recovered so the
// next interval still runs.
func (g *Gateway) runProjectCardCycle(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("project card cycle panic", "error", r)
		}
	}()
	for _, sp := range g.users.all() {
		if ctx.Err() != nil {
			return
		}
		if sp.Agents == nil {
			continue
		}
		sp.Agents.ReconcileProjectCards(ctx)
	}
}
