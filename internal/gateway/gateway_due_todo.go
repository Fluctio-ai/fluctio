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
// pushes each due-unreminded todo to the agent's reminder channel (default
// wechat), then stamps reminded_at. Target addressing reuses list_channels'
// logic: the channel must be bound+enabled for the agent, and the chat is the
// most recent session on it. Per-agent best-effort; a push failure (or no
// deliverable target) skips MarkTodoReminded so the todo retries next tick.
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
	const reminderChannel = "wechat" // TODO(reminders): make agent-configurable
	for _, ag := range agents {
		ks := kb.NewKBStore(dbs.DB(), dbs.Dialect())
		due, err := ks.ListDueTodos(ctx, ag.ID, windowHours)
		if err != nil {
			slog.Warn("due-todo sweep: list", "agent", ag.ID, "error", err)
			continue
		}
		if len(due) == 0 {
			continue
		}
		accountID, chatID, ok := deliverableTargetForAgent(ctx, dbs, ag.ID, reminderChannel)
		if !ok {
			slog.Warn("due-todo sweep: no deliverable target; skipping (bind+message the agent once)", "agent", ag.ID, "channel", reminderChannel)
			continue
		}
		ch := g.chanMgr.Get(reminderChannel, accountID)
		if ch == nil {
			slog.Warn("due-todo sweep: channel not registered", "agent", ag.ID, "channel", reminderChannel, "account", accountID)
			continue
		}
		for _, t := range due {
			if err := ch.Send(chatID, formatTodoReminder(t)); err != nil {
				slog.Warn("due-todo push failed", "agent", ag.ID, "todo", t.ID, "error", err)
				continue // don't stamp — retry next tick
			}
			slog.Info("due-todo reminder pushed", "agent", ag.ID, "todo", t.ID, "title", t.Title, "channel", reminderChannel, "chat", chatID)
			if err := ks.MarkTodoReminded(ctx, ag.ID, t.ID); err != nil {
				slog.Warn("due-todo sweep: mark reminded", "agent", ag.ID, "todo", t.ID, "error", err)
			}
		}
	}
}

// deliverableTargetForAgent resolves the (accountID, chatID) the reminders sweep
// should push to for one agent on one channel. It reuses list_channels'
// addressing: the channel row must be bound+enabled for the agent, and the
// chat is the most recently updated session on that channel whose account is
// bound (delivery can only route through a registered adapter). Returns
// ok=false when the channel isn't bound or has no session yet.
func deliverableTargetForAgent(ctx context.Context, st *store.DBStore, agentID, channel string) (accountID, chatID string, ok bool) {
	all, err := st.ListAllChannels(ctx)
	if err != nil {
		return "", "", false
	}
	bound := map[string]bool{}
	for _, c := range all {
		if c.AgentID == agentID && c.Enabled && c.Type == channel {
			bound[c.AccountID] = true
		}
	}
	if len(bound) == 0 {
		return "", "", false
	}
	sessions, err := st.ListSessions(ctx, agentID)
	if err != nil {
		return "", "", false
	}
	var bestChat, bestAcct string
	var bestLast time.Time
	for _, s := range sessions {
		if s.Channel != channel || s.ChatID == "" {
			continue
		}
		if !bound[s.AccountID] {
			continue
		}
		if s.UpdatedAt.After(bestLast) {
			bestLast, bestChat, bestAcct = s.UpdatedAt, s.ChatID, s.AccountID
		}
	}
	return bestAcct, bestChat, bestChat != ""
}

// formatTodoReminder builds the IM body for one due todo: title + due time in
// the process timezone so the user reads a local clock.
func formatTodoReminder(t kb.KBSource) string {
	msg := "⏰ 待办提醒：" + t.Title
	if t.EndAt != nil {
		msg += "（截止 " + t.EndAt.Local().Format("2006-01-02 15:04") + "）"
	}
	return msg
}
