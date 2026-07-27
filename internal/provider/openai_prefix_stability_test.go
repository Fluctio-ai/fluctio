package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// TestBuildRequestBodyDeterminism 验证 buildRequest 是确定性函数：
// 相同 (messages, tools, model, maxTokens, temperature, stream, mode)
// 两次调用产生字节完全相同的 body。json.Marshal 本身确定，但若
// toAPIMessages 或 chatRequest 组装有 map 遍历/时间戳注入，就会破坏
// 确定性 → prefix cache 永不命中。
func TestBuildRequestBodyDeterminism(t *testing.T) {
	p := &OpenAIProvider{}
	msgs := []Message{
		{Role: "system", Content: "stable system prompt"},
		{Role: "system", Content: "stable runtime context"},
		{Role: "user", Content: "hello"},
	}
	tools := []Tool{
		{Type: "function", Function: ToolFunction{Name: "t1", Description: "d1", Parameters: map[string]any{"type": "object"}}},
	}
	ctx := context.Background()

	r1, err := p.buildRequest(ctx, msgs, tools, "test-model", 4096, 0.5, true, openAIRequestMode{})
	if err != nil {
		t.Fatalf("buildRequest #1: %v", err)
	}
	b1, _ := io.ReadAll(r1.Body)
	r2, err := p.buildRequest(ctx, msgs, tools, "test-model", 4096, 0.5, true, openAIRequestMode{})
	if err != nil {
		t.Fatalf("buildRequest #2: %v", err)
	}
	b2, _ := io.ReadAll(r2.Body)

	if !bytes.Equal(b1, b2) {
		t.Fatalf("buildRequest 非确定性：\n b1=%s\n b2=%s", b1, b2)
	}
	t.Logf("确定性通过: body %d 字节，两次完全一致", len(b1))
}

// TestBodyMessagesPrefixGrowth 验证多轮对话累积历史时，toAPIMessages
// 输出的 messages 段具备字节稳定前缀——即第 N 轮的 messages JSON 是
// 第 N+1 轮的字节前缀。重点覆盖 assistant 的 RawAssistant cache-safe
// replay 路径（toAPIMessages 直用原始字节，不重序列化）。
func TestBodyMessagesPrefixGrowth(t *testing.T) {
	mkRaw := func(content string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"role": "assistant", "content": content})
		return b
	}
	user := func(c string) Message { return Message{Role: "user", Content: c} }
	asst := func(c string) Message {
		return Message{Role: "assistant", Content: c, RawAssistant: mkRaw(c)}
	}

	sysMsgs := []Message{
		{Role: "system", Content: "stable sys"},
		{Role: "system", Content: "stable rt"},
	}

	// 模拟 loop 主入口多轮：每轮 messages = sysMsgs + 累积历史
	scenarios := [][]Message{
		{user("turn1 q")},                                       // 第 1 轮
		{user("turn1 q"), asst("turn1 a"), user("turn2 q")},     // 第 2 轮
		{user("turn1 q"), asst("turn1 a"), user("turn2 q"), asst("turn2 a"), user("turn3 q")}, // 第 3 轮
	}

	var perTurn [][]byte
	for _, hist := range scenarios {
		msgs := append(append([]Message{}, sysMsgs...), hist...)
		apiMsgs := toAPIMessages(msgs)
		raw, _ := json.Marshal(apiMsgs)
		// 去掉结尾 ']'：第 N 轮 [...msgN 去尾应与第 N+1 轮 [...msgN,... 去尾共享前缀
		perTurn = append(perTurn, []byte(strings.TrimSuffix(string(raw), "]")))
	}

	allStable := true
	for i := 0; i+1 < len(perTurn); i++ {
		if !bytes.HasPrefix(perTurn[i+1], perTurn[i]) {
			split := 0
			n := len(perTurn[i])
			if len(perTurn[i+1]) < n {
				n = len(perTurn[i+1])
			}
			for j := 0; j < n; j++ {
				if perTurn[i][j] != perTurn[i+1][j] {
					split = j
					break
				}
			}
			t.Errorf("turn %d→%d messages 段前缀在字节 %d 分叉", i+1, i+2, split)
			allStable = false
		} else {
			t.Logf("turn %d→%d messages 段前缀稳定，共享 %d 字节", i+1, i+2, len(perTurn[i]))
		}
	}
	if allStable {
		t.Logf("=== 结论：provider 序列化层（toAPIMessages + buildRequest）")
		t.Logf("    产生的 body 在多轮下具备字节稳定前缀；RawAssistant replay 工作正常 ===")
	}
}
