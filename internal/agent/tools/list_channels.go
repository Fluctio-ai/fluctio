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
// chat this agent can deliver to right now as one self-contained row per
// conversation. Each row is exactly what create_cron_job's optional target
// params need, so the model copies a row verbatim into a cross-channel
// schedule.
func RegisterListChannelsTool(r *Registry, st store.Store, agentID string) {
	if r == nil || st == nil || agentID == "" {
		return
	}
	r.Register("list_channels",
		`List every chat this agent can deliver to right now, one row per conversation, for passing into create_cron_job's optional channel/accountId/chatId target fields.
Use this when the user wants to send or schedule something to a specific channel/chat (e.g. "也发到微信", "每天在 Telegram 提醒我"): pick the row for the target conversation and copy its channel + accountId + chatId verbatim into create_cron_job.
Rows are de-duplicated by conversation: a /new in the same chat starts a fresh session (and on some channels a new account_id) but does NOT create extra rows — only one row per (channel, chatId). Within a conversation, the row reflects the most recent session whose bot account is currently bound+enabled, because delivery can only route through a registered adapter. Chats with no bound adapter are omitted (they can't be delivered to). Delivery addressing is the same three fields on every IM channel (wechat/telegram/discord/qq/slack/feishu/line) — no session_key or guild_id is needed. If the channel the user names isn't listed, tell them to bind/enable it in channel settings and message the agent once from it, then retry. Each row also carries title/messageCount/lastActive to help tell conversations apart. No credentials are ever included.`,
		map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		makeListChannels(st, agentID),
	)
}

// deliverableItem is one conversation the agent can push to. The three id
// fields are exactly what create_cron_job's target needs and what every IM
// channel's SendMessage consumes: the adapter is keyed by (channel,
// accountId) and the chat is addressed by chatId. title / messageCount /
// lastActive only help the model distinguish multiple conversations.
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
		// channels-table rows that are bound AND enabled.
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

		sessions, err := st.ListSessions(ctx, agentID)
		if err != nil {
			return "", fmt.Errorf("list sessions: %w", err)
		}

		// Group sessions by (channel, chatId). A /new in the same chat
		// starts a fresh session_key — and on some channels a fresh
		// account_id — but it's the SAME upstream conversation, so it must
		// collapse to one deliverable row, not many. Within a group, pick
		// the most recent session whose account is bound+enabled: that's
		// the adapter the scheduler will actually route through. Sessions
		// on unbound accounts (e.g. an older rebound bot) are skipped.
		type gKey struct{ channel, chatID string }
		type sess struct {
			accountID string
			title     string
			count     int
			last      time.Time
		}
		groups := map[gKey][]sess{}
		for _, s := range sessions {
			if s.Channel == "" || s.ChatID == "" {
				continue
			}
			k := gKey{s.Channel, s.ChatID}
			groups[k] = append(groups[k], sess{s.AccountID, s.Title, s.MessageCount, s.UpdatedAt})
		}

		result := listChannelsResult{AgentID: agentID}
		for k, ss := range groups {
			sort.Slice(ss, func(i, j int) bool { return ss[i].last.After(ss[j].last) }) // newest first
			var pick *sess
			for i := range ss {
				if bound[k.channel+"\x00"+ss[i].accountID] {
					pick = &ss[i]
					break
				}
			}
			if pick == nil {
				continue // no bound adapter for this conversation → can't deliver
			}
			result.Deliverable = append(result.Deliverable, deliverableItem{
				Channel:      k.channel,
				AccountID:    pick.accountID,
				ChatID:       k.chatID,
				Title:        pick.title,
				MessageCount: pick.count,
				LastActive:   pick.last,
			})
		}
		sort.Slice(result.Deliverable, func(i, j int) bool {
			di, dj := result.Deliverable[i], result.Deliverable[j]
			if di.Channel != dj.Channel {
				return di.Channel < dj.Channel
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
