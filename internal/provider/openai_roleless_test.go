package provider

import (
	"encoding/json"
	"testing"
)

// TestToAPIMessagesStripsRolelessRawAssistant verifies that an Anthropic
// thinking block cached as RawAssistant (no "role" field) is NOT replayed
// verbatim — doing so sends a role-less message that Zhipu GLM rejects with
// code 1214 "Role information cannot be empty". It must fall through to
// normal construction (role=assistant) with a non-empty placeholder content.
func TestToAPIMessagesStripsRolelessRawAssistant(t *testing.T) {
	// Anthropic thinking block: signature + thinking + type, NO role field.
	thinkingRaw := json.RawMessage(`{"signature":"abc","thinking":"let me think","type":"thinking"}`)
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "", RawAssistant: thinkingRaw},
	}
	out := toAPIMessages(msgs)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(out))
	}
	var am struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(out[1], &am); err != nil {
		t.Fatalf("unmarshal assistant output: %v (raw=%s)", err, out[1])
	}
	if am.Role != "assistant" {
		t.Errorf("role lost: got %q (raw=%s)", am.Role, out[1])
	}
	if am.Content == "" {
		t.Errorf("empty assistant content would also be rejected by GLM: raw=%s", out[1])
	}
}

// TestToAPIMessagesReplaysWellFormedRawAssistant verifies a normal
// OpenAI-shaped RawAssistant (with role) is still replayed verbatim so
// prompt-cache prefixes stay byte-identical.
func TestToAPIMessagesReplaysWellFormedRawAssistant(t *testing.T) {
	goodRaw := json.RawMessage(`{"role":"assistant","content":"hello"}`)
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello", RawAssistant: goodRaw},
	}
	out := toAPIMessages(msgs)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(out))
	}
	if string(out[1]) != string(goodRaw) {
		t.Errorf("well-formed raw not replayed verbatim:\n got: %s\nwant: %s", out[1], goodRaw)
	}
}
