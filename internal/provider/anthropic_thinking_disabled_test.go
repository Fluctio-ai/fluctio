package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestAnthropicThinkingDisabledContext pins the WithThinkingDisabled
// contract: the flag reaches the wire as a request-level
// {"type":"disabled"} thinking block, and without the flag no thinking
// field is emitted at all (native Anthropic thinking is opt-off and would
// reject the block — it must only ever ride an explicit retry).
func TestAnthropicThinkingDisabledContext(t *testing.T) {
	p := &AnthropicProvider{apiBase: "https://example.invalid"}
	msgs := []Message{{Role: "user", Content: "hi"}}

	req, err := p.buildRequest(context.Background(), msgs, nil, "m", 1024, 0.2, false)
	if err != nil {
		t.Fatal(err)
	}
	if body := readBody(t, req); strings.Contains(string(body), "thinking") {
		t.Errorf("default ctx must not emit a thinking field: %s", body)
	}

	req, err = p.buildRequest(WithThinkingDisabled(context.Background()), msgs, nil, "m", 8192, 0.2, false)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, req)
	var parsed struct {
		Thinking *struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("parse body: %v (%s)", err, body)
	}
	if parsed.Thinking == nil || parsed.Thinking.Type != "disabled" {
		t.Errorf("thinking block = %+v, want {type: disabled}", parsed.Thinking)
	}
}

// readBody drains the built request's JSON body for assertion.
func readBody(t *testing.T, req *http.Request) []byte {
	t.Helper()
	b, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
