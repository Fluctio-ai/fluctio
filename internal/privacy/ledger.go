package privacy

import (
	"regexp"
	"strings"
	"sync"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// 恢复通道(round-trip restore): 出站 scrub 把 PII 换成 [LABEL_xxxxxx]
// 假名占位符,模型回显占位符后,在落库 / 发给用户 / 交给工具执行前换回
// 真值。指纹占位符本身就是确定性映射 key,台账只存内存(与 magpie 的
// values map 同理),不落盘。借鉴 yetone/magpie 的 Mask(出站可逆)/
// Scrub(落库单向)分流:会话历史本来就存原文(scrub 只在发送时发生),
// 恢复只是把 assistant 消息拉回同一语义。
var ledger = struct {
	sync.Mutex
	m map[string]string
}{m: make(map[string]string)}

// ledgerCap 是占位符→原值映射的上限。条目是确定性幂等的:被淘汰的
// 占位符下轮对历史重放 scrub 时会重新录入。
const ledgerCap = 10000

// placeholderRe 从文本里捞出 [LABEL_xxxxxx] 形态的占位符。
var placeholderRe = regexp.MustCompile(`\[[A-Z]+_[0-9a-f]{6}\]`)

// recordSwap 记录占位符→原值。tagged() 打占位符时调用。
func recordSwap(placeholder, value string) {
	if value == "" {
		return
	}
	ledger.Lock()
	defer ledger.Unlock()
	if _, ok := ledger.m[placeholder]; ok {
		return
	}
	if len(ledger.m) >= ledgerCap {
		return // 满则跳过;重 scrub 时若仍在历史中会重新录入
	}
	ledger.m[placeholder] = value
}

// Restore 把 s 中已登记的占位符换回原值;未知占位符原样保留
// (模型讨论了占位符格式本身、或台账已满时,不强行改写)。
func Restore(s string) string {
	if !strings.Contains(s, "[") {
		return s
	}
	matches := placeholderRe.FindAllString(s, -1)
	if len(matches) == 0 {
		return s
	}
	ledger.Lock()
	defer ledger.Unlock()
	for _, ph := range matches {
		if v, ok := ledger.m[ph]; ok {
			s = strings.ReplaceAll(s, ph, v)
		}
	}
	return s
}

// RestoreResponse 把模型响应里回显的占位符换回真值:正文、思考与
// tool call 参数(参数换回后工具才能拿真值干活;下轮出站 scrub 会对
// 历史重新脱敏,闭环一致)。RawAssistant 不动:其中的占位符下轮会
// 被重新 mask,保持跨轮稳定。
func RestoreResponse(resp *provider.Response) {
	if resp == nil {
		return
	}
	resp.Content = Restore(resp.Content)
	if resp.Thinking != "" {
		resp.Thinking = Restore(resp.Thinking)
	}
	for i := range resp.ToolCalls {
		resp.ToolCalls[i].Function.Arguments = Restore(resp.ToolCalls[i].Function.Arguments)
	}
}

// RestoreStreamReader 包装流式响应:每个 chunk 的内容与 tool call
// 参数过恢复通道后再转发。错误在源流结束后、输出通道关闭前挂到
// 包装流上(消费者只在通道关闭后读 Err,时序安全)。用于流式直发
// 路径(streamFinalDeliveryAfterCap)。
func RestoreStreamReader(sr *provider.StreamReader) *provider.StreamReader {
	if sr == nil {
		return nil
	}
	out := make(chan provider.StreamChunk)
	wrapped := provider.NewStreamReader(out)
	go func() {
		defer close(out)
		for {
			chunk, ok := sr.Next()
			if !ok {
				break
			}
			if chunk.Content != "" {
				chunk.Content = Restore(chunk.Content)
			}
			for i := range chunk.ToolCalls {
				chunk.ToolCalls[i].Function.Arguments = Restore(chunk.ToolCalls[i].Function.Arguments)
			}
			out <- chunk
		}
		if err := sr.Err(); err != nil {
			wrapped.SetErr(err)
		}
	}()
	return wrapped
}
