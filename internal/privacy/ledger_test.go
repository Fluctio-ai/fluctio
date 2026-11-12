package privacy

import (
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// TestRestoreRoundTrip: scrub 出的占位符经 Restore 必须逐字节还原。
// 这是恢复通道的核心契约:模型只见占位符,用户/工具拿回真值。
func TestRestoreRoundTrip(t *testing.T) {
	in := "联系 13900139000 或邮箱 a@b.com,密码是 Hunter2xyz"
	scrubbed := Scrub(in)
	if scrubbed == in {
		t.Fatalf("scrub 未生效: %q", scrubbed)
	}
	if got := Restore(scrubbed); got != in {
		t.Fatalf("Restore 未还原\n  in       = %q\n  scrubbed = %q\n  got      = %q", in, scrubbed, got)
	}
}

// TestRestoreUnknownPlaceholderKept: 未登记的占位符(模型自己编出来的、
// 或台账已满被淘汰的)原样保留,不强行改写。
func TestRestoreUnknownPlaceholderKept(t *testing.T) {
	s := "格式形如 [PHONE_ffffff] 的占位符"
	if got := Restore(s); got != s {
		t.Fatalf("未知占位符被改写: %q", got)
	}
}

// TestRestorePlainTextUntouched: 不含占位符的文本原样返回(快速路径)。
func TestRestorePlainTextUntouched(t *testing.T) {
	s := "普通回复,没有任何标记"
	if got := Restore(s); got != s {
		t.Fatalf("普通文本被改写: %q", got)
	}
}

// TestRestoreResponseCoversContentThinkingToolArgs: 恢复覆盖正文 / 思考 /
// 工具参数三处——工具参数换回真值后模型才能拿真值调工具(查快递场景)。
func TestRestoreResponseCoversContentThinkingToolArgs(t *testing.T) {
	in := "账号 a@b.com"
	scrubbed := Scrub(in)
	ph := placeholderRe.FindString(scrubbed)
	if ph == "" {
		t.Fatalf("scrub 未产出占位符: %q", scrubbed)
	}
	resp := &provider.Response{
		Content:  "好的,账号是 " + ph,
		Thinking: "用户账号 " + ph + " 需要记录",
		ToolCalls: []provider.ToolCall{
			{ID: "c1", Function: provider.FunctionCall{Name: "exec", Arguments: `{"cmd":"echo ` + ph + `"}`}},
		},
	}
	RestoreResponse(resp)
	if !strings.Contains(resp.Content, in[strings.Index(in, "账")+3:]) && !strings.Contains(resp.Content, "@") {
		t.Errorf("正文未恢复: %q", resp.Content)
	}
	if strings.Contains(resp.Content, ph) {
		t.Errorf("正文仍含占位符: %q", resp.Content)
	}
	if strings.Contains(resp.Thinking, ph) {
		t.Errorf("思考仍含占位符: %q", resp.Thinking)
	}
	if strings.Contains(resp.ToolCalls[0].Function.Arguments, ph) {
		t.Errorf("工具参数仍含占位符: %q", resp.ToolCalls[0].Function.Arguments)
	}
}

// TestRestoreDeterministicAcrossRescrub: 确定性指纹保证同一值跨轮
// scrub 得到同一占位符,台账条目幂等,恢复不依赖时序。
func TestRestoreDeterministicAcrossRescrub(t *testing.T) {
	in := "电话 13900139000"
	first := Scrub(in)
	second := Scrub(in)
	if first != second {
		t.Fatalf("同一值两次 scrub 结果不同: %q vs %q", first, second)
	}
	ledger.Lock()
	n := len(ledger.m)
	ledger.Unlock()
	third := Scrub(in)
	_ = third
	ledger.Lock()
	defer ledger.Unlock()
	if len(ledger.m) != n {
		t.Fatalf("重复 scrub 使台账增长: %d -> %d", n, len(ledger.m))
	}
}

// TestRestoreLedgerCap: 台账有界,写满后新条目被跳过且不 panic。
func TestRestoreLedgerCap(t *testing.T) {
	ledger.Lock()
	old := ledger.m
	ledger.m = make(map[string]string)
	ledger.Unlock()
	t.Cleanup(func() {
		ledger.Lock()
		ledger.m = old
		ledger.Unlock()
	})
	for i := 0; i < ledgerCap+10; i++ {
		recordSwap(strings.Repeat("x", 6)+string(rune('a'+i%26))+strings.Repeat("y", i%17), "v")
	}
	ledger.Lock()
	defer ledger.Unlock()
	if len(ledger.m) > ledgerCap {
		t.Fatalf("台账超限: %d", len(ledger.m))
	}
}
