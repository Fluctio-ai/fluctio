package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// RegisterListChannelsTool registers list_channels, which returns only
// channels the agent can actually deliver to right now (bound AND enabled)
// with each channel's chat IDs from past sessions. Credentials are never
// exposed to the model.
func RegisterListChannelsTool(r *Registry, st store.Store, agentID string) {
	if r == nil || st == nil || agentID == "" {
		return
	}
	r.Register("list_channels",
		`List the channels this agent can actually deliver to right now (only bound + enabled ones), with each channel's chat IDs drawn from past sessions.
Use this when the user wants to send or schedule something to a specific channel/chat (e.g. "也发到微信", "每天在 Telegram 提醒我"): look up the channel + accountId + chatId to pass into create_cron_job's optional channel/accountId/chatId fields.
Only currently bound AND enabled channels are listed. A channel whose bot binding was removed or disabled has no live adapter registered, so its chat IDs cannot be delivered to and are NOT returned — returning them would give the agent ids it cannot use. If the channel the user names isn't listed, tell them to bind/enable it in channel settings first, then have them message the agent once from it so a chat_id is created. Returns channel, accountId, and chats (chatId, title, messageCount, lastActive). No credentials.`,
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
	Chats     []channelChat `json:"chats,omitempty"`
}

type listChannelsResult struct {
	AgentID  string         `json:"agentId"`
	Channels []channelEntry `json:"channels"`
}

func makeListChannels(st store.Store, agentID string) ToolFunc {
	return func(ctx context.Context, _ json.RawMessage) (string, error) {
		// Only channels currently bound AND enabled can actually receive a
		// delivery: the gateway registers adapters from the channels table,
		// and a disabled/unbound channel has no adapter to carry the
		// outbound. Listing anything else hands the agent ids it cannot
		// push to, so filter strictly.
		all, err := st.ListAllChannels(ctx)
		if err != nil {
			return "", fmt.Errorf("list channels: %w", err)
		}
		var bound []store.ChannelRecord
		for _, ch := range all {
			if ch.AgentID == agentID && ch.Enabled {
				bound = append(bound, ch)
			}
		}

		// Collapse each session into one chat per (channel, accountID, chatId).
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
		for _, ch := range bound {
			ca := ch.Type + "\x00" + ch.AccountID
			result.Channels = append(result.Channels, channelEntry{
				Channel:   ch.Type,
				AccountID: ch.AccountID,
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
			return "No deliverable channels for this agent. If the user names a channel that isn't listed, it is not currently bound/enabled — ask them to bind it in settings and message the agent once from it, then retry.", nil
		}
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}
