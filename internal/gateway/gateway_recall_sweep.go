package gateway

import (
	"context"
	"log/slog"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/embedding"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// runImplicitFeedbackSweep periodically converts "did the user stay on the
// recalled topic?" into thumbs-up/down feedback, so the MMR-lambda bandit
// can tune without anyone clicking a button. Probes once at boot so a
// backlog clears immediately, then on a slow cadence (the signal needs the
// post-recall conversation to mature a few messages). Tickers until ctx
// is cancelled.
func (g *Gateway) runImplicitFeedbackSweep(ctx context.Context) {
	const interval = 10 * time.Minute
	g.sweepAllAgentsImplicitFeedback(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.sweepAllAgentsImplicitFeedback(ctx)
		}
	}
}

// sweepAllAgentsImplicitFeedback walks every agent with embedding enabled,
// builds its embedder from the agent memory config, and runs the per-agent
// sweep. Failures are per-agent best-effort (logged, not fatal) so one bad
// agent doesn't stall the rest.
func (g *Gateway) sweepAllAgentsImplicitFeedback(ctx context.Context) {
	db, ok := g.store.(*store.DBStore)
	if !ok || db == nil {
		return
	}
	agents, err := db.ListAllAgents(ctx)
	if err != nil {
		slog.Warn("implicit feedback: list agents", "error", err)
		return
	}
	cfg := store.DefaultImplicitFeedbackConfig
	for _, ag := range agents {
		var mem config.MemoryCfg
		if err := scope.SettingInto(ctx, db, NSMemory, ag.UserID, ag.ID, &mem); err != nil {
			continue
		}
		if !mem.Embedding.Enabled {
			continue
		}
		// Failure-driven gate: skip the embedder probe + sweep when no
		// recall events are awaiting feedback. The probe is a real
		// embedding API call, so on an idle agent this avoids a pointless
		// round-trip every tick. Mirrors memoryindex's failure-driven style.
		pending, perr := db.HasPendingImplicitFeedback(ctx, ag.ID, cfg.MaxAgeMinutes)
		if perr != nil {
			slog.Warn("implicit feedback: pending check", "agent", ag.ID, "error", perr)
			continue
		}
		if !pending {
			continue
		}
		ec := mem.Embedding
		emb := embedding.ProbeEmbedder(ctx,
			embedding.NewOpenAICompatEmbedder(ec.APIBase, ec.APIKey, ec.Model, ec.Dim, ec.DimEnabled))
		if !emb.Available() {
			continue
		}
		n, err := db.SweepImplicitFeedback(ctx, ag.ID, emb, cfg)
		if err != nil {
			slog.Warn("implicit feedback sweep", "agent", ag.ID, "error", err)
			continue
		}
		if n > 0 {
			slog.Info("implicit feedback sweep", "agent", ag.ID, "recorded", n)
		}
	}
}
