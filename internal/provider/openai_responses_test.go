package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// sseLines formats SSE data events.
func sseLines(events ...string) string {
	var sb strings.Builder
	for _, e := range events {
		sb.WriteString("data: " + e + "\n\n")
	}
	return sb.String()
}

// TestResponsesInputWire exercises toResponseInput via a live request,
// covering every conversion path: EasyInput system/user, multimodal user
// parts, verbatim output-items replay, chat-shaped RawAssistant
// decomposition, tool → function_call_output, and orphan stripping.
func TestResponsesInputWire(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("request path = %q, want /v1/responses", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseLines(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`))
	}))
	defer srv.Close()

	msgs := []Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "hi"},
		{Role: "user", ContentParts: []ContentPart{
			{Type: "text", Text: "look"},
			{Type: "image_url", ImageURL: &ImageURL{URL: "https://x/img.png"}},
		}},
		// Responses-shaped RawAssistant: replayed verbatim as items.
		{Role: "assistant", RawAssistant: json.RawMessage(`[{"type":"reasoning","id":"rs_1","encrypted_content":"ENC"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]`)},
		{Role: "tool", ToolCallID: "call_x", Content: "result"},
		// Chat-shaped RawAssistant (pre-apiType-switch session):
		// decomposed into message + function_call items. reasoning_content
		// (DeepSeek thinking placeholder, e.g. loop_auth's " ") becomes a
		// synthetic reasoning item ahead of them.
		{Role: "assistant", RawAssistant: json.RawMessage(`{"role":"assistant","content":"hi there","reasoning_content":" ","tool_calls":[{"id":"call_x","type":"function","function":{"name":"f","arguments":"{}"}}]}`)},
		{Role: "tool", ToolCallID: "call_x", Content: "done"},
		// Orphaned tool_calls: assistant declares call_dead with no
		// following tool message — must be stripped from the wire.
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_dead", Type: "function", Function: FunctionCall{Name: "g", Arguments: "{}"}}}},
		{Role: "user", Content: "again"},
	}
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "get_weather", Description: "w", Parameters: map[string]any{"type": "object"}}}}

	p := NewOpenAIResponses("test-key", srv.URL)
	if _, err := p.Chat(context.Background(), msgs, tools, "test-model", 123, 0.7); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if gotBody["store"] != false {
		t.Errorf("store = %v, want false", gotBody["store"])
	}
	if gotBody["max_output_tokens"] != float64(123) {
		t.Errorf("max_output_tokens = %v, want 123", gotBody["max_output_tokens"])
	}
	if _, ok := gotBody["temperature"]; !ok {
		t.Errorf("missing temperature: %#v", gotBody)
	}
	inc, _ := gotBody["include"].([]any)
	if len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		t.Errorf("include = %#v, want [reasoning.encrypted_content]", gotBody["include"])
	}
	tls, _ := gotBody["tools"].([]any)
	if len(tls) != 1 {
		t.Fatalf("tools = %#v", gotBody["tools"])
	}
	td := tls[0].(map[string]any)
	if td["type"] != "function" || td["name"] != "get_weather" || td["strict"] != false {
		t.Errorf("tool def = %#v, want flattened with strict:false", td)
	}
	if _, nested := td["function"]; nested {
		t.Errorf("tool def nests function key: %#v", td)
	}

	input, _ := gotBody["input"].([]any)
	if len(input) != 12 {
		t.Fatalf("input len = %d, want 12: %#v", len(input), input)
	}
	assertJSONEq(t, "input[0]", input[0], map[string]any{"role": "system", "content": "You are helpful."})
	assertJSONEq(t, "input[1]", input[1], map[string]any{"role": "user", "content": "hi"})
	// Multimodal user: parts renamed text→input_text, image_url→input_image.
	assertJSONEq(t, "input[2]", input[2], map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "input_text", "text": "look"},
		map[string]any{"type": "input_image", "image_url": map[string]any{"url": "https://x/img.png"}},
	}})
	// Verbatim items replay: reasoning (encrypted) + message.
	assertJSONEq(t, "input[3]", input[3], map[string]any{"type": "reasoning", "id": "rs_1", "encrypted_content": "ENC"})
	assertJSONEq(t, "input[4]", input[4], map[string]any{"type": "message", "role": "assistant", "content": []any{
		map[string]any{"type": "output_text", "text": "hello"},
	}})
	assertJSONEq(t, "input[5]", input[5], map[string]any{"type": "function_call_output", "call_id": "call_x", "output": "result"})
	// Chat-shaped decomposition: synthetic reasoning (from
	// reasoning_content), then message item, then function_call item.
	assertJSONEq(t, "input[6]", input[6], map[string]any{"type": "reasoning", "content": []any{
		map[string]any{"type": "reasoning_text", "text": " "},
	}})
	assertJSONEq(t, "input[7]", input[7], map[string]any{"role": "assistant", "content": "hi there"})
	assertJSONEq(t, "input[8]", input[8], map[string]any{"type": "function_call", "id": "call_x", "call_id": "call_x", "name": "f", "arguments": "{}"})
	assertJSONEq(t, "input[9]", input[9], map[string]any{"type": "function_call_output", "call_id": "call_x", "output": "done"})
	// Orphaned tool_calls stripped: the assistant turn survives as an
	// empty message with no function_call item after it.
	assertJSONEq(t, "input[10]", input[10], map[string]any{"role": "assistant", "content": ""})
	for _, raw := range input {
		if m, ok := raw.(map[string]any); ok {
			if _, has := m["call_id"]; has && m["call_id"] == "call_dead" {
				t.Errorf("orphaned call_dead leaked into input: %#v", m)
			}
		}
	}
}

// assertJSONEq compares two JSON values by encoding both sides — map key
// order doesn't matter, and neither side carries bare ints.
func assertJSONEq(t *testing.T, label string, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Errorf("%s =\n  %s\nwant\n  %s", label, g, w)
	}
}

// TestResponsesChatAggregation runs a full tool-call turn through Chat
// and checks every aggregated field: content deltas, reasoning summary,
// tool call assembly across argument deltas, usage (with cache split),
// and the RawAssistant items array.
func TestResponsesChatAggregation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseLines(
			`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
			`{"type":"response.reasoning_summary_text.delta","delta":"pondering"}`,
			`{"type":"response.reasoning_text.delta","delta":" (deepseek-style)"}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"get_weather"}}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"ci"}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"ty\":\"SF\"}"}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"SF\"}","status":"completed"}}`,
			`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[`+
				`{"type":"reasoning","id":"rs_1","encrypted_content":"ENC"},`+
				`{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"SF\"}"}`+
				`],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40},"output_tokens":50,"total_tokens":150}}}`,
		))
	}))
	defer srv.Close()

	p := NewOpenAIResponses("test-key", srv.URL)
	resp, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "weather?"}}, nil, "test-model", 512, 0)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1: %#v", len(resp.ToolCalls), resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_abc" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"SF"}` {
		t.Errorf("tool call = %#v", tc)
	}
	if resp.Usage.InputTokens != 60 || resp.Usage.CacheReadTokens != 40 || resp.Usage.OutputTokens != 50 {
		t.Errorf("usage = %#v, want input=60 cache=40 output=50", resp.Usage)
	}
	if resp.Thinking != "pondering (deepseek-style)" {
		t.Errorf("thinking = %q, want both summary and reasoning_text deltas aggregated", resp.Thinking)
	}
	// RawAssistant must be an items array carrying the encrypted
	// reasoning (replayable next turn) and the function call.
	var items []map[string]any
	if err := json.Unmarshal(resp.RawAssistant, &items); err != nil {
		t.Fatalf("RawAssistant not an items array: %v (%s)", err, resp.RawAssistant)
	}
	if len(items) != 2 || items[0]["encrypted_content"] != "ENC" || items[1]["call_id"] != "call_abc" {
		t.Errorf("RawAssistant items = %#v", items)
	}
}

// TestResponsesChatStreamIncremental verifies ChatStream forwards text
// deltas as they arrive and emits one Done chunk with the assembled turn.
func TestResponsesChatStreamIncremental(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseLines(
			`{"type":"response.output_text.delta","delta":"Hel"}`,
			`{"type":"response.output_text.delta","delta":"lo"}`,
			`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}`,
		))
	}))
	defer srv.Close()

	p := NewOpenAIResponses("test-key", srv.URL)
	reader, err := p.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, "test-model", 100, 0)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var chunks []StreamChunk
	for {
		c, ok := reader.Next()
		if !ok {
			break
		}
		chunks = append(chunks, c)
	}
	if reader.Err() != nil {
		t.Fatalf("stream error: %v", reader.Err())
	}
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3 (2 deltas + done): %#v", len(chunks), chunks)
	}
	if chunks[0].Content != "Hel" || chunks[1].Content != "lo" {
		t.Errorf("deltas = %q %q", chunks[0].Content, chunks[1].Content)
	}
	last := chunks[len(chunks)-1]
	if !last.Done || last.Usage.OutputTokens != 2 {
		t.Errorf("final chunk = %#v", last)
	}
	var items []map[string]any
	if err := json.Unmarshal(last.RawAssistant, &items); err != nil || len(items) != 1 {
		t.Errorf("final RawAssistant = %s (%v)", last.RawAssistant, err)
	}
}

// TestResponsesRawAssistantItemsReplay closes the loop: the RawAssistant
// captured in turn one is replayed verbatim as input items in turn two.
func TestResponsesRawAssistantItemsReplay(t *testing.T) {
	var calls int32
	var secondInput []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := atomic.AddInt32(&calls, 1)
		if call == 2 {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			secondInput, _ = body["input"].([]any)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseLines(
			`{"type":"response.output_text.delta","delta":"hi"}`,
			`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		))
	}))
	defer srv.Close()

	p := NewOpenAIResponses("test-key", srv.URL)
	turn1, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, "test-model", 100, 0)
	if err != nil {
		t.Fatalf("turn1: %v", err)
	}
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", RawAssistant: turn1.RawAssistant},
		{Role: "user", Content: "again"},
	}
	if _, err := p.Chat(context.Background(), msgs, nil, "test-model", 100, 0); err != nil {
		t.Fatalf("turn2: %v", err)
	}
	if len(secondInput) != 3 {
		t.Fatalf("second input len = %d, want 3: %#v", len(secondInput), secondInput)
	}
	var wantItems []any
	_ = json.Unmarshal(turn1.RawAssistant, &wantItems)
	assertJSONEq(t, "replayed items", secondInput[1], wantItems[0])
}

// TestResponsesFailedEvent checks that response.failed surfaces as an
// error rather than a silently empty response.
func TestResponsesFailedEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseLines(`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"boom"}}}`))
	}))
	defer srv.Close()

	p := NewOpenAIResponses("test-key", srv.URL)
	if _, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, "test-model", 100, 0); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want boom", err)
	}
}

// TestResponsesRetriesWithoutTextFormat mirrors the chat provider's
// response_format retry for the Responses parameter names.
func TestResponsesRetriesWithoutTextFormat(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := atomic.AddInt32(&calls, 1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if call == 1 {
			if _, ok := body["text"]; !ok {
				t.Fatalf("first request missing text.format: %#v", body)
			}
			http.Error(w, `{"error":{"message":"Unsupported parameter: 'text.format'."}}`, http.StatusBadRequest)
			return
		}
		if _, ok := body["text"]; ok {
			t.Fatalf("retry still sent text.format: %#v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseLines(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`))
	}))
	defer srv.Close()

	p := NewOpenAIResponses("test-key", srv.URL)
	ctx := WithJSONMode(context.Background())
	if _, err := p.Chat(ctx, []Message{{Role: "user", Content: "hi"}}, nil, "test-model", 100, 0); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}
