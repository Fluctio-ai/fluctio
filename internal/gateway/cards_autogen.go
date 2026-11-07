package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/cardsgen"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/wiki"
)

// runCardsForAgent runs one card-generation pass for an agent: resolve the
// agent's provider/model (wiki path), build the stores, hand off to
// cardsgen.Run for yesterday's date.
func runCardsForAgent(ctx context.Context, st store.Store, agentID, date string, dailyLimit int) {
	dbs, ok := st.(*store.DBStore)
	if !ok {
		slog.Warn("cards: store is not DBStore, cannot resolve db handle", "agent", agentID)
		return
	}
	prov, model := resolveWikiProvider(st, agentID, "")
	if prov == nil {
		slog.Warn("cards: no provider/model resolvable, skipping", "agent", agentID)
		return
	}
	ks := kb.NewKBStore(dbs.DB(), dbs.Dialect())
	ws := wiki.NewWikiStore(dbs.DB(), dbs.Dialect())
	created, err := cardsgen.Run(ctx, dbs, ks, ws, agentID, date, prov, model, dailyLimit)
	if err != nil {
		slog.Warn("cards: generate failed", "agent", agentID, "date", date, "error", err)
		return
	}
	slog.Info("cards generated", "agent", agentID, "date", date, "created", created)
}

// runCardsCycle walks every agent with cards.enabled and, for each whose
// yesterday run is still missing AND whose CronTime has passed today,
// fires runCardsForAgent asynchronously. Idempotent — an existing
// kb_card_gen_runs row for yesterday skips. Mirrors runDiaryCycle.
func (g *Gateway) runCardsCycle(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("cards cycle panic", "error", r)
		}
	}()
	dbs, ok := g.store.(*store.DBStore)
	if !ok {
		return
	}
	agents, err := g.store.ListAllAgents(ctx)
	if err != nil {
		slog.Warn("cards: list agents failed", "error", err)
		return
	}
	nowCST := time.Now().In(diaryCST)
	yesterday := nowCST.AddDate(0, 0, -1).Format("2006-01-02")
	for _, ar := range agents {
		cfg := cardsCfgFor(ar)
		if cfg == nil || !cfg.Enabled {
			continue
		}
		// Idempotent: skip if yesterday's generation already ran.
		if cardsgen.HasRunFor(ctx, dbs, ar.ID, yesterday) {
			continue
		}
		// Gate: only generate after today's CronTime has passed (default
		// 03:00) so the diary for that day gets a chance to land first.
		ct := strings.TrimSpace(cfg.CronTime)
		if ct == "" {
			ct = "03:00"
		}
		cronAt := parseCronTimeToday(ct, nowCST)
		if !cronAt.IsZero() && nowCST.Before(cronAt) {
			continue
		}
		// In-flight guard mirrors runDiaryCycle: a cardsgen.Run slower
		// than the 30-minute tick must not be double-fired for the same
		// (agent, date) — duplicate LLM spend + duplicate writes.
		key := "cards|" + ar.ID + "|" + yesterday
		if !g.autogenBegin(key) {
			continue
		}
		limit := cfg.DailyLimit
		go func(ar store.AgentRecord, key string, limit int) {
			defer g.autogenEnd(key)
			defer func() {
				// Bare goroutine: cardsgen.Run panics must not kill the
				// whole process (the cycle-level recover can't reach here).
				if r := recover(); r != nil {
					slog.Warn("cards generation panic", "agent", ar.ID, "date", yesterday, "error", r)
				}
			}()
			// Bounded + lifecycle-bound ctx instead of context.Background():
			// shutdown cancels in-flight runs, and a hung provider can't
			// leak the goroutine + DB connections forever.
			genCtx, cancel := context.WithTimeout(ctx, autogenMaxRunTime)
			defer cancel()
			runCardsForAgent(genCtx, g.store, ar.ID, yesterday, limit)
		}(ar, key, limit)
	}
}

// cardsCfgFor reads the agent's cards config from its config blob
// (AgentRecord.Config["cards"]), normalizing the interface{} round-trip
// the same way reminderChannelFor does. Returns nil when unset.
func cardsCfgFor(ar store.AgentRecord) *config.AgentCardsCfg {
	return cardsCfgFromMap(ar.Config)
}

// cardsCfgFromMap normalizes a raw config-map "cards" sub-object into
// AgentCardsCfg, marshal+unmarshaling the interface{} round-trip the same
// way reminderChannelFor does. Shared by the gateway cycle and the M3
// push sweep.
func cardsCfgFromMap(cfg map[string]interface{}) *config.AgentCardsCfg {
	cfgAny, ok := cfg["cards"]
	if !ok || cfgAny == nil {
		return nil
	}
	var out config.AgentCardsCfg
	if b, err := json.Marshal(cfgAny); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return &out
}

// cardsTicker is the gateway central ticker for nightly card generation:
// half-hourly tick (mirrors diaryTicker), boot pass included so a
// freshly-enabled agent catches up immediately.
func (g *Gateway) cardsTicker(ctx context.Context) {
	g.runEvery(ctx, 30*time.Minute, g.runCardsCycle)
}
