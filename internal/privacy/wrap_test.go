package privacy

import (
	"context"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// captureProvider 记录入站消息并返回预置响应,用于验证 WrapProvider
// 的出站 scrub / 入站 restore 双向行为。
type captureProvider struct {
	gotMessages []provider.Message
	resp        *provider.Response
	stream      []provider.StreamChunk
}

func (c *captureProvider) Chat(ctx context.Context, messages []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.Response, error) {
	c.gotMessages = messages
	return c.resp, nil
}

func (c *captureProvider) ChatStream(ctx context.Context, messages []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.StreamReader, error) {
	c.gotMessages = messages
	ch := make(chan provider.StreamChunk, len(c.stream))
	for _, chunk := range c.stream {
		ch <- chunk
	}
	close(ch)
	return provider.NewStreamReader(ch), nil
}

// fakePhone 是符合中国手机号形态的假值(测试内构造,不依赖环境)。
const fakePhone = "13812345678"

// roundTripFixture scrub 一条含真实手机号的消息,返回原文、脱敏文与
// 占位符。Scrub 与 WrapProvider 共用同一份 Options{}(同密钥),占位符
// 一致。
func roundTripFixture(t *testing.T) (raw, scrubbed, ph string) {
	t.Helper()
	raw = "联系 " + fakePhone + " 确认"
	scrubbed = Scrub(raw)
	ph = placeholderRe.FindString(scrubbed)
	if ph == "" || scrubbed == raw {
		t.Fatalf("scrub 未产出占位符: %q", scrubbed)
	}
	return raw, scrubbed, ph
}

// TestWrapProviderChatRoundTrip: WrapProvider 必须双向 —— 出站消息
// scrub(真值→占位符),响应 restore(模型回显的占位符→真值)。后台
// 管线(compaction / 摘要 / MEMORY.md 沉淀 / wiki / insights / 技能
// 学习)经此包装,其输出落库的本地工件必须是真值,否则占位符被永久
// 烤进持久面,模型后续抄进代码再写盘就是坏的。
func TestWrapProviderChatRoundTrip(t *testing.T) {
	raw, scrubbed, ph := roundTripFixture(t)
	inner := &captureProvider{
		resp: &provider.Response{
			Content: "好的,号码是 " + ph,
			ToolCalls: []provider.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: provider.FunctionCall{
					Name:      "write_file",
					Arguments: `{"path":"a.txt","content":"phone=` + ph + `"}`,
				},
			}},
		},
	}
	wrapped := WrapProvider(inner, Options{}, true)

	resp, err := wrapped.Chat(context.Background(),
		[]provider.Message{{Role: "user", Content: raw}}, nil, "test-model", 0, 0)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(inner.gotMessages) != 1 || inner.gotMessages[0].Content != scrubbed {
		t.Fatalf("出站消息未脱敏: got %q, want %q", inner.gotMessages[0].Content, scrubbed)
	}
	if resp.Content != "好的,号码是 "+fakePhone {
		t.Fatalf("响应未恢复真值: got %q", resp.Content)
	}
	wantArgs := `{"path":"a.txt","content":"phone=` + fakePhone + `"}`
	if resp.ToolCalls[0].Function.Arguments != wantArgs {
		t.Fatalf("工具参数未恢复真值: got %s, want %s", resp.ToolCalls[0].Function.Arguments, wantArgs)
	}
}

// TestWrapProviderStreamRoundTrip: 流式响应的 chunk 同样过恢复通道。
func TestWrapProviderStreamRoundTrip(t *testing.T) {
	raw, scrubbed, ph := roundTripFixture(t)
	inner := &captureProvider{
		stream: []provider.StreamChunk{
			{Content: "号码是 " + ph},
			{Done: true},
		},
	}
	wrapped := WrapProvider(inner, Options{}, true)

	sr, err := wrapped.ChatStream(context.Background(),
		[]provider.Message{{Role: "user", Content: raw}}, nil, "test-model", 0, 0)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(inner.gotMessages) != 1 || inner.gotMessages[0].Content != scrubbed {
		t.Fatalf("出站消息未脱敏: got %q, want %q", inner.gotMessages[0].Content, scrubbed)
	}
	var got strings.Builder
	for {
		chunk, ok := sr.Next()
		if !ok {
			break
		}
		got.WriteString(chunk.Content)
	}
	if got.String() != "号码是 "+fakePhone {
		t.Fatalf("流式内容未恢复真值: got %q", got.String())
	}
}
