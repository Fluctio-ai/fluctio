package agent

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// TestParseTopicsBodyBareArray guards the glm-5.3 drift seen on
// s-1786509815067-63ksrb (2026-08-25): the model dropped the prompt's
// {"topics": ...} wrapper and emitted the bare topics array, failing
// "cannot unmarshal array into Go value of type struct" every idle sweep.
// The first case is the verbatim opening of the logged raw output.
func TestParseTopicsBodyBareArray(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    int // expected topic count
	}{
		{
			name:    "bare array (logged glm-5.3 output, verbatim excerpt)",
			content: `[{"topic":"附件接收功能测试","summary":"用户多次尝试发送附件均失败，只能收到文字消息。后续分别成功接收了txt、zip和pdf格式的附件，确认附件通道恢复正常。","keywords":["附件","测试","txt","zip","pdf","接收成功"],"importance":4,"kind":"episodic","supersedes":[],"segments":[{"s":0,"e":7},{"s":10,"e":11}]},{"topic":"开源Agent与工作流框架调研及源码拆解","summary":"深入调研并对比了 Agent 与工作流编排类开源项目，重点聚焦于“固定链路 DAG + LLM 作为节点”的工作流引擎。","keywords":["AgentFlow","工作流","DAG调度"],"importance":5,"kind":"durable","supersedes":[],"segments":[{"s":34,"e":43}]}]`,
			want:    2,
		},
		{
			name:    "bare array with bare inner quotes (repair path)",
			content: `[{"topic":"吃饭时间","summary":"颠覆了常见的"中午随便、晚上大餐"习惯。","importance":2,"kind":"durable","supersedes":[],"segments":[{"s":1,"e":2}]}]`,
			want:    1,
		},
		{
			name:    "wrapped object (contract shape)",
			content: `{"topics":[{"topic":"甲功复查","summary":"TSH 0.34 仍偏低但明显回升。","importance":4,"kind":"episodic","supersedes":[],"segments":[{"s":210,"e":215}]}]}`,
			want:    1,
		},
		{
			name:    "wrapped object with bare inner quotes (92d1ecf regression)",
			content: `{"topics":[{"topic":"吃饭时间","summary":"颠覆了常见的"中午随便、晚上大餐"习惯。","importance":2,"kind":"durable","supersedes":[],"segments":[{"s":1,"e":2}]}]}`,
			want:    1,
		},
	}
	for _, tc := range cases {
		out, err := parseTopicsBody(tc.content)
		if err != nil {
			t.Errorf("%s: parseTopicsBody: %v", tc.name, err)
			continue
		}
		if len(out.Topics) != tc.want {
			t.Errorf("%s: got %d topics, want %d", tc.name, len(out.Topics), tc.want)
		}
	}
	if out, err := parseTopicsBody("not json at all"); err == nil {
		t.Errorf("garbage input must error, got %d topics", len(out.Topics))
	}
	// The verbatim-excerpt case must carry real field values through.
	out, err := parseTopicsBody(cases[0].content)
	if err != nil || len(out.Topics) < 2 {
		t.Fatalf("reparse case 0: err=%v topics=%d", err, len(out.Topics))
	}
	if out.Topics[0].Kind != "episodic" || !strings.Contains(out.Topics[1].Summary, "工作流引擎") {
		t.Errorf("case 0 fields lost: %+v", out.Topics)
	}
}

// TestParseTopicsBodyShelved covers the settlement section added beside
// topics, including the bare-array fallback (which must still recognize
// the struct despite the extra map field) and a malformed shelved block
// degrading without losing topics.
func TestParseTopicsBodyShelved(t *testing.T) {
	content := `{"topics":[{"topic":"甲功复查","summary":"TSH 回升。","importance":4,"kind":"episodic","segments":[{"s":210,"e":215}]}],
		"shelved":{"new":[{"kind":"deferred","topic":"整理周报模板","context":"聊到周报时用户说回头再弄","reason":"明确说回头但当天没回来","seq":214,"action_prompt":"把周报模板补完"}],"resolved":[3,7]}}`
	out, err := parseTopicsBody(content)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out.Topics) != 1 || len(out.Shelved.New) != 1 || len(out.Shelved.Resolved) != 2 {
		t.Fatalf("unexpected shape: %+v", out)
	}
	if out.Shelved.New[0].Kind != "deferred" || out.Shelved.New[0].Seq != 214 {
		t.Errorf("shelved fields lost: %+v", out.Shelved.New[0])
	}

	// Malformed shelved block must not take topics down with it, and must
	// be flagged so the caller can queue a retry.
	out, err = parseTopicsBody(`{"topics":[{"topic":"甲功复查","summary":"TSH 回升。"}],"shelved":"oops"}`)
	if err != nil || len(out.Topics) != 1 || !out.ShelvedInvalid {
		t.Errorf("malformed shelved: err=%v topics=%d invalid=%v", err, len(out.Topics), out.ShelvedInvalid)
	}
	// A bad field inside an otherwise valid object is also flagged.
	out, err = parseTopicsBody(`{"topics":[{"topic":"甲","summary":"乙"}],"shelved":{"new":"oops"}}`)
	if err != nil || !out.ShelvedInvalid {
		t.Errorf("bad new[]: err=%v invalid=%v", err, out.ShelvedInvalid)
	}
	// A well-formed section is not flagged.
	out, err = parseTopicsBody(`{"topics":[],"shelved":{"new":[],"resolved":[]}}`)
	if err != nil || out.ShelvedInvalid {
		t.Errorf("valid section flagged: err=%v invalid=%v", err, out.ShelvedInvalid)
	}

	// Bare array fallback still applies when shelved is absent.
	out, err = parseTopicsBody(`[{"topic":"甲","summary":"乙"}]`)
	if err != nil || len(out.Topics) != 1 {
		t.Errorf("bare array with shelved field: err=%v topics=%d", err, len(out.Topics))
	}
}

// TestAssembleShelvedExtract pins the settlement validation rules:
// user-line anchor, kind whitelist, per-window cap, and resolved-id
// membership.
func TestAssembleShelvedExtract(t *testing.T) {
	fed := []store.SessionMessage{
		{Seq: 10, Role: "assistant", Content: "要不要我帮你整理周报？"},
		{Seq: 11, Role: "user", Content: "这个先记着，回头我再弄"},
		{Seq: 12, Role: "user", Content: "先这样吧"},
	}
	parsed := shelvedExtract{
		New: []ExtractedShelved{
			{Kind: "deferred", Topic: "整理周报模板", Context: "聊周报时提到", Reason: "说了回头再弄", Seq: 11, ActionPrompt: "补完周报模板"},
			{Kind: "deferred", Topic: "assistant 锚点应被拒", Seq: 10},
			{Kind: "bogus", Topic: "非法 kind", Seq: 12},
			{Kind: "commitment", Topic: "", Seq: 12},
			{Kind: "awaiting_user", Topic: "没回答的问题", Seq: 12},
			{Kind: "deferred", Topic: "重复锚点", Seq: 11},
		},
		Resolved: []int64{3, 7, 99, 3},
	}
	upserts, resolve := assembleShelvedExtract("agt_1", "s-1", fed,
		[]store.ShelvedCandidate{{ID: 3}, {ID: 7}}, parsed, time.Date(2026, 11, 4, 10, 0, 0, 0, time.Local), "m1")

	if len(upserts) != 2 {
		t.Fatalf("want 2 valid new candidates, got %d: %+v", len(upserts), upserts)
	}
	if upserts[0].Topic != "整理周报模板" || upserts[0].Since != "2026-11-04" ||
		upserts[0].Source != "summary" || upserts[0].Extractor != "m1@v1" {
		t.Errorf("candidate fields wrong: %+v", upserts[0])
	}
	if upserts[1].Kind != "awaiting_user" || upserts[1].Seq != 12 {
		t.Errorf("second candidate wrong: %+v", upserts[1])
	}
	if len(resolve) != 2 || resolve[0] != 3 || resolve[1] != 7 {
		t.Errorf("resolve must be open-set only and deduped: %v", resolve)
	}

	// Rune-safe truncation: a byte cut at 200 would split a Chinese char.
	long := strings.Repeat("周报", 120)
	ups, _ := assembleShelvedExtract("agt_1", "s-1", fed, nil, shelvedExtract{
		New: []ExtractedShelved{{Kind: "deferred", Topic: long, Seq: 11}},
	}, time.Now(), "m")
	if len(ups) != 1 || !strings.HasSuffix(ups[0].Topic, "…") || !utf8.ValidString(ups[0].Topic) {
		t.Errorf("truncation must stay on rune boundaries: %q", ups[0].Topic)
	}
}

// TestShelvedPromptCarriesOpenList verifies both extraction prompts
// actually receive the inherited open list (without it the model can
// never resolve anything) and the output contract mentions shelved.
func TestShelvedPromptCarriesOpenList(t *testing.T) {
	full := buildFullPrompt(nil, `[{"id":3,"topic":"整理周报模板","since":"2026-11-01"}]`, "[seq=1 role=user] hi", true)
	if !strings.Contains(full, `"id":3`) || !strings.Contains(full, `"shelved"`) {
		t.Errorf("full prompt missing open list or shelved contract")
	}
	inc := buildIncrementalPrompt(nil, nil, `[{"id":7,"topic":"甲","since":"2026-11-02"}]`, "[seq=2 role=user] hi", true)
	if !strings.Contains(inc, `"id":7`) || !strings.Contains(inc, `"resolved"`) {
		t.Errorf("incremental prompt missing open list or resolved contract")
	}
}

// TestShelvedPromptDisabled verifies opted-out agents get the pre-feature
// prompt: no shelved instructions, no schema, no empty example.
func TestShelvedPromptDisabled(t *testing.T) {
	full := buildFullPrompt(nil, "[]", "[seq=1 role=user] hi", false)
	if strings.Contains(full, "SHELVED LEDGER") || strings.Contains(full, `"shelved"`) {
		t.Errorf("disabled full prompt still mentions shelved")
	}
	if !strings.Contains(full, `{"topics":[]}`) {
		t.Errorf("disabled full prompt lost its empty example")
	}
	inc := buildIncrementalPrompt(nil, nil, "[]", "[seq=2 role=user] hi", false)
	if strings.Contains(inc, "SHELVED LEDGER") || strings.Contains(inc, `"shelved"`) {
		t.Errorf("disabled incremental prompt still mentions shelved")
	}
}
