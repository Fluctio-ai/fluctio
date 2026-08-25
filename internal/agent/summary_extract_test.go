package agent

import (
	"strings"
	"testing"
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
			name: "bare array (logged glm-5.3 output, verbatim excerpt)",
			content: `[{"topic":"附件接收功能测试","summary":"用户多次尝试发送附件均失败，只能收到文字消息。后续分别成功接收了txt、zip和pdf格式的附件，确认附件通道恢复正常。","keywords":["附件","测试","txt","zip","pdf","接收成功"],"importance":4,"kind":"episodic","supersedes":[],"segments":[{"s":0,"e":7},{"s":10,"e":11}]},{"topic":"开源Agent与工作流框架调研及源码拆解","summary":"深入调研并对比了 Agent 与工作流编排类开源项目，重点聚焦于“固定链路 DAG + LLM 作为节点”的工作流引擎。","keywords":["AgentFlow","工作流","DAG调度"],"importance":5,"kind":"durable","supersedes":[],"segments":[{"s":34,"e":43}]}]`,
			want: 2,
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
		topics, err := parseTopicsBody(tc.content)
		if err != nil {
			t.Errorf("%s: parseTopicsBody: %v", tc.name, err)
			continue
		}
		if len(topics) != tc.want {
			t.Errorf("%s: got %d topics, want %d", tc.name, len(topics), tc.want)
		}
	}
	if topics, err := parseTopicsBody("not json at all"); err == nil {
		t.Errorf("garbage input must error, got %d topics", len(topics))
	}
	// The verbatim-excerpt case must carry real field values through.
	topics, err := parseTopicsBody(cases[0].content)
	if err != nil || len(topics) < 2 {
		t.Fatalf("reparse case 0: err=%v topics=%d", err, len(topics))
	}
	if topics[0].Kind != "episodic" || !strings.Contains(topics[1].Summary, "工作流引擎") {
		t.Errorf("case 0 fields lost: %+v", topics)
	}
}
