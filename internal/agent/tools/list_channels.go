package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// RegisterListChannelsTool registers list_channels, which returns every
// (channel, accountId, chatId) triple this agent can deliver to right now.
// Each row is exactly what create_cron_job's optional target params need,
// so the model copies a row verbatim into a cross-channel schedule.
func RegisterListChannelsTool(r *Registry, st store.Store, agentID string) {
	if r == nil || st == nil || agentID == "" {
		return
	}
	r.Register("list_channels",
		`List every (channel, accountId, chatId) triple this agent can deliver to right now, for passing into create_cron_job's optional channel/accountId/chatId target fields.
Use this when the user wants to send or schedule something to a specific channel/chat (e.g. "也发到微信", "每天在 Telegram 提醒我"): pick the row for the target chat and copy its channel + accountId + chatId verbatim into create_cron_job.
Delivery addressing is the SAME three fields on every IM channel (wechat/telegram/discord/qq/slack/feishu/line): the bot adapter is located by (channel, accountId) and the chat is addressed by chatId — no other id (no session_key, no guild_id) is needed. Only currently bound AND enabled channels are listed, and only chats that have actually messaged the agent (so a usable chatId exists): an unbound/disabled channel has no live adapter, so its chats can't be delivered to and are omitted. If the channel the user names isn't listed, tell them to bind/enable it in channel settings and message the agent once from it, then retry. Each row also carries title/messageCount/lastActive to help tell chats apart. No credentials are ever included.`,
		map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		makeListChannels(st, agentID),
	)
}

// deliverableItem is one chat the agent can push to. The three id fields
// are exactly the union of what create_cron_job's target needs and what
// every IM channel's SendMessage consumes: the adapter is keyed by
// (channel, accountId) and the chat is addressed by chatId. title /
// messageCount / lastActive only help the model distinguish multiple
// chats on the same channel.
type deliverableItem struct {
	Channel      string    `json:"channel"`
	AccountID    string    `json:"accountId"`
	ChatID       string    `json:"chatId"`
	Title        string    `json:"title,omitempty"`
	MessageCount int       `json:"messageCount"`
	LastActive   time.Time `json:"lastActive"`
}

type listChannelsResult struct {
	AgentID     string            `json:"agentId"`
	Deliverable []deliverableItem `json:"deliverable"`
}

func makeListChannels(st store.Store, agentID string) ToolFunc {
	return func(ctx context.Context, _ json.RawMessage) (string, error) {
		// A chat is only deliverable if its bot adapter is currently
		// registered, and the gateway only registers adapters for
		// channels-table rows that are bound AND enabled. Build that
		// allowlist first.
		all, err := st.ListAllChannels(ctx)
		if err != nil {
			return "", fmt.Errorf("list channels: %w", err)
		}
		bound := map[string]bool{}
		for _, ch := range all {
			if ch.AgentID == agentID && ch.Enabled {
				bound[ch.Type+"\x00"+ch.AccountID] = true
			}
		}

		// Collapse sessions into one entry per (channel, accountID, chatId).
		sessions, err := st.ListSessions(ctx, agentID)
		if err != nil {
			return "", fmt.Errorf("list sessions: %w", err)
		}
		type chatKey struct{ channel, accountID, chatID string }
		type agg struct {
			title string
			count int
			last  time.Time
		}
		aggs := map[chatKey]*agg{}
		for _, s := range sessions {
			if s.Channel == "" || s.ChatID == "" {
				continue
			}
			k := chatKey{s.Channel, s.AccountID, s.ChatID}
			a := aggs[k]
			if a == nil {
				a = &agg{}
				aggs[k] = a
			}
			a.count += s.MessageCount
			if s.UpdatedAt.After(a.last) {
				a.last = s.UpdatedAt
				a.title = s.Title // prefer the title from the most recent session
			}
		}

		// Emit one deliverable row per chat whose channel is bound+enabled.
		// Chats on unbound/disabled channels are omitted: their chatId can't
		// be delivered to, so returning it would mislead the model.
		result := listChannelsResult{AgentID: agentID}
		for k, a := range aggs {
			if !bound[k.channel+"\x00"+k.accountID] {
				continue
			}
			result.Deliverable = append(result.Deliverable, deliverableItem{
				Channel:      k.channel,
				AccountID:    k.accountID,
				ChatID:       k.chatID,
				Title:        a.title,
				MessageCount: a.count,
				LastActive:   a.last,
			})
		}
		sort.Slice(result.Deliverable, func(i, j int) bool {
			di, dj := result.Deliverable[i], result.Deliverable[j]
			if di.Channel != dj.Channel {
				return di.Channel < dj.Channel
			}
			if di.AccountID != dj.AccountID {
				return di.AccountID < dj.AccountID
			}
			return di.LastActive.After(dj.LastActive) // most recently active first
		})

		if len(result.Deliverable) == 0 {
			return "No deliverable targets for this agent. If the user names a channel that isn't listed, it is not currently bound/enabled — ask them to bind it in settings and message the agent once from it, then retry.", nil
		}
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}
