package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
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
// wechat), archives it hidden for web visibility (see archivePushNotice),
// then stamps reminded_at. Target addressing reuses list_channels' logic: the
// channel must be bound+enabled for the agent, and the chat is the most
// recent session on it. Per-agent best-effort; a push failure (or no
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
	for _, ag := range agents {
		channel := reminderChannelFor(ag)
		ks := kb.NewKBStore(dbs.DB(), dbs.Dialect())
		due, err := ks.ListDueTodos(ctx, ag.ID, windowHours)
		if err != nil {
			slog.Warn("due-todo sweep: list", "agent", ag.ID, "error", err)
			continue
		}
		if len(due) == 0 {
			continue
		}
		accountID, chatID, sessionKey, ok := deliverableTargetForAgent(ctx, dbs, ag.ID, channel)
		if !ok {
			slog.Warn("due-todo sweep: no deliverable target; skipping (bind+message the agent once)", "agent", ag.ID, "channel", channel)
			continue
		}
		ch := g.chanMgr.Get(channel, accountID)
		if ch == nil {
			slog.Warn("due-todo sweep: channel not registered", "agent", ag.ID, "channel", channel, "account", accountID)
			continue
		}
		for _, t := range due {
			text := formatTodoReminder(t)
			if err := ch.Send(chatID, text); err != nil {
				slog.Warn("due-todo push failed", "agent", ag.ID, "todo", t.ID, "error", err)
				continue // don't stamp — retry next tick
			}
			if err := archivePushNotice(ctx, dbs, ag.ID, sessionKey, text, "todo_reminder"); err != nil {
				// IM delivery already succeeded — log only, still stamp so
				// the reminder doesn't re-fire for a lost web bubble.
				slog.Warn("due-todo sweep: archive notice", "agent", ag.ID, "todo", t.ID, "error", err)
			}
			slog.Info("due-todo reminder pushed", "agent", ag.ID, "todo", t.ID, "title", t.Title, "channel", channel, "chat", chatID)
			if err := ks.MarkTodoReminded(ctx, ag.ID, t.ID); err != nil {
				slog.Warn("due-todo sweep: mark reminded", "agent", ag.ID, "todo", t.ID, "error", err)
			}
		}
	}
}

// reminderChannelFor reads the agent's configured reminder channel from its KB
// config (AgentRecord.Config["kb"].ReminderChannel), defaulting to wechat when
// unset. Config["kb"] round-trips through interface{} (JSON-loaded map), so
// marshal+unmarshal normalizes it into config.AgentKBCfg.
func reminderChannelFor(ag store.AgentRecord) string {
	if cfgAny, ok := ag.Config["kb"]; ok && cfgAny != nil {
		var kbCfg config.AgentKBCfg
		if b, err := json.Marshal(cfgAny); err == nil {
			_ = json.Unmarshal(b, &kbCfg)
		}
		if kbCfg.ReminderChannel != "" {
			return kbCfg.ReminderChannel
		}
	}
	return "wechat"
}

// archivePushNotice writes a delivered IM push (cards digest, todo
// reminder) into the target session's archive with llm_visible=0 — the
// regex-hook FeedToLLM=false shape: web history shows the notification
// bubble while the LLM working set, summary and recall never see it (all
// three filter on llm_visible / the working set is never touched by a
// direct archive append). Origin must stay empty — WebChatHistory skips
// rows whose Origin != OriginUser, which would hide the notice from the
// very surface this write exists for. The pushNotice kind marker
// distinguishes these rows from genuine assistant turns when auditing.
func archivePushNotice(ctx context.Context, dbs *store.DBStore, agentID, sessionKey, text, kind string) error {
	return dbs.AppendSessionMessage(ctx, agentID, sessionKey, store.SessionMessage{
		Role:       "assistant",
		Content:    text,
		Metadata:   map[string]any{"pushNotice": kind},
		LLMVisible: false,
	})
}

// deliverableTargetForAgent resolves the (accountID, chatID, sessionKey) the
// reminders / cards-digest sweeps should push to for one agent on one channel.
// It reuses list_channels' addressing: the channel row must be bound+enabled
// for the agent, and the chat is the most recently updated session on that
// channel whose account is bound (delivery can only route through a registered
// adapter). Returns ok=false when the channel isn't bound or has no session yet.
func deliverableTargetForAgent(ctx context.Context, st *store.DBStore, agentID, channel string) (accountID, chatID, sessionKey string, ok bool) {
	all, err := st.ListAllChannels(ctx)
	if err != nil {
		return "", "", "", false
	}
	bound := map[string]bool{}
	for _, c := range all {
		if c.AgentID == agentID && c.Enabled && c.Type == channel {
			bound[c.AccountID] = true
		}
	}
	if len(bound) == 0 {
		return "", "", "", false
	}
	sessions, err := st.ListSessions(ctx, agentID)
	if err != nil {
		return "", "", "", false
	}
	var bestChat, bestAcct, bestKey string
	var bestLast time.Time
	for _, s := range sessions {
		if s.Channel != channel || s.ChatID == "" {
			continue
		}
		if !bound[s.AccountID] {
			continue
		}
		if s.UpdatedAt.After(bestLast) {
			bestLast, bestChat, bestAcct, bestKey = s.UpdatedAt, s.ChatID, s.AccountID, s.Key
		}
	}
	return bestAcct, bestChat, bestKey, bestChat != ""
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
