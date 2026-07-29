package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// RegisterListChannelsTool registers list_channels, which lets the agent
// discover the IM channels bound to it and the chat IDs that have
// interacted with it on each channel. The agent uses this to address a
// specific channel/chat when scheduling cross-channel delivery — e.g.
// create_cron_job targeting WeChat while the current conversation is on
// QQ. Only the agent's own bound channels are listed, and credentials
// (bot tokens) are never exposed to the model.
func RegisterListChannelsTool(r *Registry, st store.Store, agentID string) {
	if r == nil || st == nil || agentID == "" {
		return
	}
	r.Register("list_channels",
		"List every IM channel bound to this agent, plus the chat IDs that have messaged it on each channel. "+
			"Use this whenever the user wants to send or schedule something to a specific channel/chat other than the current one "+
			"(e.g. \"也发到微信\", \"每天在 Telegram 提醒我\", \"推送到飞书群\"), so you can look up the exact channel + accountId + chatId "+
			"to pass into create_cron_job's optional channel/accountId/chatId fields. Returns each channel's type, accountId, "+
			"enabled flag, and the chats under it (chatId, title, messageCount, lastActive). Sensitive credentials are never included.",
		map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		makeListChannels(st, agentID),
	)
}

// channelChat is one chat (group or DM) that has talked to the agent on a
// given channel. The same chatId can span several session keys (a chatter
// typing /new starts a fresh session in the same chat); we collapse those
// to one entry, keeping the most recently active session's title/time and
// summing message counts across sessions.
type channelChat struct {
	ChatID       string    `json:"chatId"`
	Title        string    `json:"title,omitempty"`
	MessageCount int       `json:"messageCount"`
	LastActive   time.Time `json:"lastActive"`
}

type channelEntry struct {
	Channel   string        `json:"channel"`
	AccountID string        `json:"accountId,omitempty"`
	Enabled   bool          `json:"enabled"`
	Chats     []channelChat `json:"chats,omitempty"`
}

type listChannelsResult struct {
	AgentID  string         `json:"agentId"`
	Channels []channelEntry `json:"channels"`
}

func makeListChannels(st store.Store, agentID string) ToolFunc {
	return func(ctx context.Context, _ json.RawMessage) (string, error) {
		// ListAllChannels + filter by agentID so every binding on this
		// agent is covered regardless of who bound it (owner vs. a system/
		// public row with user_id=''). ListChannels(userID, agentID) only
		// matches one user_id, and there is no agent-only query, so the
		// full scan (small set in single-user mode) is the reliable path.
		all, err := st.ListAllChannels(ctx)
		if err != nil {
			return "", fmt.Errorf("list channels: %w", err)
		}
		var channels []store.ChannelRecord
		for _, ch := range all {
			if ch.AgentID == agentID {
				channels = append(channels, ch)
			}
		}
		sessions, err := st.ListSessions(ctx, agentID)
		if err != nil {
			return "", fmt.Errorf("list sessions: %w", err)
		}

		// Collapse sessions into one entry per (channel, accountID, chatId).
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

		// Index chats by (channel, accountID) for attachment to channels.
		chatsByCA := map[string][]channelChat{}
		for k, a := range aggs {
			ca := k.channel + "\x00" + k.accountID
			chatsByCA[ca] = append(chatsByCA[ca], channelChat{
				ChatID: k.chatID, Title: a.title, MessageCount: a.count, LastActive: a.last,
			})
		}
		for ca := range chatsByCA {
			cs := chatsByCA[ca]
			sort.Slice(cs, func(i, j int) bool { return cs[i].LastActive.After(cs[j].LastActive) })
		}

		result := listChannelsResult{AgentID: agentID}
		for _, ch := range channels {
			ca := ch.Type + "\x00" + ch.AccountID
			result.Channels = append(result.Channels, channelEntry{
				Channel:   ch.Type,
				AccountID: ch.AccountID,
				Enabled:   ch.Enabled,
				Chats:     chatsByCA[ca],
			})
		}
		sort.Slice(result.Channels, func(i, j int) bool {
			if result.Channels[i].Channel != result.Channels[j].Channel {
				return result.Channels[i].Channel < result.Channels[j].Channel
			}
			return result.Channels[i].AccountID < result.Channels[j].AccountID
		})

		if len(result.Channels) == 0 {
			return "No IM channels bound to this agent yet.", nil
		}
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}
