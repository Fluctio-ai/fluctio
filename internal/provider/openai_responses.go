package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// OpenAIResponsesProvider implements the Provider interface for the OpenAI
// Responses API (POST {apiBase}/responses) — the successor to Chat
// Completions that GPT-6-class models require for tool calling. See
// https://developers.openai.com/api/docs/guides/migrate-to-responses.
//
// Wire-shape differences vs the chat provider (openai.go):
//
//   - `messages` becomes an `input` array of typed Items. Simple
//     {role, content} shapes are accepted verbatim (EasyInputMessage);
//     tool traffic must use {type:"function_call"} /
//     {type:"function_call_output"} items correlated by `call_id`.
//   - Tool definitions are internally tagged (name/parameters flattened
//     onto the item), not nested under a `function` key.
//   - Only `max_output_tokens` exists (no max_tokens /
//     max_completion_tokens split), and JSON mode moves to
//     text.format.
//   - Streaming uses typed events (response.output_text.delta etc.)
//     instead of choices[].delta, and there is no [DONE] sentinel —
//     response.completed terminates the stream. We still accept [DONE]
//     because compat gateways (vLLM etc.) emit it.
//
// State is managed locally (store:false + full input replay), matching the
// chat provider's architecture: local compaction / memory injection /
// RawAssistant byte-cache would all fight server-side state
// (previous_response_id), and OpenAI bills the whole chain as input tokens
// either way. Reasoning items come back with encrypted_content (via
// include) so store:false turns still replay reasoning for GPT-5-class
// models — same role DeepSeek's reasoning_content plays on the chat wire.
type OpenAIResponsesProvider struct {
	apiKey  string
	apiBase string
	client  *http.Client
}

// NewOpenAIResponses creates a Responses-API provider. apiBase follows the
// same convention as the chat provider: /v1 is part of the base and the
// runtime appends "/responses".
func NewOpenAIResponses(apiKey, apiBase string) *OpenAIResponsesProvider {
	return &OpenAIResponsesProvider{
		apiKey:  apiKey,
		apiBase: NormalizeAPIBase(apiBase, "openai-responses"),
		client:  newLLMHTTPClient(),
	}
}

// responsesRequest is the wire format for POST /responses. Store is
// deliberately NOT omitempty — it must be sent as false every time
// (OpenAI's default is true) so no conversation state accumulates
// server-side.
type responsesRequest struct {
	Model           string                `json:"model"`
	Input           []json.RawMessage     `json:"input"`
	Tools           []responseToolDef     `json:"tools,omitempty"`
	MaxOutputTokens int                   `json:"max_output_tokens,omitempty"`
	Temperature     *float64              `json:"temperature,omitempty"`
	Stream          bool                  `json:"stream"`
	Store           bool                  `json:"store"`
	Include         []string              `json:"include,omitempty"`
	Reasoning       *responsesReasoning   `json:"reasoning,omitempty"`
	Text            *responsesTextConfig  `json:"text,omitempty"`
}

// responsesReasoning maps our WithNoThinking preference onto OpenAI's
// effort dial. Compat implementations that reject the parameter are
// retried without it (see doResponsesRequest).
type responsesReasoning struct {
	Effort string `json:"effort"` // "minimal" | "low" | "medium" | "high"
}

// responsesTextConfig carries the JSON-mode hint. Responses moved it from
// response_format to text.format.
type responsesTextConfig struct {
	Format *responsesFormatObject `json:"format,omitempty"`
}

type responsesFormatObject struct {
	Type string `json:"type"` // "json_object"
}

// responseToolDef is the flattened (internally tagged) function tool
// definition. Responses defaults to attempting strict mode when `strict`
// is omitted — our tool schemas are deliberately loose JSON Schema, so we
// pin strict:false explicitly rather than let the server guess.
type responseToolDef struct {
	Type        string `json:"type"` // "function"
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	Strict      *bool  `json:"strict,omitempty"`
}

// easyInputMessage is the Responses-compatible simple message shape.
// {role, content} items are accepted directly by /responses, which keeps
// the system/user/assistant-text paths identical in shape to the chat
// wire (and preserves our prefix-cache byte layout).
type easyInputMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // JSON string or parts array
}

// responseInputPart is a user-content part. Responses renames the chat
// content types: text→input_text, image_url→input_image.
type responseInputPart struct {
	Type     string    `json:"type"` // "input_text" | "input_image"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// responseFunctionCallItem is an assistant-requested tool call as an
// output Item. `call_id` is what function_call_output items reference —
// it carries the same value as the chat wire's tool_call_id, which is
// what our stored tool messages key on.
type responseFunctionCallItem struct {
	Type      string `json:"type"` // "function_call"
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// responseFunctionCallOutputItem answers a function_call. Output must be
// a plain string, never an array.
type responseFunctionCallOutputItem struct {
	Type   string `json:"type"` // "function_call_output"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// responseReasoningItem is the synthetic reasoning item built from a
// chat-wire reasoning_content. No id / encrypted_content — the server
// accepts the plain text form (verified against DeepSeek).
type responseReasoningItem struct {
	Type    string                  `json:"type"` // "reasoning"
	Content []responseReasoningPart `json:"content"`
}

type responseReasoningPart struct {
	Type string `json:"type"` // "reasoning_text"
	Text string `json:"text"`
}

// toResponseTools flattens the provider-neutral Tool shape into the
// internally-tagged Responses definition. Typed defs — the request marshal
// encodes each tool exactly once.
func toResponseTools(tools []Tool) []responseToolDef {
	if len(tools) == 0 {
		return nil
	}
	strictFalse := false
	out := make([]responseToolDef, 0, len(tools))
	for _, t := range tools {
		out = append(out, responseToolDef{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
			Strict:      &strictFalse,
		})
	}
	return out
}

// callItem renders a ToolCall as a function_call output item. The chat
// wire's tool_call id doubles as the Responses call_id because our stored
// tool messages key on it.
func callItem(tc ToolCall) json.RawMessage {
	return mustJSON(responseFunctionCallItem{
		Type:      "function_call",
		ID:        tc.ID,
		CallID:    tc.ID,
		Name:      tc.Function.Name,
		Arguments: tc.Function.Arguments,
	})
}

// assistantTextItem renders an assistant text turn; content must already
// be a JSON-encoded string (or parts array).
func assistantTextItem(content json.RawMessage) json.RawMessage {
	return mustJSON(easyInputMessage{Role: "assistant", Content: content})
}

// toResponseInput converts provider Messages into the Responses input
// item array. Orphan tool_calls are stripped with the same
// findOrphanToolCalls pass the chat provider uses — OpenAI rejects a
// function_call that has no matching function_call_output, and dirty
// sessions (tool-loop detector break, pre-fix streamed messages) can
// produce exactly that.
func toResponseInput(msgs []Message) []json.RawMessage {
	orphanAssistant, orphanTool := findOrphanToolCalls(msgs)
	out := make([]json.RawMessage, 0, len(msgs))
	for i, m := range msgs {
		if orphanTool[i] {
			continue
		}

		switch m.Role {
		case "tool":
			out = append(out, mustJSON(responseFunctionCallOutputItem{
				Type:   "function_call_output",
				CallID: m.ToolCallID,
				Output: m.Content,
			}))
			continue

		case "assistant":
			// Replay the cached items verbatim when we have them —
			// byte-identical prefix for cache hits, and the encrypted
			// reasoning items survive (required: GPT-5-class models 400
			// when a function_call replay loses its reasoning item).
			// An array JSON unmarshals into []json.RawMessage; the chat
			// wire's object-shaped cache does not — that is the shape
			// discriminator, no separate probe needed.
			if len(m.RawAssistant) > 0 && !orphanAssistant[i] {
				var items []json.RawMessage
				if json.Unmarshal(m.RawAssistant, &items) == nil && len(items) > 0 {
					out = append(out, items...)
					continue
				}
				if rawAssistantHasRole(m.RawAssistant) {
					// Chat-shaped cache from before an apiType switch:
					// decompose into a message item + function_call
					// items. reasoning_content is dropped — the
					// Responses wire has no place for it.
					var am apiMessage
					if json.Unmarshal(m.RawAssistant, &am) == nil && (am.Content != nil || len(am.ToolCalls) > 0) {
						out = append(out, chatShapedAssistantToItems(am)...)
						continue
					}
				}
			}
			// No usable cache (compaction rewrite, anthropic thinking
			// residue, first turn) — build items from parsed fields.
			out = append(out, assistantToItems(m, orphanAssistant[i])...)
			continue

		default: // system, user
			msg := easyInputMessage{Role: m.Role}
			if len(m.ContentParts) > 0 {
				parts := make([]responseInputPart, 0, len(m.ContentParts))
				for _, p := range m.ContentParts {
					switch p.Type {
					case "image_url":
						parts = append(parts, responseInputPart{Type: "input_image", ImageURL: p.ImageURL})
					default:
						parts = append(parts, responseInputPart{Type: "input_text", Text: p.Text})
					}
				}
				msg.Content = mustJSON(parts)
			} else {
				msg.Content = mustJSON(m.Content)
			}
			out = append(out, mustJSON(msg))
		}
	}
	return out
}

// assistantToItems builds Responses items from a parsed assistant
// Message: one message item (padded with a placeholder if empty — strict
// gateways reject empty assistant turns) plus one function_call item per
// tool call. The chat provider's stripping of orphaned tool_calls
// (orphanAssistant) lands here as "drop the tool_calls, keep the text".
func assistantToItems(m Message, stripToolCalls bool) []json.RawMessage {
	content := m.Content
	if content == "" && len(m.ContentParts) == 0 && len(m.ToolCalls) == 0 {
		content = "[internal reasoning]"
	}
	items := []json.RawMessage{assistantTextItem(mustJSON(content))}
	if !stripToolCalls {
		for _, tc := range m.ToolCalls {
			items = append(items, callItem(tc))
		}
	}
	return items
}

// chatShapedAssistantToItems decomposes a cached chat-wire assistant
// message into Responses items. tool_call ids double as call_ids because
// our stored tool messages key on them.
//
// reasoning_content becomes a synthetic reasoning item (reasoning_text
// part): DeepSeek's Responses endpoint enforces thinking-mode turns to
// carry reasoning_text back, exactly like its chat wire's
// reasoning_content rule — and loop_auth's authorized-exec synthetic
// turns pack " " into reasoning_content for precisely that requirement.
// Without the conversion, a thinking session that hits the auth path
// 400s on the next turn ("The reasoning_text in the thinking mode must
// be passed back to the API").
func chatShapedAssistantToItems(am apiMessage) []json.RawMessage {
	var items []json.RawMessage
	if am.ReasoningContent != "" {
		items = append(items, mustJSON(responseReasoningItem{
			Type:    "reasoning",
			Content: []responseReasoningPart{{Type: "reasoning_text", Text: am.ReasoningContent}},
		}))
	}
	if len(am.Content) > 0 && string(am.Content) != `""` {
		items = append(items, assistantTextItem(am.Content))
	}
	for _, tc := range am.ToolCalls {
		items = append(items, callItem(tc))
	}
	if len(items) == 0 {
		items = append(items, assistantTextItem(mustJSON("[internal reasoning]")))
	}
	return items
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return raw
}

// --- streaming event shapes ---

// responsesStreamEvent is a loose union over the typed SSE events. Fields
// that only exist on some event types are simply empty on the others;
// unknown event types are ignored — compat implementations (vLLM, Open
// Responses gateways) emit subsets and extras, so we branch only on what
// we consume.
type responsesStreamEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`    // output_text.delta / reasoning delta events
	ItemID string `json:"item_id"` // function_call_arguments.*
	Args  string `json:"arguments"` // function_call_arguments.done (complete args)
	Item  json.RawMessage `json:"item"` // output_item.added/done
	// Response stays raw: only the terminal events (completed/incomplete/
	// failed) consume it — decoding a value type would snapshot the whole
	// accumulated output on every event that carries a response.
	Response json.RawMessage `json:"response,omitempty"`
	Code     string          `json:"code"`    // error event
	Message  string          `json:"message"` // error event
}

// responsesOutputItem is the loose output-Item probe. Content and Summary
// stay raw — we only consume the scalar envelope fields.
type responsesOutputItem struct {
	Type      string `json:"type"` // "message" | "function_call" | "reasoning" | hosted-tool items
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// responsesFinalBody is the response object decoded (on demand) from the
// terminal events. Output is kept raw so RawAssistant replays stay
// byte-identical to what the server sent.
type responsesFinalBody struct {
	Output []json.RawMessage `json:"output"`
	Usage  *responsesUsage   `json:"usage,omitempty"`
	Error  *struct {
		Code    string `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details,omitempty"`
}

// responsesUsage mirrors the Responses usage block. cached_tokens folds
// into CacheReadTokens the same way the chat provider folds
// prompt_tokens_details.
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
}

func responsesUsageToProvider(u *responsesUsage) Usage {
	if u == nil {
		return Usage{}
	}
	cached := 0
	if u.InputTokensDetails != nil {
		cached = u.InputTokensDetails.CachedTokens
	}
	return foldCachedInputTokens(Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}, cached)
}

// responsesStreamAgg accumulates one response's worth of typed events.
// Shared by Chat (aggregate-only) and ChatStream (forward text deltas,
// emit everything on the final chunk).
type responsesStreamAg struct {
	content   strings.Builder
	reasoning strings.Builder
	calls     []*ToolCall
	callIdx   map[string]int              // item id → index into calls
	args      map[string]*strings.Builder // item id → incremental argument builder
	usage     Usage
	output    []json.RawMessage // final items from response.completed
}

func (a *responsesStreamAg) openCall(item responsesOutputItem) {
	if a.callIdx == nil {
		a.callIdx = map[string]int{}
	}
	if a.args == nil {
		a.args = map[string]*strings.Builder{}
	}
	tc := &ToolCall{ID: item.CallID, Type: "function", Function: FunctionCall{Name: item.Name}}
	a.calls = append(a.calls, tc)
	a.callIdx[item.ID] = len(a.calls) - 1
	a.args[item.ID] = &strings.Builder{}
}

// settleArgs flushes argument builders that never received their
// authoritative done event (compat gateways that skip them) onto the
// tool calls.
func (a *responsesStreamAg) settleArgs() {
	for id, b := range a.args {
		if idx, ok := a.callIdx[id]; ok {
			a.calls[idx].Function.Arguments = b.String()
		}
		delete(a.args, id)
	}
}

func (a *responsesStreamAg) completeCall(item responsesOutputItem) {
	delete(a.args, item.ID)
	if idx, ok := a.callIdx[item.ID]; ok {
		// output_item.done carries the authoritative scalar fields;
		// delta accumulation may have missed pieces (or none at all on
		// gateways that skip the argument-delta events).
		if item.CallID != "" {
			a.calls[idx].ID = item.CallID
		}
		if item.Name != "" {
			a.calls[idx].Function.Name = item.Name
		}
		if item.Arguments != "" {
			a.calls[idx].Function.Arguments = item.Arguments
		}
		return
	}
	a.openCall(item)
	if item.Arguments != "" {
		a.calls[len(a.calls)-1].Function.Arguments = item.Arguments
	}
}

// handleEvent processes one decoded event. It returns the text delta to
// forward ("" when none), whether the stream terminated, and an error for
// failure events.
func (a *responsesStreamAg) handleEvent(ev *responsesStreamEvent) (delta string, terminal bool, err error) {
	switch ev.Type {
	case "response.output_text.delta":
		a.content.WriteString(ev.Delta)
		return ev.Delta, false, nil

	// OpenAI emits reasoning summaries as response.reasoning_summary_text;
	// DeepSeek's Responses implementation streams the raw thinking as
	// response.reasoning_text (matching its content-part type name). Aggregate
	// both into Thinking so memory extraction doesn't silently lose DeepSeek
	// reasoning.
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		a.reasoning.WriteString(ev.Delta)
		return "", false, nil

	case "response.output_item.added":
		var item responsesOutputItem
		if json.Unmarshal(ev.Item, &item) == nil && item.Type == "function_call" {
			a.openCall(item)
		}
		return "", false, nil

	case "response.function_call_arguments.delta":
		// Incremental append into a per-item builder — string += here would
		// be quadratic on the large write_file/edit argument payloads this
		// repo's tools produce, and the accumulation is only a fallback for
		// gateways that skip the authoritative done events anyway.
		if b := a.args[ev.ItemID]; b != nil {
			b.WriteString(ev.Delta)
		}
		return "", false, nil

	case "response.function_call_arguments.done":
		if idx, ok := a.callIdx[ev.ItemID]; ok && ev.Args != "" {
			a.calls[idx].Function.Arguments = ev.Args
			delete(a.args, ev.ItemID)
		}
		return "", false, nil

	case "response.output_item.done":
		var item responsesOutputItem
		if json.Unmarshal(ev.Item, &item) == nil && item.Type == "function_call" {
			a.completeCall(item)
		}
		return "", false, nil

	case "response.completed", "response.incomplete":
		var fin responsesFinalBody
		if len(ev.Response) > 0 {
			_ = json.Unmarshal(ev.Response, &fin)
		}
		a.usage = responsesUsageToProvider(fin.Usage)
		a.output = fin.Output
		if ev.Type == "response.incomplete" && fin.IncompleteDetails != nil {
			slog.Warn("responses stream incomplete", "reason", fin.IncompleteDetails.Reason)
		}
		return "", true, nil

	case "response.failed":
		msg := "response failed"
		var fin responsesFinalBody
		if len(ev.Response) > 0 {
			_ = json.Unmarshal(ev.Response, &fin)
		}
		if fin.Error != nil && fin.Error.Message != "" {
			msg = fin.Error.Message
		}
		return "", true, fmt.Errorf("responses API error: %s", msg)

	case "error":
		msg := ev.Message
		if msg == "" {
			msg = ev.Code
		}
		if msg == "" {
			msg = "unknown stream error"
		}
		return "", true, fmt.Errorf("responses API error: %s", msg)

	default:
		// response.created / in_progress, content_part.*, hosted-tool
		// call events, and anything a compat gateway invents.
		return "", false, nil
	}
}

var (
	sseDataPrefix = []byte("data: ")
	sseDoneMarker = []byte("[DONE]")
)

// scan drives the SSE loop shared by Chat and ChatStream: per-event
// dispatch through the aggregator, text deltas forwarded via onDelta
// (nil to drop). Returns whether a terminal event ended the stream —
// false means [DONE] (compat gateways) or plain EOF.
//
// Works on scanner.Bytes() end to end: scanner.Text() plus
// []byte(data) would copy every event line twice on the per-token hot
// path.
func (a *responsesStreamAg) scan(r io.Reader, onDelta func(string)) (terminal bool, err error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, sseDataPrefix) {
			continue
		}
		data := bytes.TrimPrefix(line, sseDataPrefix)
		if bytes.Equal(data, sseDoneMarker) {
			break
		}

		var ev responsesStreamEvent
		if uerr := json.Unmarshal(data, &ev); uerr != nil {
			slog.Warn("parse responses SSE event", "error", uerr, "data", string(data))
			continue
		}

		delta, term, herr := a.handleEvent(&ev)
		if herr != nil {
			return false, herr
		}
		if delta != "" && onDelta != nil {
			onDelta(delta)
		}
		if term {
			return true, nil
		}
	}
	return false, scanner.Err()
}

// toolCalls copies the accumulated calls out of the aggregator.
func (a *responsesStreamAg) toolCalls() []ToolCall {
	if len(a.calls) == 0 {
		return nil
	}
	out := make([]ToolCall, len(a.calls))
	for i, tc := range a.calls {
		out[i] = *tc
	}
	return out
}

// snapshot materializes the accumulated turn shared by Response and the
// final StreamChunk.
func (a *responsesStreamAg) snapshot() (calls []ToolCall, thinking string, usage Usage, raw json.RawMessage) {
	a.settleArgs()
	return a.toolCalls(), a.reasoning.String(), a.usage, a.rawAssistant()
}

func (a *responsesStreamAg) response() *Response {
	calls, thinking, usage, raw := a.snapshot()
	return &Response{
		Content:      a.content.String(),
		ToolCalls:    calls,
		Thinking:     thinking,
		Usage:        usage,
		RawAssistant: raw,
	}
}

func (a *responsesStreamAg) doneChunk() StreamChunk {
	calls, thinking, usage, raw := a.snapshot()
	return StreamChunk{
		ToolCalls:    calls,
		Done:         true,
		Thinking:     thinking,
		Usage:        usage,
		RawAssistant: raw,
	}
}

// rawAssistant serializes the assistant turn as an output-items array —
// the shape toResponseInput replays. Preferred source is the completed
// response's output verbatim (byte-identical cache prefix + encrypted
// reasoning items). When the stream ended without a terminal event
// (compat gateway [DONE], truncated connection) we rebuild items from
// the accumulated state; the reasoning item is unrecoverable there, so
// GPT-5-class strictness may 400 on the next turn — accepted, since a
// stream that never reaches response.completed is already the failure
// path.
func (a *responsesStreamAg) rawAssistant() json.RawMessage {
	if len(a.output) > 0 {
		if raw, err := json.Marshal(a.output); err == nil {
			return raw
		}
	}
	items := []json.RawMessage{assistantTextItem(mustJSON(a.content.String()))}
	for _, tc := range a.calls {
		items = append(items, callItem(*tc))
	}
	return mustJSON(items)
}

// --- request plumbing ---

// responsesRequestMode tracks the 4xx-driven parameter drops, mirroring
// the chat provider's openAIRequestMode dance.
type responsesRequestMode struct {
	omitTemperature bool
	omitTextFormat  bool // flipped when a 4xx says text.format is rejected
	omitReasoning   bool // flipped when a 4xx says the reasoning param is rejected
	omitInclude     bool // flipped when a 4xx says include/encrypted_content is rejected
}

func initialResponsesRequestMode(model string) responsesRequestMode {
	// Reuse the gpt-5*/o-series temperature policy — those models reject
	// temperature on both wires.
	m := initialOpenAIRequestMode(model)
	return responsesRequestMode{omitTemperature: m.omitTemperature}
}

func (p *OpenAIResponsesProvider) buildRequest(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64, stream bool, mode responsesRequestMode) (*http.Request, error) {
	req := responsesRequest{
		Model:           StripProviderPrefix(model),
		Input:           toResponseInput(messages),
		Tools:           toResponseTools(tools),
		MaxOutputTokens: maxTokens,
		Stream:          stream,
		Store:           false,
	}
	if !mode.omitTemperature {
		req.Temperature = &temperature
	}
	if !mode.omitInclude {
		// Encrypted reasoning keeps store:false turns replayable for
		// reasoning models (see the provider comment).
		req.Include = []string{"reasoning.encrypted_content"}
	}
	if NoThinkingRequested(ctx) && !mode.omitReasoning {
		req.Reasoning = &responsesReasoning{Effort: "minimal"}
	}
	if JSONModeRequested(ctx) && !mode.omitTextFormat {
		req.Text = &responsesTextConfig{Format: &responsesFormatObject{Type: "json_object"}}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := p.apiBase + "/responses"
	slog.Info("openai-responses request", "url", url, "model", req.Model)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	return httpReq, nil
}

func (p *OpenAIResponsesProvider) Chat(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64) (*Response, error) {
	resp, err := p.doResponsesRequest(ctx, messages, tools, model, maxTokens, temperature, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return p.parseSSE(resp.Body)
}

// ChatStream returns a StreamReader that yields chunks as they arrive.
func (p *OpenAIResponsesProvider) ChatStream(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64) (*StreamReader, error) {
	resp, err := p.doResponsesRequest(ctx, messages, tools, model, maxTokens, temperature, true)
	if err != nil {
		return nil, err
	}

	ch := make(chan StreamChunk, 64)
	reader := NewStreamReader(ch)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		agg := &responsesStreamAg{}
		terminal, err := agg.scan(resp.Body, func(delta string) {
			select {
			case ch <- StreamChunk{Content: delta}:
			case <-ctx.Done():
			}
		})
		if err != nil {
			reader.SetErr(err)
			return
		}
		if !terminal {
			// Stream ended without a terminal event — no Done chunk,
			// mirroring the chat provider's [DONE]-never-arrived path.
			return
		}
		select {
		case ch <- agg.doneChunk():
		case <-ctx.Done():
		}
	}()

	return reader, nil
}

func (p *OpenAIResponsesProvider) doResponsesRequest(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64, stream bool) (*http.Response, error) {
	mode := initialResponsesRequestMode(model)
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		httpReq, err := p.buildRequest(ctx, messages, tools, model, maxTokens, temperature, stream, mode)
		if err != nil {
			return nil, err
		}

		resp, err := p.client.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("send request: %w", err)
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body := string(respBody)
		lastErr = &HTTPError{StatusCode: resp.StatusCode, Body: body, Headers: resp.Header}

		// Same retry ladder as the chat provider, minus the
		// max_tokens/max_completion_tokens dance (Responses has exactly
		// one parameter name).
		if !mode.omitTemperature && shouldRetryWithoutTemperature(resp.StatusCode, body) {
			mode.omitTemperature = true
			continue
		}
		if !mode.omitTextFormat && is4xx(resp.StatusCode) && shouldRetryResponsesWithoutTextFormat(body) {
			mode.omitTextFormat = true
			continue
		}
		if !mode.omitReasoning && is4xx(resp.StatusCode) && shouldRetryResponsesWithoutReasoning(body) {
			mode.omitReasoning = true
			continue
		}
		if !mode.omitInclude && is4xx(resp.StatusCode) && shouldRetryResponsesWithoutInclude(body) {
			mode.omitInclude = true
			continue
		}

		return nil, lastErr
	}
	return nil, lastErr
}

func is4xx(status int) bool {
	return status >= 400 && status < 500
}

// shouldRetryResponsesWithoutTextFormat mirrors the chat provider's
// response_format retry, matching the Responses parameter names.
func shouldRetryResponsesWithoutTextFormat(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "text.format") || strings.Contains(lower, "json_object")
}

// shouldRetryResponsesWithoutReasoning catches gateways that don't
// implement the reasoning parameter at all.
func shouldRetryResponsesWithoutReasoning(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "reasoning") && (strings.Contains(lower, "effort") || strings.Contains(lower, "unsupported") || strings.Contains(lower, "unknown"))
}

// shouldRetryResponsesWithoutInclude catches gateways that reject the
// encrypted-reasoning include.
func shouldRetryResponsesWithoutInclude(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "include") && strings.Contains(lower, "encrypted_content")
}

func (p *OpenAIResponsesProvider) parseSSE(reader io.Reader) (*Response, error) {
	agg := &responsesStreamAg{}
	if _, err := agg.scan(reader, nil); err != nil {
		return nil, err
	}
	return agg.response(), nil
}
