package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/diary"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// runShelvedPushSweep periodically pushes each agent's shelved-ledger
// changes to its configured IM channel. Mirrors cardsPushCycle's shape:
// half-hourly tick, pushTime gate, diary_shelved_push_runs stamp for
// once-a-day semantics — a failed push leaves no stamp so the next tick
// retries, a delivered one never double-sends.
func (g *Gateway) runShelvedPushSweep(ctx context.Context) {
	g.runEvery(ctx, 30*time.Minute, g.shelvedPushCycle)
}

// shelvedPushCycle walks every agent with diary shelved push enabled and,
// when the ledger changed overnight (a fresh shelve or a pickup), sends one
// nudge and archives it into the target session — once per ledger change
// (dedup keyed on the head diary's date), never per calendar day. The
// nudge only ever points at the ledger — settling it stays the user's own
// move.
func (g *Gateway) shelvedPushCycle(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("shelved push cycle panic", "error", r)
		}
	}()
	dbs, ok := g.store.(*store.DBStore)
	if !ok || dbs == nil {
		return
	}
	agents, err := dbs.ListAllAgents(ctx)
	if err != nil {
		slog.Warn("shelved push: list agents", "error", err)
		return
	}
	nowCST := time.Now().In(diaryCST)
	for _, ag := range agents {
		// Parse the config blob straight from the agent row we already
		// hold — the config loader would re-scan the agents table per
		// agent (cardsCfgFromMap's zero-query pattern).
		dc := diaryCfgFromMap(ag.Config)
		if dc == nil || !dc.Enabled || !dc.ShelvedEnabled || !dc.ShelvedPushEnabled {
			continue
		}
		// Gate: only push after today's push time (default 09:00) so the
		// nudge lands in the morning, not at boot / midnight generation.
		if !pushTimePassed(dc.ShelvedPushTime, nowCST) {
			continue
		}
		ledger, ledgerDate, err := diary.CurrentShelvedLedger(ctx, dbs, ag.ID, nowCST)
		if err != nil {
			slog.Warn("shelved push: ledger", "agent", ag.ID, "error", err)
			continue
		}
		// No head, or a ledger that merely rolls forward unchanged = no
		// nudge (and no stamp: the check is one narrow row, and an
		// unstamped no-change day keeps the door open for a manual diary
		// regenerate that does change the ledger later today).
		if ledgerDate == "" || !diary.ShelvedLedgerChanged(ledger, ledgerDate) {
			continue
		}
		// Dedup by the CHANGE's identity (the head diary's date), not the
		// push calendar day: the changed-predicate is a property of the
		// head, so keying on "today" would re-nag every morning for a
		// stale-but-unchanged head. One head's change is pushed once.
		if dbs.HasDailyRun(ctx, "diary_shelved_push_runs", ag.ID, ledgerDate) {
			continue
		}
		// Shared delivery policy (notifications_enqueue.go): IM optional,
		// queue always, either delivering stamps the run.
		uid := sweepOwnerUID(ctx, dbs, ag)
		ch, chatID, sessionKey, channel := g.resolveSweepTarget(ctx, dbs, ag.ID, dc.ShelvedPushChannel, "shelved push")
		nudge, n := formatShelvedNudge(ag.ID, ledger, ledgerDate)
		if !g.deliverSweepNudge(ctx, dbs, ag, uid, ch, chatID, sessionKey,
			"shelved_nudge", "📌 搁浅话题", nudge, "/agents/"+ag.ID+"/knowledge/diary", "shelved_nudge", "shelved push") {
			continue // no stamp — retry next tick
		}
		if err := dbs.StampShelvedPushRun(ctx, ag.ID, ledgerDate, n, channel); err != nil {
			slog.Warn("shelved push: stamp", "agent", ag.ID, "error", err)
		}
		slog.Info("shelved nudge pushed", "agent", ag.ID, "open", n, "channel", channel, "chat", chatID, "ledger", ledgerDate)
	}
}

// diaryCfgFromMap parses the agent row's "diary" config sub-object without
// the config loader's extra full-table scan — the sweep already holds the
// agent row (cardsCfgFromMap's zero-query pattern).
func diaryCfgFromMap(cfg map[string]interface{}) *config.AgentDiaryCfg {
	cfgAny, ok := cfg["diary"]
	if !ok || cfgAny == nil {
		return nil
	}
	var out config.AgentDiaryCfg
	if b, err := json.Marshal(cfgAny); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return &out
}

// formatShelvedNudge builds the IM body for one ledger change:
//
//	📌 搁浅话题
//	· 新搁浅: 想重构推送架构（10-24）
//	· 仍在搁浅: 2 条
//	✓ 捡起了: 周报模板
//	处理: <deep link to the diary page>
//
// Returns the body plus the open count (for the run stamp).
func formatShelvedNudge(agentID string, ledger []store.DiaryShelved, ledgerDate string) (string, int) {
	var b strings.Builder
	b.WriteString("📌 搁浅话题")
	open := 0
	const maxList = 5
	listed := 0
	for _, e := range ledger {
		if e.Status == "open" {
			open++
			if e.Since == ledgerDate && listed < maxList {
				b.WriteString("\n· 新搁浅: " + e.Topic)
				listed++
			}
		}
	}
	for _, e := range ledger {
		if e.Status == "resolved" {
			b.WriteString("\n✓ 捡起了: " + e.Topic)
		}
	}
	carried := open - listed
	if carried > 0 {
		b.WriteString("\n· 仍在搁浅: " + strconv.Itoa(carried) + " 条")
	}
	if base := strings.TrimRight(config.LoadEnv().Gateway.PublicBaseURL, "/"); base != "" {
		b.WriteString("\n处理: " + base + "/agents/" + agentID + "/knowledge/diary")
	}
	return b.String(), open
}
