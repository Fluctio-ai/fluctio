package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/session"
)

// filterAuthorizedCalls runs the auth gate over a batch of tool calls and
// splits them into ones to execute vs. ones blocked/prompted. The caller
// executes toExec and merges blocked into the results map keyed by tool_use
// id (so every original call still gets a paired tool_result — no orphan
// ids that would 400 the next LLM request).
//
// Prompt-tier calls (ask mode, outside-workspace/dangerous) are parked on
// the session via PushPendingCalls; /yes later moves them to the approved
// batch and drainApprovedPending runs them. Each parked call still gets a
// holding tool_result here so its tool_use id stays paired.
//
// Returns the (possibly empty) description of a call that needs an
// authorization prompt this round — the caller emits the "⚠️ 回复 /yes"
// message once per round. Empty desc means no prompt is needed.
func (a *Agent) filterAuthorizedCalls(sess *session.Session, calls []provider.ToolCall) (toExec []provider.ToolCall, blocked map[string]toolCallResult, promptDesc string) {
	blocked = make(map[string]toolCallResult)
	if a.authGate == nil {
		return calls, blocked, ""
	}
	mode := a.authMode
	if mode == "" {
		mode = AuthModeAsk
	}
	var waiting []provider.ToolCall
	for _, tc := range calls {
		dec := a.authGate.evaluateCall(tc.Function.Name, tc.Function.Arguments, mode)
		switch dec.action {
		case authAllow:
			toExec = append(toExec, tc)
		case authBlock:
			blocked[tc.ID] = toolCallResult{
				toolCallID: tc.ID,
				toolName:   tc.Function.Name,
				result:     denyMessageBypass(dec.reason),
			}
		case authPrompt:
			// Park the call on the session; /yes executes it directly.
			// Emit a holding tool_result so the tool_use id stays paired
			// (no orphan 400) and the LLM knows not to retry immediately.
			waiting = append(waiting, tc)
			blocked[tc.ID] = toolCallResult{
				toolCallID: tc.ID,
				toolName:   tc.Function.Name,
				result: "⚠️ 需要授权：" + dec.reason + "。已请求用户授权，等待用户回复 /yes（执行）/ /no（取消）/ /auto / /yolo。请勿自行重试。\n" +
					"Authorization required: " + dec.reason + ". Waiting for the user to reply " +
					"/yes (run) / /no (cancel) / /auto / /yolo. Do not retry on your own.",
			}
			if promptDesc == "" {
				promptDesc = dec.reason
			}
		}
	}
	if len(waiting) > 0 {
		sess.PushPendingCalls(waiting, promptDesc)
	}
	return toExec, blocked, promptDesc
}

// emitAuthPrompt surfaces the "needs authorization" message to the user.
// On web it sends a structured auth_prompt event the front-end renders as
// tappable buttons; on IM channels (no bubble UI, no SSE) it ALSO pushes
// the prompt text through the outbound bus so it actually reaches the
// chatter's WeChat/QQ/Telegram/etc. — emitEvent only fans out to SSE
// consumers (session_events table + hub), which IM channels never read,
// so without the outbound push an IM user sees nothing, never learns the
// turn is blocked on authorization, and the parked tool_call waits
// forever for a /yes that never comes.
//
// It deliberately does NOT append an assistant message to the running
// message list — inserting one between an assistant's tool_calls and the
// paired tool_results breaks the tool_calls↔tool pairing (LLM APIs reject
// "tool message without preceding tool_calls"). The authorization ask is
// already embedded in the blocked tool_result, which the model sees as a
// normal tool response.
func (a *Agent) emitAuthPrompt(ctx context.Context, desc string, msg bus.InboundMessage) {
	options := []map[string]string{
		{"cmd": "/yes", "label_zh": "授权执行", "label_en": "Approve"},
		{"cmd": "/no", "label_zh": "拒绝", "label_en": "Deny"},
		{"cmd": "/auto", "label_zh": "切到自动拒绝", "label_en": "Switch to auto-deny"},
		{"cmd": "/yolo", "label_zh": "切到全放行", "label_en": "Switch to allow all"},
	}
	emitEvent(ctx, ChatEvent{Type: "auth_prompt", Data: map[string]any{
		"description": desc,
		"options":     options,
	}})
	if msg.Channel == "web" {
		return
	}
	content := "⚠️ 需要授权：" + desc + "\n" +
		"/yes — 授权执行 (Approve)\n" +
		"/no — 拒绝 (Deny)\n" +
		"/auto — 切到自动拒绝 (Auto-deny)\n" +
		"/yolo — 切到全放行 (Allow all)"
	// SSE content event keeps web-compatible clients (and session_events
	// replay) in sync; IM channels never read SSE so the outbound push
	// below is the one that actually delivers the prompt to the chatter.
	emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": content}})
	if isIMChannel(msg.Channel) && a.messageBus != nil {
		// Non-blocking send: a full outbound queue drops the prompt rather
		// than stalling the agent loop — matches the compaction-notice
		// pattern in loop.go. The prompt is best-effort UX; losing it is
		// no worse than the pre-fix status quo (IM saw nothing at all).
		select {
		case a.messageBus.Outbound <- bus.OutboundMessage{
			Channel:   msg.Channel,
			AccountID: msg.AccountID,
			ChatID:    msg.ChatID,
			Text:      content,
		}:
		default:
			slog.Warn("outbound channel full, dropping auth prompt", "agent", a.name, "channel", msg.Channel)
		}
	}
}

// drainApprovedPending executes tool_calls the user just authorized (/yes
// or /yolo re-judge) and folds their results into the working message list
// so the LLM picks up where it left off. Called at the top of the turn —
// the /yes (or /auto / /yolo) message itself drives this turn.
//
// Results are emitted as a fresh assistant(tool_calls)+tool(result) pair
// with new IDs (the original waiting call already has a holding result
// paired to its own ID in history; reusing it would double-pair). The
// LLM sees "the authorized op ran, here's the outcome" and continues.
func (a *Agent) drainApprovedPending(ctx context.Context, sess *session.Session, messages *[]provider.Message) int {
	calls := sess.DrainApprovedPending()
	if len(calls) == 0 {
		return 0
	}
	results := a.engine.executeToolsConcurrently(ctx, a.registry, calls, a.workspacePath)

	// Synthesize a fresh tool_calls assistant message + per-call tool
	// results, so the pair is well-formed regardless of the original IDs.
	// DeepSeek's thinking mode requires reasoning_content to round-trip on
	// every assistant message, so pack it into RawAssistant — without it
	// the next call 400s with "The reasoning_content in the thinking mode
	// must be passed back".
	synthID := "authrun-" + fmt.Sprintf("%d", time.Now().UnixNano())
	var tcs []provider.ToolCall
	for i, tc := range calls {
		id := fmt.Sprintf("%s-%d", synthID, i)
		tcs = append(tcs, provider.ToolCall{ID: id, Type: "function", Function: tc.Function})
	}
	rawAsst := struct {
		Role             string              `json:"role"`
		ReasoningContent string              `json:"reasoning_content"`
		ToolCalls        []provider.ToolCall `json:"tool_calls,omitempty"`
	}{Role: "assistant", ReasoningContent: " ", ToolCalls: tcs}
	rawJSON, _ := json.Marshal(rawAsst)
	asstMsg := provider.Message{Role: "assistant", ToolCalls: tcs, RawAssistant: rawJSON}
	sess.Append(asstMsg)
	*messages = append(*messages, asstMsg)
	for i, r := range results {
		content, _ := extractToolMeta(r.result)
		toolMsg := provider.Message{Role: "tool", Content: content, ToolCallID: tcs[i].ID, Name: calls[i].Function.Name}
		sess.Append(toolMsg)
		*messages = append(*messages, toolMsg)
		emitEvent(ctx, ChatEvent{Type: "tool_result", Data: map[string]any{"id": tcs[i].ID, "name": calls[i].Function.Name, "result": content}})
	}
	return len(results)
}
