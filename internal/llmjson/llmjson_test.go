package llmjson

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
		{
			// The quote after 好 precedes a comma, which a strict parser
			// would read as a close — the heuristic still escapes it
			// because an earlier repair keeps the scanner inside the
			// string, and the final result stays valid JSON.
			name:  "quote before comma inside value still repaired",
			input: `{"a":"他说"好,"然后走了","b":1}`,
			want:  `{"a":"他说\"好,\"然后走了","b":1}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RepairUnescapedQuotes(tt.input); got != tt.want {
				t.Errorf("RepairUnescapedQuotes(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// The repaired output of the logged failure shape must actually unmarshal
// — that's the whole point of the repair path.
func TestRepairUnescapedQuotesRoundTrip(t *testing.T) {
	type topic struct {
		Topic   string   `json:"topic"`
		Summary string   `json:"summary"`
	}
	raw := `{"topics":[{"topic":"AgentFlow开源项目分析","summary":"用户让分析GitHub上的AgentFlow项目。"},{"topic":"吃饭时间","summary":"核心观点：早餐要吃好、晚餐要少吃，颠覆了常见的"中午随便、晚上大餐"习惯。"}]}`
	if json.Valid([]byte(raw)) {
		t.Fatal("test premise: raw must be invalid JSON (bare quotes)")
	}
	var parsed struct {
		Topics []topic `json:"topics"`
	}
	if err := json.Unmarshal([]byte(RepairUnescapedQuotes(raw)), &parsed); err != nil {
		t.Fatalf("repaired JSON still fails to parse: %v", err)
	}
	if len(parsed.Topics) != 2 {
		t.Fatalf("want 2 topics, got %d", len(parsed.Topics))
	}
	if parsed.Topics[1].Summary != `核心观点：早餐要吃好、晚餐要少吃，颠覆了常见的"中午随便、晚上大餐"习惯。` {
		t.Errorf("summary mangled: %q", parsed.Topics[1].Summary)
	}
}
