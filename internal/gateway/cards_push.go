package gateway

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// runCardsPushSweep periodically pushes each agent's due-card digest to
// its configured IM channel. Mirrors runDueTodoReminderSweep's addressing
// (deliverableTargetForAgent) but with its own once-a-day semantics: a
// kb_card_push_runs row for today gates the push, so the half-hourly tick
// retries a failed push but never double-sends a delivered one.
func (g *Gateway) runCardsPushSweep(ctx context.Context) {
	g.runEvery(ctx, 30*time.Minute, g.cardsPushCycle)
}

// cardsPushCycle walks every agent with cards.pushEnabled whose pushTime
// has passed today and hasn't been pushed to yet, and sends one summary
// digest (due count + first few questions + deep link when a public base
// URL is configured), then archives it hidden into the target session so
// the web history shows the notification too. Best-effort per agent;
// failure leaves no stamp so the next tick retries.
func (g *Gateway) cardsPushCycle(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("cards push cycle panic", "error", r)
		}
	}()
	dbs, ok := g.store.(*store.DBStore)
	if !ok || dbs == nil {
		return
	}
	agents, err := dbs.ListAllAgents(ctx)
	if err != nil {
		slog.Warn("cards push: list agents", "error", err)
		return
	}
	nowCST := time.Now().In(diaryCST)
	today := nowCST.Format("2006-01-02")
	for _, ag := range agents {
		cfg := cardsCfgFromMap(ag.Config)
		if cfg == nil || !cfg.PushEnabled {
			continue
		}
		if cardsPushedToday(ctx, dbs, ag.ID, today) {
			continue
		}
		// Gate: only push after today's PushTime (default 09:00) so the
		// digest lands at the configured hour, not at boot.
		pt := strings.TrimSpace(cfg.PushTime)
		if pt == "" {
			pt = "09:00"
		}
		pushAt := parseCronTimeToday(pt, nowCST)
		if !pushAt.IsZero() && nowCST.Before(pushAt) {
			continue
		}
		ks := kb.NewKBStore(dbs.DB(), dbs.Dialect())
		// Same queue the web review session walks — capped by
		// reviewLimit, most-overdue first — so the digest advertises
		// exactly today's reviewable group, not the whole backlog.
		reviewLimit := cfg.ReviewLimit
		if reviewLimit < 1 {
			reviewLimit = 20
		}
		due, err := ks.ListDueQueue(ctx, ag.ID, reviewLimit)
		if err != nil {
			slog.Warn("cards push: list due", "agent", ag.ID, "error", err)
			continue
		}
		if len(due) == 0 {
			continue
		}
		channel := cfg.PushChannel
		if channel == "" {
			channel = "wechat"
		}
		accountID, chatID, sessionKey, ok := deliverableTargetForAgent(ctx, dbs, ag.ID, channel)
		if !ok {
			slog.Warn("cards push: no deliverable target; skipping (bind+message the agent once)", "agent", ag.ID, "channel", channel)
			continue
		}
		ch := g.chanMgr.Get(channel, accountID)
		if ch == nil {
			slog.Warn("cards push: channel not registered", "agent", ag.ID, "channel", channel, "account", accountID)
			continue
		}
		st, _ := ks.CardStats(ctx, ag.ID)
		digest := formatCardsDigest(ag.ID, due, st)
		if err := ch.Send(chatID, digest); err != nil {
			slog.Warn("cards push failed", "agent", ag.ID, "error", err)
			continue // no stamp — retry next tick
		}
		if err := archivePushNotice(ctx, dbs, ag.ID, sessionKey, digest, "cards_digest"); err != nil {
			// IM delivery already succeeded — keep the push stamped and
			// just log: losing the web-side bubble beats double-sending.
			slog.Warn("cards push: archive digest", "agent", ag.ID, "error", err)
		}
		if err := stampCardsPush(ctx, dbs, ag.ID, today, len(due), channel); err != nil {
			slog.Warn("cards push: stamp", "agent", ag.ID, "error", err)
		}
		slog.Info("cards digest pushed", "agent", ag.ID, "due", len(due), "channel", channel, "chat", chatID)
	}
}

// formatCardsDigest builds the IM body for the daily card group — the
// day's learning plan in ONE message:
//
//	🧠 今日卡片组（5 张 · 连续 3 天）
//	1. 问题一
//	2. 问题二 🔁        ← previously reviewed, back for another pass
//	…
//	（其中 2 张来自前几日未完成）
//	开始复习：<deep link>
//
// Reading the numbered questions in chat is itself a light self-test; the
// 🔁 marks repeat cards and the carry-over line makes an unexecuted group
// visibly roll into today. The deep link (review flow) only appears when
// a public base URL is configured — env-gated like pubimg.
func formatCardsDigest(agentID string, due []kb.KBCard, st kb.KBCardStats) string {
	var b strings.Builder
	b.WriteString("🧠 今日卡片组（" + strconv.Itoa(len(due)) + " 张")
	if st.StreakDays > 0 {
		b.WriteString(" · 连续 " + strconv.Itoa(st.StreakDays) + " 天")
	}
	b.WriteString("）")
	todayStart := time.Now().In(diaryCST).Format("2006-01-02")
	carry := 0
	const maxList = 12
	for i, c := range due {
		if i >= maxList {
			break
		}
		mark := ""
		if c.ReviewCount > 0 {
			mark = " 🔁"
		}
		b.WriteString("\n" + strconv.Itoa(i+1) + ". " + c.Question + mark)
	}
	if len(due) > maxList {
		b.WriteString("\n…")
	}
	for _, c := range due {
		if c.DueAt != nil && c.DueAt.In(diaryCST).Format("2006-01-02") != todayStart {
			carry++
		}
	}
	if carry > 0 {
		b.WriteString("\n（其中 " + strconv.Itoa(carry) + " 张来自前几日未完成）")
	}
	if base := strings.TrimRight(config.LoadEnv().Gateway.PublicBaseURL, "/"); base != "" {
		b.WriteString("\n开始复习：" + base + "/agents/" + agentID + "/knowledge/cards?review=1")
	}
	return b.String()
}

// cardsPushedToday reports whether the agent already got its digest today.
func cardsPushedToday(ctx context.Context, dbs *store.DBStore, agentID, date string) bool {
	var one int
	err := dbs.DB().QueryRowContext(ctx,
		`SELECT 1 FROM kb_card_push_runs WHERE agent_id = `+dbs.Ph(1)+` AND date = `+dbs.Ph(2),
		agentID, date).Scan(&one)
	return err == nil
}

// stampCardsPush records today's delivered digest (the once-a-day gate).
func stampCardsPush(ctx context.Context, dbs *store.DBStore, agentID, date string, count int, channel string) error {
	q := `INSERT INTO kb_card_push_runs (agent_id, date, pushed_count, channel, pushed_at)
		VALUES (` + dbs.Ph(1) + `,` + dbs.Ph(2) + `,` + dbs.Ph(3) + `,` + dbs.Ph(4) + `,CURRENT_TIMESTAMP)
		ON CONFLICT(agent_id, date) DO UPDATE SET pushed_count=excluded.pushed_count,
			channel=excluded.channel, pushed_at=CURRENT_TIMESTAMP`
	_, err := dbs.DB().ExecContext(ctx, q, agentID, date, count, channel)
	return err
}
