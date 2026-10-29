package gateway

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// runBlindspotPushSweep periodically pushes each agent's latest diary
// blindspot section ("you might have missed") to its configured IM channel
// and the notification queue. Mirrors runShelvedPushSweep's shape:
// half-hourly tick, pushTime gate, diary_blindspot_push_runs stamp keyed
// on the diary's date — one push per diary, whatever its content, and
// never anything when the blindspot array is empty (the generator's
// 宁可空不凑数 discipline makes that the common case; this sweep only
// amplifies, never invents).
func (g *Gateway) runBlindspotPushSweep(ctx context.Context) {
	g.runEvery(ctx, 30*time.Minute, g.blindspotPushCycle)
}

// blindspotPushCycle walks every agent with diary + blindspot push enabled
// and, when the latest diary carries blindspots that haven't been pushed,
// sends one nudge (IM optional, notification queue always) and archives it
// into the target session.
func (g *Gateway) blindspotPushCycle(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("blindspot push cycle panic", "error", r)
		}
	}()
	dbs, ok := g.store.(*store.DBStore)
	if !ok || dbs == nil {
		return
	}
	agents, err := dbs.ListAllAgents(ctx)
	if err != nil {
		slog.Warn("blindspot push: list agents", "error", err)
		return
	}
	nowCST := time.Now().In(diaryCST)
	today := nowCST.Format("2006-01-02")
	for _, ag := range agents {
		dc := diaryCfgFromMap(ag.Config)
		if dc == nil || !dc.Enabled || !dc.BlindspotPushEnabled {
			continue
		}
		// Gate: only push after today's push time (default 09:00) so the
		// nudge lands in the morning, after the 02:30 diary generation.
		if !pushTimePassed(dc.BlindspotPushTime, nowCST) {
			continue
		}
		blinds, diaryDate, err := dbs.LatestBlindspots(ctx, ag.ID, today)
		if err != nil {
			slog.Warn("blindspot push: latest blindspots", "agent", ag.ID, "error", err)
			continue
		}
		if diaryDate == "" || len(blinds) == 0 {
			continue
		}
		// Dedup by the diary's identity (its date), not the push calendar
		// day: a stale head that already fired never re-nags, and a manual
		// diary regenerate doesn't re-push either.
		if dbs.HasDailyRun(ctx, "diary_blindspot_push_runs", ag.ID, diaryDate) {
			continue
		}
		// Shared delivery policy (notifications_enqueue.go): IM optional,
		// queue always, either delivering stamps the diary.
		uid := sweepOwnerUID(ctx, dbs, ag)
		ch, chatID, sessionKey, channel := g.resolveSweepTarget(ctx, dbs, ag.ID, dc.BlindspotPushChannel, "blindspot push")
		nudge := formatBlindspotNudge(ag.ID, blinds)
		if !g.deliverSweepNudge(ctx, dbs, ag, uid, ch, chatID, sessionKey,
			"blindspot", "🔍 可能漏了的事", nudge, "/agents/"+ag.ID+"/knowledge/diary", "blindspot_nudge", "blindspot push") {
			continue // no stamp — retry next tick
		}
		if err := dbs.StampBlindspotPushRun(ctx, ag.ID, diaryDate, len(blinds), channel); err != nil {
			slog.Warn("blindspot push: stamp", "agent", ag.ID, "error", err)
		}
		slog.Info("blindspot nudge pushed", "agent", ag.ID, "blindspots", len(blinds), "diary", diaryDate, "channel", channel, "chat", chatID)
	}
}

// formatBlindspotNudge builds the IM body for one diary's blindspot
// section:
//
//	🔍 可能漏了的事（2 条）
//	· 想重构推送架构 — 说周五定方案，一周没回头
//	查看: <deep link to the diary page>
//
// The reason clause rides along on the same line (dash-separated) — it's
// the "why you should care" hook, not filler. The deep link only appears
// when a public base URL is configured (same env-gate as the shelved
// nudge). Cap is defensive: the generator already limits itself to 0-4.
func formatBlindspotNudge(agentID string, blinds []store.DiaryBlindspot) string {
	var b strings.Builder
	b.WriteString("🔍 可能漏了的事（" + strconv.Itoa(len(blinds)) + " 条）")
	const maxList = 4
	for i, bs := range blinds {
		if i >= maxList {
			break
		}
		line := "\n· " + bs.Point
		if r := strings.TrimSpace(bs.Reason); r != "" {
			line += " — " + r
		}
		b.WriteString(line)
	}
	if base := strings.TrimRight(config.LoadEnv().Gateway.PublicBaseURL, "/"); base != "" {
		b.WriteString("\n查看: " + base + "/agents/" + agentID + "/knowledge/diary")
	}
	return b.String()
}
