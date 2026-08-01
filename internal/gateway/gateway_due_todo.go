package gateway

import (
	"context"
	"log/slog"
	"time"

	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// runDueTodoReminderSweep periodically scans every agent's todos for items
// nearing or past their end_at. Each due todo is logged (and, once a default
// push channel is wired, forwarded there) then stamped via MarkTodoReminded
// so it won't fire again until a status/time change resets reminded_at. Boots
// once immediately so a backlog clears, then on a 30min cadence.
func (g *Gateway) runDueTodoReminderSweep(ctx context.Context) {
	const interval = 30 * time.Minute
	const windowHours = 24
	g.sweepAllAgentsDueTodos(ctx, windowHours)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.sweepAllAgentsDueTodos(ctx, windowHours)
		}
	}
}

// sweepAllAgentsDueTodos walks every agent, builds a throwaway KB store, and
// marks each due-unreminded todo. Per-agent best-effort (logged, not fatal) so
// one bad agent doesn't stall the rest. The KB store needs no embedder —
// ListDueTodos is pure SQL.
func (g *Gateway) sweepAllAgentsDueTodos(ctx context.Context, windowHours int) {
	dbs, ok := g.store.(*store.DBStore)
	if !ok || dbs == nil {
		return
	}
	agents, err := dbs.ListAllAgents(ctx)
	if err != nil {
		slog.Warn("due-todo sweep: list agents", "error", err)
		return
	}
	for _, ag := range agents {
		ks := kb.NewKBStore(dbs.DB(), dbs.Dialect())
		due, err := ks.ListDueTodos(ctx, ag.ID, windowHours)
		if err != nil {
			slog.Warn("due-todo sweep: list", "agent", ag.ID, "error", err)
			continue
		}
		for _, t := range due {
			// TODO(reminders): push to the agent's default IM channel once
			// channel-selection is defined. For now this is a log-only probe
			// so the sweep cadence + dedup are observable in practice.
			slog.Info("due-todo reminder", "agent", ag.ID, "todo", t.ID, "title", t.Title, "end_at", t.EndAt)
			if err := ks.MarkTodoReminded(ctx, ag.ID, t.ID); err != nil {
				slog.Warn("due-todo sweep: mark reminded", "agent", ag.ID, "todo", t.ID, "error", err)
			}
		}
	}
}
