package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/fluctio-ai/fluctio/internal/bus"
)

// messageImageMarkdownRe detects image markdown (![alt](src)) in the text
// arg. Used to refuse sending via this text-only tool and nudge the model
// to put images in its auto-delivered reply instead.
var messageImageMarkdownRe = regexp.MustCompile(`!\[[^\]]*\]\(`)

type messageArgs struct {
	Channel   string `json:"channel"`
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	AccountID string `json:"account"`
}

// RegisterMessage registers the message tool with the given message bus.
// allowSplitFn (optional) is consulted on every send to stamp
// OutboundMessage.AllowSplit — controls whether the WeChat adapter will
// honor SplitMessageMarker for multi-bubble output. Pass nil if the
// caller doesn't care (e.g. tests, non-WeChat-bound deployments) —
// AllowSplit defaults to false in that case.
func RegisterMessage(r *Registry, mb *bus.MessageBus, allowSplitFn func() bool) {
	r.tools["message"] = registeredTool{
		def: r.tools["message"].def,
		fn:  makeMessageTool(mb, allowSplitFn),
	}
}

func registerMessage(r *Registry) {
	// Register with a placeholder; will be re-registered with actual bus later.
	r.Register("message", "Send a PLAIN-TEXT message to a specific channel + chat_id. Text-only — CANNOT send images or files. Your normal reply is already auto-delivered to the user you are talking with (including any ![](/workspace/<file>.png) images), so do NOT call this tool to reply to the current user or to deliver images. Use it only when you must push plain text to a specific channel/chat.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"channel": map[string]interface{}{
				"type":        "string",
				"description": "Target channel (e.g. 'wechat', 'telegram')",
			},
			"chat_id": map[string]interface{}{
				"type":        "string",
				"description": "Full target chat ID (same form as in inbound messages)",
			},
			"account": map[string]interface{}{
				"type":        "string",
				"description": "Source account ID for the channel. Auto-filled when you pick a session in the workflow editor. Required when a channel has multiple accounts; leave empty for single-account channels.",
			},
			"text": map[string]interface{}{
				"type":        "string",
				"description": "Plain text only (no images — image markdown will not render)",
			},
		},
		"required": []string{"channel", "chat_id", "text"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		return "", fmt.Errorf("message bus not initialized")
	})
}

func makeMessageTool(mb *bus.MessageBus, allowSplitFn func() bool) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args messageArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		// This tool ships text as-is — no media upload happens here (only
		// the agent's normal reply gets images extracted/uploaded). An
		// image markdown ref won't render on IM channels, so refuse and
		// point the model at its reply instead of burning a turn.
		if messageImageMarkdownRe.MatchString(args.Text) {
			return "message tool is text-only — image markdown here won't render on IM channels. Put the image in your normal reply as ![](/workspace/<file>.png) and it is auto-delivered; no need to call this tool.", nil
		}

		allowSplit := false
		if allowSplitFn != nil {
			allowSplit = allowSplitFn()
		}

		mb.Outbound <- bus.OutboundMessage{
			Channel:    args.Channel,
			AccountID:  args.AccountID,
			ChatID:     args.ChatID,
			Text:       args.Text,
			AllowSplit: allowSplit,
		}

		return "Message sent", nil
	}
}
