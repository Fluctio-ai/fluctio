package kb

import (
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
	injectKBContext(hc, results, []string{"K1"}, AutoQueryCfg{SearchMode: "augment"})

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
