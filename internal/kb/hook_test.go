package kb

import (
	"context"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

func TestNumberKBResults(t *testing.T) {
	results := []KBResult{
		{SourceID: "s1", SourceTitle: "产品手册", ChunkIndex: 0},
		{SourceID: "s2", SourceTitle: "FAQ", ChunkIndex: 2},
	}
	citations, sources := numberKBResults(results)
	if len(citations) != 2 || citations[0] != "K1" || citations[1] != "K2" {
		t.Fatalf("citations = %v, want [K1 K2]", citations)
	}
	if len(sources) != 2 {
		t.Fatalf("sources len = %d, want 2", len(sources))
	}
	if sources[0].ID != "K1" || sources[0].File != "产品手册" || sources[0].Chunk != 0 {
		t.Fatalf("sources[0] = %+v", sources[0])
	}
	if sources[1].ID != "K2" || sources[1].File != "FAQ" || sources[1].Chunk != 2 {
		t.Fatalf("sources[1] = %+v", sources[1])
	}
}

func TestBuildToolResultSummaryCitesIDs(t *testing.T) {
	results := []KBResult{
		{SourceTitle: "产品手册", Content: "内容A"},
		{SourceTitle: "FAQ", Content: "答案B"},
	}
	got := buildToolResultSummary(results, []string{"K1", "K2"})
	if !strings.Contains(got, "[K1] 产品手册") {
		t.Fatalf("summary missing [K1] marker: %q", got)
	}
	if !strings.Contains(got, "[K2] FAQ") {
		t.Fatalf("summary missing [K2] marker: %q", got)
	}
}

func TestInjectKBContextMarksCitationsAndInstruction(t *testing.T) {
	hc := &HookContext{Messages: []provider.Message{{Role: "system", Content: "sys"}}}
	results := []KBResult{
		{SourceTitle: "产品手册", ChunkIndex: 0, Content: "全文内容"},
	}
	injectKBContext(hc, results, []string{"K1"})

	// Expect [system, kb-block]: the [KB] user message is inserted after system.
	if len(hc.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2 (system + kb block)", len(hc.Messages))
	}
	kbBlock := hc.Messages[1].Content
	if !strings.Contains(kbBlock, "[K1] Source: 产品手册") {
		t.Fatalf("kb block missing [K1] source marker: %q", kbBlock)
	}
	if !strings.Contains(kbBlock, "cite it inline with the bracketed id") {
		t.Fatalf("kb block missing citation instruction: %q", kbBlock)
	}
}

func TestClipUTF8DoesNotSplitRune(t *testing.T) {
	// 中文每字 3 字节；在 4 字节处截断会劈开第二个字 → 必须回退到 3 字节边界。
	s := "中文测试内容"
	got := clipUTF8(s, 4)
	if got != "中" {
		t.Fatalf("clipUTF8(s, 4) = %q (%d bytes), want \"中\" (back up to rune boundary)", got, len(got))
	}
	// 低于上限原样返回。
	if clipUTF8(s, 100) != s {
		t.Fatalf("clipUTF8 under the limit must return the full string")
	}
}

func TestSoftClipUTF8BreaksAtPunctuation(t *testing.T) {
	// 30 字节窗口内有逗号；softClip 应在最近的逗号/句号处断，而不是切在字中间。
	s := "这是一段中文内容，然后继续到这里。后面还有"
	got := softClipUTF8(s, 30)
	last := []rune(got)
	if len(last) == 0 {
		t.Fatalf("softClipUTF8 returned empty")
	}
	r := last[len(last)-1]
	if r != '，' && r != '。' && r != ',' && r != '.' {
		t.Fatalf("softClipUTF8 should end at punctuation, got last rune %q in %q", r, got)
	}
	// 无标点时退回字符边界（不劈开汉字）。
	got2 := softClipUTF8("无标点长文本内容到这里", 8)
	if got2 != "无标" { // 8 字节 → 退到 6 字节边界（2 字）
		t.Fatalf("softClipUTF8 no-punct fallback = %q, want \"无标\"", got2)
	}
}

// memLaneRecorder wires a memory lane that records every query it receives.
type memLaneRecorder struct {
	queries []string
}

func (m *memLaneRecorder) searcher() MemoryAutoSearcher {
	return func(ctx context.Context, agentID, query string, limit int) ([]MemoryRecallHit, error) {
		m.queries = append(m.queries, query)
		return []MemoryRecallHit{{ID: 1, Topic: "t", Summary: "s"}}, nil
	}
}

func rewriteHookCtx() *HookContext {
	return &HookContext{
		Messages: []provider.Message{
			{Role: "user", Content: "上次聊的那个方案怎么样了"},
		},
	}
}

func TestAutoQueryHookRewritesMemoryQuery(t *testing.T) {
	var rewrites int
	rewriter := func(ctx context.Context, msgs []provider.Message, q string) string {
		rewrites++
		return "Docker 迁移 Kubernetes 方案 进度"
	}
	mem := &memLaneRecorder{}
	hook := AutoQueryHook(nil, "a1", func() AutoQueryCfg { return AutoQueryCfg{QueryRewrite: true} }, mem.searcher(), rewriter)
	hc := rewriteHookCtx()
	hook(context.Background(), hc)

	if rewrites != 1 {
		t.Fatalf("rewriter calls = %d, want 1", rewrites)
	}
	if len(mem.queries) != 1 || mem.queries[0] != "Docker 迁移 Kubernetes 方案 进度" {
		t.Fatalf("memory lane queries = %v, want the rewritten query", mem.queries)
	}
	if len(hc.SyntheticToolCalls) != 1 || !strings.Contains(hc.SyntheticToolCalls[0].Args, "Docker 迁移 Kubernetes 方案 进度") {
		t.Fatalf("synthetic tool call args = %q, want rewritten query", hc.SyntheticToolCalls[0].Args)
	}
}

func TestAutoQueryHookRewriteFallsBackToRaw(t *testing.T) {
	// The rewriter's failure contract is "return the input unchanged" —
	// the hook must search with the raw message, never skip the lane.
	mem := &memLaneRecorder{}
	hook := AutoQueryHook(nil, "a1", func() AutoQueryCfg { return AutoQueryCfg{QueryRewrite: true} }, mem.searcher(),
		func(ctx context.Context, msgs []provider.Message, q string) string { return q })
	hook(context.Background(), rewriteHookCtx())

	if len(mem.queries) != 1 || mem.queries[0] != "上次聊的那个方案怎么样了" {
		t.Fatalf("memory lane queries = %v, want the raw query", mem.queries)
	}
}

func TestAutoQueryHookNoRewriteWhenDisabled(t *testing.T) {
	var rewrites int
	mem := &memLaneRecorder{}
	hook := AutoQueryHook(nil, "a1", func() AutoQueryCfg { return AutoQueryCfg{QueryRewrite: false} }, mem.searcher(),
		func(ctx context.Context, msgs []provider.Message, q string) string {
			rewrites++
			return "rewritten"
		})
	hook(context.Background(), rewriteHookCtx())

	if rewrites != 0 {
		t.Fatalf("rewriter calls = %d, want 0 when QueryRewrite is off", rewrites)
	}
	if len(mem.queries) != 1 || mem.queries[0] != "上次聊的那个方案怎么样了" {
		t.Fatalf("memory lane queries = %v, want the raw query", mem.queries)
	}
}

func TestAutoQueryHookNoRewriteWhenAllLanesOff(t *testing.T) {
	// Memory disabled and no KB store → no lane searches → the rewrite
	// call must not be paid at all.
	var rewrites int
	hook := AutoQueryHook(nil, "a1", func() AutoQueryCfg { return AutoQueryCfg{QueryRewrite: true, MemoryAutoMode: "disabled"} }, nil,
		func(ctx context.Context, msgs []provider.Message, q string) string {
			rewrites++
			return "rewritten"
		})
	hook(context.Background(), rewriteHookCtx())
	if rewrites != 0 {
		t.Fatalf("rewriter calls = %d, want 0 when no lane runs", rewrites)
	}
}

func TestAutoQueryHookRewriteCacheShortCircuit(t *testing.T) {
	// Second hook firing on the same message (ReAct loop iteration 2)
	// with the [MEM] injection still present must not re-pay the rewrite.
	var rewrites int
	mem := &memLaneRecorder{}
	hook := AutoQueryHook(nil, "a1", func() AutoQueryCfg { return AutoQueryCfg{QueryRewrite: true} }, mem.searcher(),
		func(ctx context.Context, msgs []provider.Message, q string) string {
			rewrites++
			return "改写后查询"
		})
	hc := rewriteHookCtx()
	hook(context.Background(), hc)
	hook(context.Background(), hc)

	if rewrites != 1 {
		t.Fatalf("rewriter calls = %d, want 1 across two iterations", rewrites)
	}
	if len(mem.queries) != 1 {
		t.Fatalf("memory lane queries = %v, want a single search", mem.queries)
	}
}
