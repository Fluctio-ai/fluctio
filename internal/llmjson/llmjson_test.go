package llmjson

import (
	"encoding/json"
	"strings"
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

// TestUnmarshalLLMCoversAllTolerancePaths walks UnmarshalLLM through every
// failure mode it claims to tolerate, using the shapes actually observed in
// production logs (bare array, fence, bare quotes, and combinations).
func TestUnmarshalLLMCoversAllTolerancePaths(t *testing.T) {
	type card struct {
		Front string `json:"front"`
		Back  string `json:"back"`
	}
	newTarget := func() any {
		return &struct {
			Cards []card `json:"cards"`
		}{}
	}
	strict := `[{"front":"Q1","back":"A1"},{"front":"Q2","back":"A2"}]`
	cases := []struct {
		name string
		raw  string
	}{
		{"valid wrapped object", `{"cards":[{"front":"Q","back":"A"}]}`},
		{"valid bare array", strict},
		{"fenced object", "```json\n{\"cards\":[{\"front\":\"Q\",\"back\":\"A\"}]}\n```"},
		{"fenced bare array", "```json\n" + strict + "\n```"},
		{"bare array with bare quotes", `[{"front":"Q","back":"说"漏了"两个字"}]`},
		{"wrapped with bare quotes", `{"cards":[{"front":"Q","back":"说"漏了"两个字"}]}`},
	}
	for _, tc := range cases {
		v := newTarget()
		if err := UnmarshalLLM(tc.raw, v); err != nil {
			t.Errorf("%s: UnmarshalLLM: %v", tc.name, err)
			continue
		}
		pv := v.(*struct {
			Cards []card `json:"cards"`
		})
		if len(pv.Cards) == 0 {
			t.Errorf("%s: parsed but zero cards", tc.name)
		}
	}

	// Structs with no (or several) slice fields must not take the bare-array
	// path — they fall through to the object path and fail on an array.
	if err := UnmarshalLLM(strict, &struct{ Name string `json:"name"` }{}); err == nil {
		t.Error("non-slice struct must reject a bare array")
	}
	if err := UnmarshalLLM(`not json`, newTarget()); err == nil {
		t.Error("garbage must error")
	}
	// UnmarshalLLM surfaces the strict-parse error for logging context.
	if err := UnmarshalLLM(`not json`, newTarget()); err == nil || !strings.Contains(err.Error(), "invalid character") {
		t.Errorf("want the original strict error, got %v", err)
	}
}
