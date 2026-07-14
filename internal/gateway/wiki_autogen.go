package gateway

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/wiki"
)

// wikiAutoGenDefaultInterval is the cadence used when WikiAutoGen.Interval
// is enabled but unset. Tuned for "new KB content shows up in wiki within a
// workday" without re-running the pipeline every tick on busy agents.
const wikiAutoGenDefaultInterval = 6 * time.Hour

// runWikiAutoGenForAgent runs one wiki-generation sweep for one agent:
// resolve the agent's LLM provider/model, list KB sources whose
// wiki_generated_at is NULL, and run the two-step wiki pipeline on each.
// Only new sources are processed — already-generated ones are skipped via
// the WikiGeneratedAt check (kept accurate by kb.KBStore.ListSources).
// Called by the gateway central ticker, run in its own goroutine per agent.
func runWikiAutoGenForAgent(ctx context.Context, st store.Store, agentID string, cfg config.WikiAutoGenCfg) {
	dbs, ok := st.(*store.DBStore)
	if !ok {
		slog.Warn("wiki autogen: store is not DBStore, cannot resolve db handle", "agent", agentID)
		return
	}

	prov, model := resolveWikiProvider(st, agentID, cfg.Model)
	if prov == nil {
		slog.Warn("wiki autogen: no provider/model resolvable, skipping", "agent", agentID)
		pending, _ := st.CountPendingKBSources(ctx, agentID)
		_ = st.SetWikiAutoGenResult(ctx, agentID, time.Now(), "no_provider", "", pending)
		return
	}

	ws := wiki.NewWikiStore(dbs.DB(), dbs.Dialect())
	kbs := kb.NewKBStore(dbs.DB(), dbs.Dialect())

	sources, err := kbs.ListSources(ctx, agentID, 500, 0)
	if err != nil {
		slog.Warn("wiki autogen: list sources failed", "agent", agentID, "error", err)
		_ = st.SetWikiAutoGenResult(ctx, agentID, time.Now(), "error", err.Error(), 0)
		return
	}

	var toProcess []kb.KBSource
	for _, s := range sources {
		if s.WikiGeneratedAt == nil {
			toProcess = append(toProcess, s)
		}
	}
	if len(toProcess) == 0 {
		slog.Debug("wiki autogen: no unprocessed sources", "agent", agentID)
		_ = st.SetWikiAutoGenResult(ctx, agentID, time.Now(), "no_sources", "", 0)
		return
	}

	invoker := func(ctx context.Context, messages []provider.Message) (string, error) {
		resp, err := prov.Chat(ctx, messages, nil, model, 8192, 0.3)
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}

	gen := wiki.NewGenerator(ws, kbs, invoker)
	created, failed := 0, 0
	var firstErr string
	for _, s := range toProcess {
		r := gen.Generate(ctx, agentID, s.ID)
		if r.Error != "" {
			slog.Warn("wiki autogen: generate failed", "agent", agentID, "source", s.ID, "error", r.Error)
			if firstErr == "" {
				firstErr = r.Error
			}
			failed++
			continue
		}
		if err := kbs.MarkSourceGenerated(ctx, s.ID); err != nil {
			slog.Warn("wiki autogen: mark generated failed", "agent", agentID, "source", s.ID, "error", err)
		}
		created++
		slog.Info("wiki autogen: source processed",
			"agent", agentID, "source", s.ID,
			"pages_created", r.PagesCreated, "pages_updated", r.PagesUpdated, "edges", r.EdgesAdded)
	}
	status := "ok"
	if created == 0 && failed > 0 {
		status = "error"
	} else if failed > 0 {
		status = "partial"
	}
	pending, _ := st.CountPendingKBSources(ctx, agentID)
	_ = st.SetWikiAutoGenResult(ctx, agentID, time.Now(), status, firstErr, pending)
	slog.Info("wiki autogen sweep done", "agent", agentID, "processed", created, "failed", failed, "elapsed_sources", len(toProcess))
}

// wikiAutoGenTicker is the gateway central ticker for background wiki
// generation: hourly tick, walk every agent with memory.wikiAutoGen.enabled,
// gate on Interval, fire runWikiAutoGenForAgent asynchronously. Decoupled
// from chat traffic — idle agents still pick up new KB content.
func (g *Gateway) wikiAutoGenTicker(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	// Run once at boot so a freshly-enabled agent doesn't wait up to an
	// hour for the first tick — mirrors idleSummaryTicker's boot sweep.
	g.runWikiAutoGenCycle(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.runWikiAutoGenCycle(ctx)
		}
	}
}

// runWikiAutoGenCycle walks every agent, and for those with wikiAutoGen.enabled
// whose Interval has elapsed since the last sweep, fires the generator
// asynchronously. One agent's failure (panic) is recovered so the rest still run.
func (g *Gateway) runWikiAutoGenCycle(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("wiki autogen cycle panic", "error", r)
		}
	}()
	agents, err := g.store.ListAllAgents(ctx)
	if err != nil {
		slog.Warn("wiki autogen: list agents failed", "error", err)
		return
	}
	for _, ar := range agents {
		var mem config.MemoryCfg
		if err := scope.SettingInto(ctx, g.store, "memory", ar.UserID, ar.ID, &mem); err != nil {
			continue
		}
		cfg := mem.WikiAutoGen
		if !cfg.Enabled {
			continue
		}
		interval := cfg.Interval
		if interval <= 0 {
			interval = wikiAutoGenDefaultInterval
		}
		last, _ := g.store.GetWikiAutoGenLastRun(ctx, ar.ID)
		if !last.IsZero() && time.Since(last) < interval {
			continue
		}
		_ = g.store.SetWikiAutoGenLastRun(ctx, ar.ID, time.Now())
		go runWikiAutoGenForAgent(context.Background(), g.store, ar.ID, cfg)
	}
}

// runWikiAutoGenCycleForTest exposes the cycle for tests to drive directly
// (bypassing the ticker's hourly timer).
func runWikiAutoGenCycleForTest(ctx context.Context, g *Gateway) {
	g.runWikiAutoGenCycle(ctx)
}

// resolveWikiProvider mirrors setup.Server.providerForAgent: merge
// system → user(owner) → agent provider scopes, then resolve the model id
// (cfg model override wins, else agent-level agents.defaults, else system
// default) and instantiate the provider. Returns (nil, "") when anything
// is missing so the caller can skip the agent cleanly.
func resolveWikiProvider(st store.Store, agentID, modelOverride string) (provider.Provider, string) {
	ctx := context.Background()

	var ownerUserID string
	if agentID != "" {
		if ag, err := st.GetAgent(ctx, agentID); err == nil && ag != nil {
			ownerUserID = ag.UserID
		}
	}

	providerMap, err := scope.Providers(ctx, st, ownerUserID, agentID)
	if err != nil || len(providerMap) == 0 {
		return nil, ""
	}

	model := modelOverride
	if model == "" && agentID != "" {
		if agentRow, _ := st.GetConfigByName(ctx, store.KindSetting, "", agentID, "agents.defaults"); agentRow != nil {
			model, _ = agentRow.Data["model"].(string)
		}
	}
	if model == "" {
		if defaultsRow, err := st.GetConfigByName(ctx, store.KindSetting, "", "", "agents.defaults"); err == nil && defaultsRow != nil {
			model, _ = defaultsRow.Data["model"].(string)
		}
	}
	if model == "" {
		return nil, ""
	}

	parts := strings.SplitN(model, "/", 2)
	if len(parts) != 2 {
		return nil, ""
	}
	p, ok := providerMap[parts[0]]
	if !ok || p.APIKey == "" {
		return nil, ""
	}
	return provider.NewProvider(p.APIKey, p.APIBase, p.APIType), model
}

// idleSummaryTicker is the background sweep that summarizes sessions the
// user ended by walking away from (no /compact, no /new). Every interval
// it walks every loaded UserSpace's agents and runs SummarizeIdleSessions.
// A session qualifies when quiet > idleAfter AND has >= minMessages; the
// sweep double-checks quiet again before summarizing so a user who came
// back between scan and processing is left alone. Exits on ctx cancel.
func (g *Gateway) idleSummaryTicker(ctx context.Context, interval, idleAfter time.Duration, minMessages int) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	if idleAfter <= 0 {
		idleAfter = 24 * time.Hour
	}
	slog.Info("idle summary sweep started",
		"interval", interval, "idle_after", idleAfter, "min_messages", minMessages)
	// Run once at boot so a backlog clears without waiting a full interval.
	g.runIdleSummaryCycle(ctx, idleAfter, minMessages)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("idle summary sweep stopped")
			return
		case <-t.C:
			g.runIdleSummaryCycle(ctx, idleAfter, minMessages)
		}
	}
}

// runIdleSummaryCycle walks every loaded UserSpace and invokes
// SummarizeIdleSessions on its agent manager. One cycle failure (panic) is
// recovered so the next interval still runs.
func (g *Gateway) runIdleSummaryCycle(ctx context.Context, idleAfter time.Duration, minMessages int) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("idle summary cycle panic", "error", r)
		}
	}()
	for _, sp := range g.users.all() {
		if ctx.Err() != nil {
			return
		}
		if sp.Agents == nil {
			continue
		}
		sp.Agents.SummarizeIdleSessions(ctx, idleAfter, minMessages)
	}
}
