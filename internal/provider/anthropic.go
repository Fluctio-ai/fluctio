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
	"os"
	"strings"
)

// AnthropicProvider implements the Provider interface for Anthropic Messages API.
type AnthropicProvider struct {
	apiKey  string
	apiBase string
	client  *http.Client
}

// NewAnthropic creates a new Anthropic Messages API provider.
func NewAnthropic(apiKey, apiBase string) *AnthropicProvider {
	if strings.TrimSpace(apiBase) == "" {
		apiBase = "https://api.anthropic.com"
	}
	return &AnthropicProvider{
		apiKey:  apiKey,
		apiBase: NormalizeAPIBase(apiBase, "anthropic-messages"),
		client:  newLLMHTTPClient(),
	}
}

// anthropicMessage is the wire format for Anthropic Messages API.
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"input_schema"`
}

// anthropicCacheControl marks a content block as a prompt-cache breakpoint.
// Anthropic's prompt caching is EXPLICIT opt-in: without this marker on a
// system or message content block the API does NOT cache any prefix, no
// matter how byte-stable the body is. {type:"ephemeral"} is the only value
// supported today.
//
// TTL defaults to "1h" (FLUCTIO_ANTHROPIC_CACHE_TTL=5m restores the old
// short window). IM turns routinely resume 30-60+ minutes after the last
// request; the 5-minute default expired before every one of those wakes —
// measured 2026-08-28, cross-hour wakes are the largest cache-miss bucket
// (~60% of all misses). z.ai's anthropic endpoint honors ttl:"1h" for real
// (verified: +6min re-read still hits after a ttl=1h write, where a 5m TTL
// would have expired). Providers that don't understand "ttl" ignore the
// field and keep their own default window.
type anthropicCacheControl struct {
	Type string `json:"type"`    // "ephemeral"
	TTL  string `json:"ttl,omitempty"`
}

// anthropicCacheTTL resolves once per process from
// FLUCTIO_ANTHROPIC_CACHE_TTL. "5m" opts back into the old short window;
// anything else (unset included) keeps "1h".
var anthropicCacheTTL = resolveAnthropicCacheTTL(os.Getenv("FLUCTIO_ANTHROPIC_CACHE_TTL"))

func resolveAnthropicCacheTTL(env string) string {
	if env == "5m" {
		return "5m"
	}
	return "1h"
}

// anthropicSystemBlock is the array-of-blocks form of the Anthropic
// `system` field. The plain-string form can't carry a cache_control
// marker, so when caching we always emit the array form with a breakpoint
// on the (single) block so the assembled system prompt is reused across
// turns.
type anthropicSystemBlock struct {
	Type         string                 `json:"type"` // "text"
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicRequest struct {
	Model     string                 `json:"model"`
	Messages  []anthropicMessage     `json:"messages"`
	System    []anthropicSystemBlock `json:"system,omitempty"`
	MaxTokens int                    `json:"max_tokens"`
	Stream    bool                   `json:"stream"`
	Tools     []anthropicTool        `json:"tools,omitempty"`
}

// toAnthropicMessages converts provider Messages to Anthropic wire format.
// Concatenates every system message into one cached system block (see the
// collection site for why the old overwrite was a bug) and returns the rest.
func toAnthropicMessages(msgs []Message) ([]anthropicSystemBlock, []anthropicMessage) {
	var systemParts []string
	var out []anthropicMessage

	// Anthropic rejects any tool_use whose tool_result doesn't appear
	// in the immediately following message. The agent loop can produce
	// this shape when loop detection / cap-reached synthesis injects a
	// system or assistant message between the tool_use and the
	// padOrphanToolResults pad. Reuse the openai-path scanner to flag
	// orphan assistant tool_calls, then sweep the WHOLE message list
	// for any tool replies whose tool_use we just decided to drop —
	// the openai scanner only checks the immediate tool-run, which
	// misses pad results that land after intervening non-tool messages.
	// Session history stays untouched; this only affects wire build.
	orphanAssistant, orphanTool := findOrphanToolCalls(msgs)
	orphanIDs := map[string]bool{}
	for i, m := range msgs {
		if orphanAssistant[i] {
			for _, id := range assistantToolCallIDs(m) {
				orphanIDs[id] = true
			}
		}
	}
	if len(orphanIDs) > 0 {
		for i, m := range msgs {
			if m.Role == "tool" && orphanIDs[m.ToolCallID] {
				orphanTool[i] = true
			}
		}
	}

	for i, m := range msgs {
		if m.Role == "system" {
			// Concatenate, don't overwrite. The agent loop sends 6-7 system
			// messages (main prompt + channel hints + sender + client params
			// + chatbot reminder + cron guidance + conversation-gap note).
			// The old `system = m.Content` kept only the LAST one, silently
			// dropping the whole SOUL/IDENTITY/skills/runtime prompt as soon
			// as any later system message came in.
			if strings.TrimSpace(m.Content) != "" {
				systemParts = append(systemParts, m.Content)
			}
			continue
		}
		if orphanTool[i] {
			continue
		}
		// If stripping orphan tool_calls would leave the assistant
		// message with nothing text-shaped to say (no Content, no
		// ContentParts, no Thinking), drop it entirely. Anthropic
		// rejects content-less messages with "expected a string or a
		// list", so we can't just emit an empty hull.
		//
		// RawAssistant is intentionally NOT part of this guard: when
		// it's non-empty for an orphan turn it captured exactly the
		// tool_use blocks we're about to strip (or an OpenAI-shape
		// blob from a previous provider) — neither replays into a
		// valid Anthropic message, so keeping the message around just
		// to "preserve" RawAssistant produces `content: null` and a
		// 400 ("messages.N.content: Input should be a valid array")
		// on the next user turn. Real thinking content survives via
		// Thinking / thinkingBlockFor, which DOES round-trip.
		if orphanAssistant[i] && m.Content == "" && len(m.ContentParts) == 0 &&
			m.Thinking == "" {
			continue
		}

		am := anthropicMessage{Role: m.Role}

		// Tool results become role "user" with tool_result content blocks.
		// Anthropic requires every tool_result for a parallel tool_use batch
		// to live in the SAME user message — separate user messages, even
		// back-to-back, get rejected with "tool_use ids ... without tool_result
		// blocks immediately after". So coalesce consecutive tool messages
		// into a single user message containing one tool_result block each.
		if m.Role == "tool" {
			block := map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Content,
			}
			if n := len(out); n > 0 && out[n-1].Role == "user" {
				var existing []interface{}
				if err := json.Unmarshal(out[n-1].Content, &existing); err == nil && len(existing) > 0 {
					allToolResults := true
					for _, eb := range existing {
						mp, ok := eb.(map[string]interface{})
						if !ok || mp["type"] != "tool_result" {
							allToolResults = false
							break
						}
					}
					if allToolResults {
						existing = append(existing, block)
						out[n-1].Content, _ = json.Marshal(existing)
						continue
					}
				}
			}
			am.Role = "user"
			am.Content, _ = json.Marshal([]interface{}{block})
			out = append(out, am)
			continue
		}

		// Assistant messages with tool calls. orphanAssistant flags
		// the message whose tool_calls have no matching tool_result
		// run — emit text-only in that case so the API doesn't reject
		// the request for a dangling tool_use.
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && !orphanAssistant[i] {
			var blocks []interface{}
			if tb := thinkingBlockFor(m); tb != nil {
				blocks = append(blocks, tb)
			}
			if m.Content != "" {
				blocks = append(blocks, map[string]interface{}{
					"type": "text",
					"text": m.Content,
				})
			}
			for _, tc := range m.ToolCalls {
				// Anthropic rejects tool_use blocks whose input isn't a
				// JSON object — `Arguments` can land here as "" (model
				// streamed a tool_use with empty input and no
				// input_json_delta events ever fired), as `null`, or as
				// a bare value like a string. Coerce all of those to an
				// empty object so the historical message replays
				// successfully. Real inputs round-trip unchanged.
				input := parseToolInput(tc.Function.Arguments)
				blocks = append(blocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": input,
				})
			}
			am.Content, _ = json.Marshal(blocks)
			out = append(out, am)
			continue
		}

		// Regular text content
		if len(m.ContentParts) > 0 {
			var blocks []interface{}
			if m.Role == "assistant" {
				if tb := thinkingBlockFor(m); tb != nil {
					blocks = append(blocks, tb)
				}
			}
			for _, part := range m.ContentParts {
				if part.Type == "text" {
					blocks = append(blocks, map[string]interface{}{
						"type": "text",
						"text": part.Text,
					})
				} else if part.Type == "image_url" && part.ImageURL != nil {
					blocks = append(blocks, map[string]interface{}{
						"type": "image",
						"source": map[string]string{
							"type": "url",
							"url":  part.ImageURL.URL,
						},
					})
				}
			}
			am.Content, _ = json.Marshal(blocks)
		} else if m.Role == "assistant" {
			var blocks []interface{}
			if tb := thinkingBlockFor(m); tb != nil {
				blocks = append(blocks, tb)
			}
			if m.Content != "" {
				blocks = append(blocks, map[string]interface{}{
					"type": "text",
					"text": m.Content,
				})
			}
			if len(blocks) > 0 {
				am.Content, _ = json.Marshal(blocks)
			} else if m.Content != "" {
				am.Content, _ = json.Marshal(m.Content)
			}
		} else if m.Content != "" {
			am.Content, _ = json.Marshal(m.Content)
		} else {
			// Defensive: a content-less user/system message would marshal
			// to a wire object missing the `content` field, which
			// Anthropic rejects with "expected a string or a list". Send
			// an empty string so it round-trips even if upstream callers
			// produce a degenerate message (e.g. legacy session load
			// pre-dating ContentParts persistence).
			am.Content, _ = json.Marshal("")
		}

		if len(am.Content) == 0 {
			// Some Anthropic-compatible providers (notably z.ai) reject
			// `content: null` even though a degenerate empty assistant
			// message can appear in historical sessions after an aborted
			// or empty streamed turn. The schema accepts a string, so
			// serialize the empty content explicitly instead of letting
			// json.RawMessage's nil value become null.
			am.Content, _ = json.Marshal("")
		}
		out = append(out, am)
	}

	// Prompt-cache breakpoints (Anthropic prompt caching is EXPLICIT opt-in
	// via cache_control markers — no marker, no cache, regardless of how
	// stable the body is). One breakpoint caps the system prompt; another
	// caps the message just before the latest user turn, so system + tools
	// + all prior history cache and only the newest user message (plus any
	// tool result it produces this turn) ships uncached. Markers carry the
	// 1h TTL (see anthropicCacheControl) so IM sessions that wake
	// mid-hour still hit the cached prefix.
	if n := len(out); n >= 2 {
		lastUser := -1
		for i := n - 1; i >= 0; i-- {
			if out[i].Role == "user" {
				lastUser = i
				break
			}
		}
		// Breakpoint on the message immediately BEFORE the latest user
		// turn. If that message has no content to cache (e.g. a degenerate
		// empty assistant hull), walk back to the nearest non-empty one so
		// the breakpoint still lands on something worth caching.
		if lastUser >= 1 {
			for j := lastUser - 1; j >= 0; j-- {
				if addEphemeralCache(out, j) {
					break
				}
			}
		}
	}

	var system []anthropicSystemBlock
	if len(systemParts) > 0 {
		system = []anthropicSystemBlock{{
			Type:         "text",
			Text:         strings.Join(systemParts, "\n\n"),
			CacheControl: &anthropicCacheControl{Type: "ephemeral", TTL: anthropicCacheTTL},
		}}
	}
	return system, out
}

// addEphemeralCache marks the last content block of out[idx] with an
// ephemeral cache_control breakpoint and reports whether it did. Returns
// false (no-op) when idx is out of range or the message has no content to
// cache — a degenerate empty assistant hull (content `""`) is left in its
// wire form so callers that rely on it (and Anthropic, which rejects null
// but accepts "") keep working; the breakpoint just moves to an earlier
// message with real content. Handles the array-of-blocks wire form
// (tool_result / tool_use / multimodal — the common case) and the
// plain-string form (coerced into a text block so the marker has a home).
func addEphemeralCache(out []anthropicMessage, idx int) bool {
	if idx < 0 || idx >= len(out) {
		return false
	}
	cc := map[string]string{"type": "ephemeral", "ttl": anthropicCacheTTL}
	raw := out[idx].Content

	var blocks []map[string]interface{}
	if json.Unmarshal(raw, &blocks) == nil {
		if len(blocks) == 0 {
			return false
		}
		last := blocks[len(blocks)-1]
		if _, ok := last["cache_control"]; !ok {
			last["cache_control"] = cc
		}
		out[idx].Content, _ = json.Marshal(blocks)
		return true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return false
		}
		out[idx].Content, _ = json.Marshal([]map[string]interface{}{
			{"type": "text", "text": s, "cache_control": cc},
		})
		return true
	}
	return false
}

// parseToolInput decodes a stored tool_use Arguments string into the
// JSON object Anthropic's wire format requires. Anything that isn't
// an object — empty string, null, JSON array, bare value — gets
// coerced to the empty object {} so the message replays cleanly.
// Real inputs round-trip unchanged.
//
// Why coerce: when Anthropic streams a tool_use whose input is `{}`,
// it emits the placeholder in `content_block_start` but no
// `input_json_delta` events follow (nothing to stream), so our
// argsJSON accumulator stays "". That gets persisted to
// session_messages.tool_calls and rejected on the next turn replay
// with "tool_use.input: Input should be an object". Coercing at
// wire-build time is cheap and also covers any other future shape
// drift from non-Anthropic providers serving via Claude-compat.
func parseToolInput(raw string) interface{} {
	if raw == "" {
		return map[string]interface{}{}
	}
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return map[string]interface{}{}
	}
	if _, ok := v.(map[string]interface{}); !ok {
		return map[string]interface{}{}
	}
	return v
}

// thinkingBlockFor returns a content-block map for an assistant message's
// prior thinking, or nil if nothing to replay. Extended-thinking models
// (real Anthropic, DeepSeek's /anthropic compat) reject the next turn when
// the prior `content[].thinking` is dropped, so we echo it back verbatim.
func thinkingBlockFor(m Message) map[string]interface{} {
	if m.Role != "assistant" {
		return nil
	}
	// Prefer the raw thinking block we captured on the response — preserves
	// signature for real Anthropic. Falls back to the plain text we stored
	// on Message.Thinking for sessions written before signature capture.
	if len(m.RawAssistant) > 0 {
		var raw map[string]interface{}
		if err := json.Unmarshal(m.RawAssistant, &raw); err == nil {
			if t, _ := raw["type"].(string); t == "thinking" {
				return raw
			}
		}
	}
	if m.Thinking != "" {
		return map[string]interface{}{
			"type":     "thinking",
			"thinking": m.Thinking,
		}
	}
	return nil
}

func (p *AnthropicProvider) buildRequest(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64, stream bool) (*http.Request, error) {
	system, anthropicMsgs := toAnthropicMessages(messages)

	if maxTokens <= 0 {
		maxTokens = 4096
	}

	// Anthropic deprecated `temperature` on extended-thinking-capable
	// models (Opus 4.x, Sonnet 4.5+, Haiku 4.5) — sending it returns a
	// hard 400. Older models accept it but the system default 0.7 is
	// rarely meaningfully tuned per-agent, so drop it on the wire across
	// the board rather than gating per-model.
	_ = temperature
	req := anthropicRequest{
		Model:     StripProviderPrefix(model),
		Messages:  anthropicMsgs,
		System:    system,
		MaxTokens: maxTokens,
		Stream:    stream,
	}

	if len(tools) > 0 {
		for _, t := range tools {
			req.Tools = append(req.Tools, anthropicTool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: t.Function.Parameters,
			})
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := p.apiBase + "/v1/messages"
	slog.Info("anthropic request", "url", url, "model", req.Model)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	return httpReq, nil
}

// Anthropic SSE event types
type anthropicSSEEvent struct {
	Type string `json:"type"`
}

type anthropicContentBlockStart struct {
	Type         string                     `json:"type"`
	Index        int                        `json:"index"`
	ContentBlock anthropicContentBlockEntry `json:"content_block"`
}

type anthropicContentBlockEntry struct {
	Type  string          `json:"type"` // "text" or "tool_use"
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Text  string          `json:"text,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicContentBlockDelta struct {
	Type  string                `json:"type"`
	Index int                   `json:"index"`
	Delta anthropicDeltaContent `json:"delta"`
}

type anthropicDeltaContent struct {
	Type        string `json:"type"` // "text_delta" | "input_json_delta" | "thinking_delta" | "signature_delta"
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
}

// anthropicUsage mirrors the `usage` field returned by Anthropic Messages.
// message_start carries the input tokens (and prompt-cache breakdown);
// message_delta carries the final output_tokens. We capture both and
// expose the totals on the Response / StreamChunk's provider.Usage.
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type anthropicMessageStart struct {
	Message struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
}

type anthropicMessageDelta struct {
	Usage anthropicUsage `json:"usage"`
}

func (p *AnthropicProvider) Chat(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64) (*Response, error) {
	httpReq, err := p.buildRequest(ctx, messages, tools, model, maxTokens, temperature, true)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: string(respBody), Headers: resp.Header}
	}

	return p.parseSSE(resp.Body)
}

func (p *AnthropicProvider) ChatStream(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64) (*StreamReader, error) {
	httpReq, err := p.buildRequest(ctx, messages, tools, model, maxTokens, temperature, true)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: string(respBody), Headers: resp.Header}
	}

	ch := make(chan StreamChunk, 64)
	reader := NewStreamReader(ch)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		type blockState struct {
			blockType string // "text" | "tool_use" | "thinking"
			id        string
			name      string
			argsJSON  strings.Builder
			thinking  strings.Builder
			signature string
		}
		blocks := make(map[int]*blockState)
		var usage Usage

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")

			var event anthropicSSEEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}

			switch event.Type {
			case "message_start":
				var ms anthropicMessageStart
				if json.Unmarshal([]byte(data), &ms) == nil {
					usage.InputTokens = ms.Message.Usage.InputTokens
					usage.CacheReadTokens = ms.Message.Usage.CacheReadInputTokens
					usage.CacheCreationTokens = ms.Message.Usage.CacheCreationInputTokens
				}
			case "message_delta":
				var md anthropicMessageDelta
				if json.Unmarshal([]byte(data), &md) == nil {
					if md.Usage.OutputTokens > 0 {
						usage.OutputTokens = md.Usage.OutputTokens
					}
					// Some Anthropic-compat backends (Zhipu GLM) carry the
					// full usage — including input_tokens and the prompt-
					// cache breakdown — on the final message_delta, leaving
					// message_start's usage as zeros. Backfill so metering
					// and cache-hit observability work on those backends.
					// On native Anthropic message_delta these fields are 0,
					// so the guards make this a no-op there.
					if md.Usage.InputTokens > 0 {
						usage.InputTokens = md.Usage.InputTokens
					}
					if md.Usage.CacheReadInputTokens > 0 {
						usage.CacheReadTokens = md.Usage.CacheReadInputTokens
					}
					if md.Usage.CacheCreationInputTokens > 0 {
						usage.CacheCreationTokens = md.Usage.CacheCreationInputTokens
					}
				}
			case "content_block_start":
				var cbs anthropicContentBlockStart
				if json.Unmarshal([]byte(data), &cbs) == nil {
					blocks[cbs.Index] = &blockState{
						blockType: cbs.ContentBlock.Type,
						id:        cbs.ContentBlock.ID,
						name:      cbs.ContentBlock.Name,
					}
					if cbs.ContentBlock.Text != "" {
						select {
						case ch <- StreamChunk{Content: cbs.ContentBlock.Text}:
						case <-ctx.Done():
							return
						}
					}
				}

			case "content_block_delta":
				var cbd anthropicContentBlockDelta
				if json.Unmarshal([]byte(data), &cbd) == nil {
					switch cbd.Delta.Type {
					case "text_delta":
						if cbd.Delta.Text != "" {
							select {
							case ch <- StreamChunk{Content: cbd.Delta.Text}:
							case <-ctx.Done():
								return
							}
						}
					case "input_json_delta":
						if bs, ok := blocks[cbd.Index]; ok {
							bs.argsJSON.WriteString(cbd.Delta.PartialJSON)
						}
					case "thinking_delta":
						if bs, ok := blocks[cbd.Index]; ok {
							bs.thinking.WriteString(cbd.Delta.Thinking)
						}
					case "signature_delta":
						if bs, ok := blocks[cbd.Index]; ok {
							bs.signature = cbd.Delta.Signature
						}
					}
				}

			case "message_stop":
				var toolCalls []ToolCall
				var thinkingText, thinkingSig string
				for i := 0; i < len(blocks); i++ {
					bs, ok := blocks[i]
					if !ok {
						continue
					}
					switch bs.blockType {
					case "tool_use":
						toolCalls = append(toolCalls, ToolCall{
							ID:   bs.id,
							Type: "function",
							Function: FunctionCall{
								Name:      bs.name,
								Arguments: bs.argsJSON.String(),
							},
						})
					case "thinking":
						if t := bs.thinking.String(); t != "" {
							thinkingText = t
						}
						if bs.signature != "" {
							thinkingSig = bs.signature
						}
					}
				}
				select {
				case ch <- StreamChunk{
					ToolCalls:         toolCalls,
					Thinking:          thinkingText,
					ThinkingSignature: thinkingSig,
					Usage:             usage,
					Done:              true,
				}:
				case <-ctx.Done():
				}
				return
			}
		}

		if err := scanner.Err(); err != nil {
			reader.SetErr(fmt.Errorf("read stream: %w", err))
		}
	}()

	return reader, nil
}

func (p *AnthropicProvider) parseSSE(body io.Reader) (*Response, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var contentBuilder strings.Builder

	type blockState struct {
		blockType string
		id        string
		name      string
		argsJSON  strings.Builder
		thinking  strings.Builder
		signature string
	}
	blocks := make(map[int]*blockState)
	var usage Usage

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var event anthropicSSEEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			slog.Warn("parse anthropic SSE event", "error", err, "data", data)
			continue
		}

		switch event.Type {
		case "message_start":
			var ms anthropicMessageStart
			if json.Unmarshal([]byte(data), &ms) == nil {
				usage.InputTokens = ms.Message.Usage.InputTokens
				usage.CacheReadTokens = ms.Message.Usage.CacheReadInputTokens
				usage.CacheCreationTokens = ms.Message.Usage.CacheCreationInputTokens
			}
		case "message_delta":
			var md anthropicMessageDelta
			if json.Unmarshal([]byte(data), &md) == nil {
				if md.Usage.OutputTokens > 0 {
					usage.OutputTokens = md.Usage.OutputTokens
				}
				// Backfill input + cache breakdown from message_delta for
				// Anthropic-compat backends (Zhipu GLM) that only surface
				// real counts here. No-op on native Anthropic (fields are 0).
				if md.Usage.InputTokens > 0 {
					usage.InputTokens = md.Usage.InputTokens
				}
				if md.Usage.CacheReadInputTokens > 0 {
					usage.CacheReadTokens = md.Usage.CacheReadInputTokens
				}
				if md.Usage.CacheCreationInputTokens > 0 {
					usage.CacheCreationTokens = md.Usage.CacheCreationInputTokens
				}
			}
		case "content_block_start":
			var cbs anthropicContentBlockStart
			if json.Unmarshal([]byte(data), &cbs) == nil {
				blocks[cbs.Index] = &blockState{
					blockType: cbs.ContentBlock.Type,
					id:        cbs.ContentBlock.ID,
					name:      cbs.ContentBlock.Name,
				}
				if cbs.ContentBlock.Text != "" {
					contentBuilder.WriteString(cbs.ContentBlock.Text)
				}
			}

		case "content_block_delta":
			var cbd anthropicContentBlockDelta
			if json.Unmarshal([]byte(data), &cbd) == nil {
				switch cbd.Delta.Type {
				case "text_delta":
					contentBuilder.WriteString(cbd.Delta.Text)
				case "input_json_delta":
					if bs, ok := blocks[cbd.Index]; ok {
						bs.argsJSON.WriteString(cbd.Delta.PartialJSON)
					}
				case "thinking_delta":
					if bs, ok := blocks[cbd.Index]; ok {
						bs.thinking.WriteString(cbd.Delta.Thinking)
					}
				case "signature_delta":
					if bs, ok := blocks[cbd.Index]; ok {
						bs.signature = cbd.Delta.Signature
					}
				}
			}

		case "message_stop":
			// Done
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}

	result := &Response{
		Content: contentBuilder.String(),
		Usage:   usage,
	}
	var thinkingBuilder strings.Builder
	var thinkingSig string
	for i := 0; i < len(blocks); i++ {
		bs, ok := blocks[i]
		if !ok {
			continue
		}
		switch bs.blockType {
		case "tool_use":
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:   bs.id,
				Type: "function",
				Function: FunctionCall{
					Name:      bs.name,
					Arguments: bs.argsJSON.String(),
				},
			})
		case "thinking":
			if t := bs.thinking.String(); t != "" {
				thinkingBuilder.WriteString(t)
			}
			if bs.signature != "" {
				thinkingSig = bs.signature
			}
		}
	}
	if thinking := thinkingBuilder.String(); thinking != "" {
		result.Thinking = thinking
		// DeepSeek's Anthropic-compat endpoint (and real Anthropic extended
		// thinking) requires the thinking block to be echoed verbatim on the
		// next turn. Pack {thinking, signature} into RawAssistant so
		// toAnthropicMessages can replay it as a content block.
		type thinkingBlock struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature,omitempty"`
		}
		if raw, err := json.Marshal(thinkingBlock{
			Type:      "thinking",
			Thinking:  thinking,
			Signature: thinkingSig,
		}); err == nil {
			result.RawAssistant = raw
		}
	}

	// Fallback: some non-Claude models served via anthropic-compat
	// endpoints (e.g. MiMo, DeepSeek-flash) emit Claude-style tool-call
	// XML inside a text content block. Two failure modes we need to
	// guard separately:
	//
	//   1. XML INSTEAD OF a structured tool_use → we have to synthesize
	//      tool calls from the XML, otherwise the agent loop sees a
	//      text-only response and treats it as a final answer.
	//
	//   2. XML ALONGSIDE a structured tool_use (DeepSeek-flash echoes
	//      its intended call as text every iteration) → the structural
	//      calls already drive execution, but the textual DSML rides
	//      along as assistant.content. That bloats every turn's prompt
	//      and, worse, in the subagent cap-reached forced-delivery path
	//      it ships back to the parent as the subagent's "final answer"
	//      — surfacing as raw DSML in the chat UI.
	//
	// So strip the XML from content unconditionally; only synthesize
	// tool calls when there aren't already structured ones to dispatch.
	if cleaned, calls := extractLeakedToolCalls(result.Content); cleaned != result.Content {
		result.Content = cleaned
		if len(result.ToolCalls) == 0 && len(calls) > 0 {
			slog.Warn("recovered leaked tool-call XML from text content",
				"count", len(calls))
			result.ToolCalls = calls
		} else if len(calls) > 0 {
			slog.Debug("stripped leaked tool-call XML echoing a structured tool_use",
				"echo_count", len(calls), "structured_count", len(result.ToolCalls))
		}
	}

	return result, nil
}
