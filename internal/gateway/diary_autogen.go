package gateway

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/diary"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// diaryCST is the UTC+8 zone the diary groups days by and the CronTime
// gate is expressed in (an alias of diary.CST, the single source of truth).
var diaryCST = diary.CST

// runDiaryForAgent generates one day's diary for an agent. Resolves the
// agent's provider/model (reusing the wiki path — same scope merge),
// reads the per-agent AgentDiaryCfg, and hands off to diary.Generate.
// Nothing is written when the day had no summaries.
func runDiaryForAgent(ctx context.Context, st store.Store, agentID, date string) {
	dbs, ok := st.(*store.DBStore)
	if !ok {
		slog.Warn("diary: store is not DBStore, cannot resolve db handle", "agent", agentID)
		return
	}
	fc, _ := config.AgentFileConfigLoader(agentID, "")
	if fc.Diary == nil || !fc.Diary.Enabled {
		return
	}
	prov, model := resolveWikiProvider(st, agentID, "")
	if prov == nil {
		slog.Warn("diary: no provider/model resolvable, skipping", "agent", agentID)
		return
	}
	entry, err := diary.Generate(ctx, dbs, agentID, date, prov, model, fc.Diary.ThinkingMode)
	if err != nil {
		slog.Warn("diary: generate failed", "agent", agentID, "date", date, "error", err)
		return
	}
	if entry == nil {
		slog.Debug("diary: no summaries for day, skipped", "agent", agentID, "date", date)
		return
	}
	slog.Info("diary generated",
		"agent", agentID, "date", date,
		"themes", len(entry.Themes), "blindspots", len(entry.Blindspots))
}

// runDiaryCycle walks every agent with diary.enabled and, for each whose
// yesterday diary is still missing AND whose CronTime has passed today,
// fires runDiaryForAgent asynchronously. One agent's panic is recovered
// so the rest still run. Idempotent — an existing yesterday row skips.
func (g *Gateway) runDiaryCycle(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("diary cycle panic", "error", r)
		}
	}()
	agents, err := g.store.ListAllAgents(ctx)
	if err != nil {
		slog.Warn("diary: list agents failed", "error", err)
		return
	}
	nowCST := time.Now().In(diaryCST)
	yesterday := nowCST.AddDate(0, 0, -1).Format("2006-01-02")
	for _, ar := range agents {
		fc, ok := config.AgentFileConfigLoader(ar.ID, "")
		if !ok || fc.Diary == nil || !fc.Diary.Enabled {
			continue
		}
		// Idempotent: skip if yesterday's diary already exists.
		if existing, _ := g.store.GetDailyDiary(ctx, ar.ID, yesterday); existing != nil {
			continue
		}
		// Gate: only generate after today's CronTime has passed, so late-
		// night conversations get a chance to compact into summaries first.
		ct := strings.TrimSpace(fc.Diary.CronTime)
		if ct == "" {
			ct = "02:30"
		}
		cronAt := parseCronTimeToday(ct, nowCST)
		if !cronAt.IsZero() && nowCST.Before(cronAt) {
			continue
		}
		go runDiaryForAgent(context.Background(), g.store, ar.ID, yesterday)
	}
}

// parseCronTimeToday parses "HH:MM" into today's instant in diaryCST.
// Returns zero time on malformed input (caller treats zero as "no gate").
func parseCronTimeToday(hhmm string, today time.Time) time.Time {
	parts := strings.Split(hhmm, ":")
	if len(parts) != 2 {
		return time.Time{}
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return time.Time{}
	}
	return time.Date(today.Year(), today.Month(), today.Day(), h, m, 0, 0, diaryCST)
}

// diaryTicker is the gateway central ticker for background diary
// generation: half-hourly tick, walk every agent with diary.enabled,
// gate on CronTime + idempotency, fire runDiaryForAgent asynchronously.
// Decoupled from chat traffic.
func (g *Gateway) diaryTicker(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	// Run once at boot so a freshly-enabled agent doesn't wait up to 30m.
	g.runDiaryCycle(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.runDiaryCycle(ctx)
		}
	}
}
