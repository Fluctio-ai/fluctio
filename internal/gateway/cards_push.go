package gateway

import (
	"context"
	"fmt"
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
	const interval = 30 * time.Minute
	g.cardsPushCycle(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.cardsPushCycle(ctx)
		}
	}
}

// cardsPushCycle walks every agent with cards.pushEnabled whose pushTime
// has passed today and hasn't been pushed to yet, and sends one summary
// digest (due count + first few questions + deep link when a public base
// URL is configured). Best-effort per agent; failure leaves no stamp so
// the next tick retries.
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
		due, err := ks.ListCards(ctx, ag.ID, "due", "", "", 50, 0)
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
		accountID, chatID, ok := deliverableTargetForAgent(ctx, dbs, ag.ID, channel)
		if !ok {
			slog.Warn("cards push: no deliverable target; skipping (bind+message the agent once)", "agent", ag.ID, "channel", channel)
			continue
		}
		ch := g.chanMgr.Get(channel, accountID)
		if ch == nil {
			slog.Warn("cards push: channel not registered", "agent", ag.ID, "channel", channel, "account", accountID)
			continue
		}
		if err := ch.Send(chatID, formatCardsDigest(ag.ID, due)); err != nil {
			slog.Warn("cards push failed", "agent", ag.ID, "error", err)
			continue // no stamp — retry next tick
		}
		if err := stampCardsPush(ctx, dbs, ag.ID, today, len(due), channel); err != nil {
			slog.Warn("cards push: stamp", "agent", ag.ID, "error", err)
		}
		slog.Info("cards digest pushed", "agent", ag.ID, "due", len(due), "channel", channel, "chat", chatID)
	}
}

// formatCardsDigest builds the IM body: due count, the first three
// questions as a teaser, and a deep link into the review flow when a
// public base URL is configured (env-gated like pubimg).
func formatCardsDigest(agentID string, due []kb.KBCard) string {
	var b strings.Builder
	b.WriteString("🧠 今日知识卡片：" + strconv.Itoa(len(due)) + " 张待复习")
	for i := 0; i < len(due) && i < 3; i++ {
		b.WriteString("\n· ")
		b.WriteString(due[i].Question)
	}
	if len(due) > 3 {
		b.WriteString("\n· …")
	}
	if base := strings.TrimRight(config.LoadEnv().Gateway.PublicBaseURL, "/"); base != "" {
		b.WriteString("\n开始复习：" + base + "/agents/" + agentID + "/knowledge/cards?review=1")
	}
	return b.String()
}

// cardsPushedToday reports whether the agent already got its digest today.
func cardsPushedToday(ctx context.Context, dbs *store.DBStore, agentID, date string) bool {
	ph := func(n int) string {
		if dbs.Dialect() == "postgres" {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	var one int
	err := dbs.DB().QueryRowContext(ctx,
		`SELECT 1 FROM kb_card_push_runs WHERE agent_id = `+ph(1)+` AND date = `+ph(2),
		agentID, date).Scan(&one)
	return err == nil
}

// stampCardsPush records today's delivered digest (the once-a-day gate).
func stampCardsPush(ctx context.Context, dbs *store.DBStore, agentID, date string, count int, channel string) error {
	ph := func(n int) string {
		if dbs.Dialect() == "postgres" {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	q := `INSERT INTO kb_card_push_runs (agent_id, date, pushed_count, channel, pushed_at)
		VALUES (` + ph(1) + `,` + ph(2) + `,` + ph(3) + `,` + ph(4) + `,CURRENT_TIMESTAMP)
		ON CONFLICT(agent_id, date) DO UPDATE SET pushed_count=excluded.pushed_count,
			channel=excluded.channel, pushed_at=CURRENT_TIMESTAMP`
	_, err := dbs.DB().ExecContext(ctx, q, agentID, date, count, channel)
	return err
}
