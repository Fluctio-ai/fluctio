package agent

import (
	"encoding/json"
	"testing"
)

// Real-world case (2026-08-24 log): the model ignored the JSON-mode hint
// and emitted bare quotes inside a summary value, so Unmarshal failed with
// "invalid character 'ä' after object key:value pair" (0xE4 = first byte
// of the CJK char following the premature string close).
func TestRepairUnescapedQuotes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "valid JSON unchanged",
			input: `{"topics":[{"topic":"x","summary":"escaped \"quote\"","keywords":["a"],"importance":3}]}`,
			want:  `{"topics":[{"topic":"x","summary":"escaped \"quote\"","keywords":["a"],"importance":3}]}`,
		},
		{
			name:  "bare quotes in value escaped",
			input: `{"summary":"常见的"中午随便、晚上大餐"习惯"}`,
			want:  `{"summary":"常见的\"中午随便、晚上大餐\"习惯"}`,
		},
		{
			name:  "bare quote in array value escaped",
			input: `{"keywords":["吃饭时间","三餐"安排"]}`,
			want:  `{"keywords":["吃饭时间","三餐\"安排"]}`,
		},
		{
			name:  "structural quotes after whitespace still close",
			input: `{"a":"v" , "b":1}`,
			want:  `{"a":"v" , "b":1}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := repairUnescapedQuotes(tt.input); got != tt.want {
				t.Errorf("repairUnescapedQuotes(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// The repaired output of the logged failure shape must actually unmarshal
// — that's the whole point of the repair path.
func TestRepairUnescapedQuotesRoundTrip(t *testing.T) {
	raw := `{"topics":[{"topic":"AgentFlow开源项目分析","summary":"用户让分析GitHub上的AgentFlow项目。","keywords":["AgentFlow","模块化Agent"],"importance":3,"kind":"durable","supersedes":[],"segments":[{"s":23,"e":26}]},{"topic":"吃饭时间","summary":"核心观点：早餐要吃好、晚餐要少吃，颠覆了常见的"中午随便、晚上大餐"习惯。","keywords":["吃饭时间"],"importance":2,"kind":"episodic","supersedes":[],"segments":[{"s":114,"e":117}]}]}`
	if json.Valid([]byte(raw)) {
		t.Fatal("test premise: raw must be invalid JSON (bare quotes)")
	}
	var parsed struct {
		Topics []ExtractedTopic `json:"topics"`
	}
	if err := json.Unmarshal([]byte(repairUnescapedQuotes(raw)), &parsed); err != nil {
		t.Fatalf("repaired JSON still fails to parse: %v", err)
	}
	if len(parsed.Topics) != 2 {
		t.Fatalf("want 2 topics, got %d", len(parsed.Topics))
	}
	if parsed.Topics[1].Summary != `核心观点：早餐要吃好、晚餐要少吃，颠覆了常见的"中午随便、晚上大餐"习惯。` {
		t.Errorf("summary mangled: %q", parsed.Topics[1].Summary)
	}
}
