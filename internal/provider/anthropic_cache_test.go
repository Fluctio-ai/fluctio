package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAnthropicSystemConcatAndCacheBreakpoints guards the two Anthropic
// prompt-cache fixes in toAnthropicMessages:
//  1. Multiple system messages are CONCATENATED into one system block
//     (the old `system = m.Content` overwrite kept only the last one,
//     dropping the whole SOUL/IDENTITY/skills/runtime prompt as soon as
//     any later system message appeared).
//  2. cache_control:{type:"ephemeral"} breakpoints are emitted — one on
//     the system block, one on the message just before the latest user
//     turn — so Anthropic actually caches the prefix (it's explicit
//     opt-in; no marker means 0% cache hit regardless of body stability).
func TestAnthropicSystemConcatAndCacheBreakpoints(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "main system prompt"},
		{Role: "system", Content: "runtime hint"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
		{Role: "user", Content: "follow up"},
	}
	system, out := toAnthropicMessages(msgs)

	// (1) concatenation, not overwrite.
	if len(system) != 1 {
		t.Fatalf("expected 1 concatenated system block, got %d: %+v", len(system), system)
	}
	if !strings.Contains(system[0].Text, "main system prompt") || !strings.Contains(system[0].Text, "runtime hint") {
		t.Errorf("system block dropped a message: %q", system[0].Text)
	}

	// (2a) system block carries an ephemeral breakpoint.
	if system[0].CacheControl == nil || system[0].CacheControl.Type != "ephemeral" {
		t.Errorf("system block missing ephemeral cache_control: %+v", system[0].CacheControl)
	}

	// 3 non-system messages survive (2 user + 1 assistant).
	if len(out) != 3 {
		t.Fatalf("expected 3 out msgs, got %d: %+v", len(out), out)
	}

	// (2b) breakpoint on out[1] (the assistant before the latest user).
	// Its content must be a block array whose last block has cache_control.
	var blocks []map[string]interface{}
	if err := json.Unmarshal(out[1].Content, &blocks); err != nil {
		t.Fatalf("out[1] content is not a block array: %v (content=%s)", err, string(out[1].Content))
	}
	if len(blocks) == 0 {
		t.Fatal("out[1] has no content blocks")
	}
	last := blocks[len(blocks)-1]
	cc, ok := last["cache_control"]
	if !ok {
		t.Errorf("out[1] last block missing cache_control: %+v", last)
	} else if ccMap, ok := cc.(map[string]interface{}); !ok || ccMap["type"] != "ephemeral" {
		t.Errorf("out[1] cache_control is not ephemeral: %+v", cc)
	}

	// The newest user message (out[2]) must NOT carry a breakpoint — it's
	// the uncached tail that changes every turn.
	var userBlocks []map[string]interface{}
	if json.Unmarshal(out[2].Content, &userBlocks) == nil {
		for _, b := range userBlocks {
			if _, has := b["cache_control"]; has {
				t.Errorf("newest user message should not have cache_control: %+v", b)
			}
		}
	}
}

// TestAnthropicCacheBreakpointSkipsEmptyHull verifies the breakpoint walk
// skips a degenerate empty assistant message (content `""`) and lands on
// the nearest earlier non-empty message — both so the breakpoint still
// buys cache and so the empty hull keeps its wire form (Anthropic rejects
// null content but accepts "").
func TestAnthropicCacheBreakpointSkipsEmptyHull(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant"}, // empty content hull
		{Role: "user", Content: "are you there?"},
	}
	_, out := toAnthropicMessages(msgs)

	if len(out) != 3 {
		t.Fatalf("expected 3 out msgs, got %d", len(out))
	}
	// The empty hull must keep its JSON "" form (not be coerced to a block).
	if string(out[1].Content) != `""` {
		t.Fatalf("empty hull content = %s, want \"\"", string(out[1].Content))
	}
	// Breakpoint walked back to out[0] (user "hello") since out[1] was empty.
	var blocks []map[string]interface{}
	if err := json.Unmarshal(out[0].Content, &blocks); err != nil {
		t.Fatalf("out[0] content is not a block array: %v", err)
	}
	if len(blocks) == 0 {
		t.Fatal("out[0] has no content blocks")
	}
	if _, ok := blocks[len(blocks)-1]["cache_control"]; !ok {
		t.Errorf("out[0] last block should carry cache_control after walking back: %+v", blocks[len(blocks)-1])
	}
}
