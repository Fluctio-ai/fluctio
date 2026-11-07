package kb

import (
	"context"
	"fmt"
	"slices"
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

// countingRewriter is a QueryRewriter stub: it counts calls and returns
// its fixed result with need=!noRecall (or the input unchanged with
// need=true when result is empty — the "rewrite unavailable" contract).
type countingRewriter struct {
	result   string
	noRecall bool
	calls    int
}

func (c *countingRewriter) rewrite(ctx context.Context, msgs []provider.Message, q string) (string, bool) {
	c.calls++
	if c.result == "" {
		return q, true
	}
	return c.result, !c.noRecall
}

func rewriteCfg(enabled bool) func() AutoQueryCfg {
	return func() AutoQueryCfg { return AutoQueryCfg{QueryRewrite: enabled} }
}

// smartCfg is rewriteCfg's sibling for the smart-mode tests: memory lane
// in "smart", rewrite on, optional fast-path keywords.
func smartCfg(keywords ...string) func() AutoQueryCfg {
	return func() AutoQueryCfg {
		return AutoQueryCfg{QueryRewrite: true, MemoryAutoMode: "smart", MemoryKeywords: keywords}
	}
}

func TestAutoQueryHookRewritesMemoryQuery(t *testing.T) {
	rw := &countingRewriter{result: "Docker 迁移 Kubernetes 方案 进度"}
	mem := &memLaneRecorder{}
	hook := AutoQueryHook(nil, "a1", rewriteCfg(true), mem.searcher(), rw.rewrite)
	hc := rewriteHookCtx()
	hook(context.Background(), hc)

	if rw.calls != 1 {
		t.Fatalf("rewriter calls = %d, want 1", rw.calls)
	}
	if len(mem.queries) != 1 || mem.queries[0] != rw.result {
		t.Fatalf("memory lane queries = %v, want the rewritten query", mem.queries)
	}
	if len(hc.SyntheticToolCalls) != 1 || !strings.Contains(hc.SyntheticToolCalls[0].Args, rw.result) {
		t.Fatalf("synthetic tool call args = %q, want rewritten query", hc.SyntheticToolCalls[0].Args)
	}
}

func TestAutoQueryHookRewriteFallsBackToRaw(t *testing.T) {
	// The rewriter's failure contract is "return the input unchanged" —
	// the hook must search with the raw message, never skip the lane.
	rw := &countingRewriter{}
	mem := &memLaneRecorder{}
	hook := AutoQueryHook(nil, "a1", rewriteCfg(true), mem.searcher(), rw.rewrite)
	hook(context.Background(), rewriteHookCtx())

	if len(mem.queries) != 1 || mem.queries[0] != "上次聊的那个方案怎么样了" {
		t.Fatalf("memory lane queries = %v, want the raw query", mem.queries)
	}
}

func TestAutoQueryHookNoRewriteWhenDisabled(t *testing.T) {
	rw := &countingRewriter{result: "rewritten"}
	mem := &memLaneRecorder{}
	hook := AutoQueryHook(nil, "a1", rewriteCfg(false), mem.searcher(), rw.rewrite)
	hook(context.Background(), rewriteHookCtx())

	if rw.calls != 0 {
		t.Fatalf("rewriter calls = %d, want 0 when QueryRewrite is off", rw.calls)
	}
	if len(mem.queries) != 1 || mem.queries[0] != "上次聊的那个方案怎么样了" {
		t.Fatalf("memory lane queries = %v, want the raw query", mem.queries)
	}
}

func TestAutoQueryHookNoRewriteWhenAllLanesOff(t *testing.T) {
	// Memory disabled and no KB store → no lane searches → the rewrite
	// call must not be paid at all.
	rw := &countingRewriter{result: "rewritten"}
	hook := AutoQueryHook(nil, "a1", func() AutoQueryCfg {
		return AutoQueryCfg{QueryRewrite: true, MemoryAutoMode: "disabled"}
	}, nil, rw.rewrite)
	hook(context.Background(), rewriteHookCtx())
	if rw.calls != 0 {
		t.Fatalf("rewriter calls = %d, want 0 when no lane runs", rw.calls)
	}
}

func TestAutoQueryHookRewriteCacheShortCircuit(t *testing.T) {
	// Second hook firing on the same message (ReAct loop iteration 2)
	// with the [MEM] injection still present must not re-pay the rewrite.
	rw := &countingRewriter{result: "改写后查询"}
	mem := &memLaneRecorder{}
	hook := AutoQueryHook(nil, "a1", rewriteCfg(true), mem.searcher(), rw.rewrite)
	hc := rewriteHookCtx()
	hook(context.Background(), hc)
	hook(context.Background(), hc)

	if rw.calls != 1 {
		t.Fatalf("rewriter calls = %d, want 1 across two iterations", rw.calls)
	}
	if len(mem.queries) != 1 {
		t.Fatalf("memory lane queries = %v, want a single search", mem.queries)
	}
}

func TestAutoQueryHookRewriteMemoizedOnZeroHits(t *testing.T) {
	// The cost-critical case: no injection means no "[MEM]" marker, so the
	// injection cache above can't short-circuit — the empty-result state
	// must, or an "always mode, nothing relevant" turn re-pays the search
	// (and via the rewrite memo the LLM judge call) on every iteration.
	// One search + one rewrite cover the whole turn; nothing is injected.
	rw := &countingRewriter{result: "改写后查询"}
	var searches int
	empty := func(ctx context.Context, agentID, query string, limit int) ([]MemoryRecallHit, error) {
		searches++
		return nil, nil
	}
	hook := AutoQueryHook(nil, "a1", rewriteCfg(true), empty, rw.rewrite)
	hc := rewriteHookCtx()
	hook(context.Background(), hc)
	hook(context.Background(), hc)
	hook(context.Background(), hc)

	if rw.calls != 1 {
		t.Fatalf("rewriter calls = %d, want 1 across three zero-hit iterations", rw.calls)
	}
	if searches != 1 {
		t.Fatalf("lane searches = %d, want 1 (empty result memoized across iterations)", searches)
	}
	if hc.Messages[0].Content != "上次聊的那个方案怎么样了" {
		t.Fatalf("zero-hit turn must not inject anything, got %q", hc.Messages[0].Content)
	}
}

func TestAutoQueryHookAckShortCircuitsAllLanes(t *testing.T) {
	// Layer 0: a whole-message acknowledgement skips everything — no
	// judge call, no search — even in always mode.
	rw := &countingRewriter{result: "rewritten"}
	mem := &memLaneRecorder{}
	hook := AutoQueryHook(nil, "a1", func() AutoQueryCfg {
		return AutoQueryCfg{QueryRewrite: true, MemoryAutoMode: "always"}
	}, mem.searcher(), rw.rewrite)
	hc := &HookContext{Messages: []provider.Message{{Role: "user", Content: "好的。"}}}
	hook(context.Background(), hc)

	if rw.calls != 0 || len(mem.queries) != 0 {
		t.Fatalf("rewriter calls = %d, lane queries = %v; want 0/none for an ack", rw.calls, mem.queries)
	}
	// The punctuation-stripped whole-match must not swallow real content.
	hc2 := &HookContext{Messages: []provider.Message{{Role: "user", Content: "好的，那个迁移方案再展开讲讲"}}}
	hook(context.Background(), hc2)
	if rw.calls != 1 {
		t.Fatalf("rewriter calls = %d after a non-ack message, want 1", rw.calls)
	}
}

func TestAutoQueryHookSmartNeedFalseSkipsLane(t *testing.T) {
	// "smart" + judge says need=false → the lane never searches.
	rw := &countingRewriter{result: "改写结果", noRecall: true}
	mem := &memLaneRecorder{}
	hook := AutoQueryHook(nil, "a1", smartCfg(), mem.searcher(), rw.rewrite)
	hook(context.Background(), rewriteHookCtx())

	if rw.calls != 1 {
		t.Fatalf("rewriter calls = %d, want 1", rw.calls)
	}
	if len(mem.queries) != 0 {
		t.Fatalf("lane queries = %v, want none on a need=false verdict", mem.queries)
	}
}

func TestAutoQueryHookSmartKeywordHitFiresDespiteNeedFalse(t *testing.T) {
	// A keyword hit fires deterministically — the judge verdict only
	// narrows lanes that deferred to it, never a fired lane.
	rw := &countingRewriter{result: "改写结果", noRecall: true}
	mem := &memLaneRecorder{}
	hook := AutoQueryHook(nil, "a1", smartCfg("方案"), mem.searcher(), rw.rewrite)
	hook(context.Background(), rewriteHookCtx())

	if len(mem.queries) != 1 {
		t.Fatalf("lane queries = %v, want the fired search despite need=false", mem.queries)
	}
	// need=false replies carry no rewrite, so the fired lane searched raw.
	if mem.queries[0] != "上次聊的那个方案怎么样了" {
		t.Fatalf("fired lane query = %q, want the raw message", mem.queries[0])
	}
}

func TestAutoQueryHookSmartVerdictMemoized(t *testing.T) {
	// ReAct iteration 2 of a need=false turn must not re-pay the judge
	// call (no [MEM] marker exists to short-circuit the injection cache).
	rw := &countingRewriter{result: "改写结果", noRecall: true}
	mem := &memLaneRecorder{}
	hook := AutoQueryHook(nil, "a1", smartCfg(), mem.searcher(), rw.rewrite)
	hc := rewriteHookCtx()
	hook(context.Background(), hc)
	hook(context.Background(), hc)

	if rw.calls != 1 {
		t.Fatalf("rewriter calls = %d, want 1 across two iterations", rw.calls)
	}
	if len(mem.queries) != 0 {
		t.Fatalf("lane queries = %v, want none", mem.queries)
	}
}

func TestAutoQueryHookSmartChatOnlyDegradesToKeyword(t *testing.T) {
	// Chat-only turns degrade smart to keyword: no keywords configured →
	// no trigger, and crucially no judge call at all.
	rw := &countingRewriter{result: "rewritten"}
	mem := &memLaneRecorder{}
	hook := AutoQueryHook(nil, "a1", smartCfg(), mem.searcher(), rw.rewrite)
	hc := rewriteHookCtx()
	hc.ChatOnly = true
	hook(context.Background(), hc)

	if rw.calls != 0 || len(mem.queries) != 0 {
		t.Fatalf("rewriter calls = %d, lane queries = %v; want 0/none in a chat-only smart turn", rw.calls, mem.queries)
	}
}

// dedupCfg is the always-mode memory config the dedup tests use.
func dedupCfg() func() AutoQueryCfg {
	return func() AutoQueryCfg { return AutoQueryCfg{MemoryAutoMode: "always"} }
}

// advanceTurn simulates the loop's working set carrying turn 1's result
// into turn 2: append the assistant reply and a fresh user message.
func advanceTurn(hc *HookContext, userMessage string) {
	hc.Messages = append(hc.Messages,
		provider.Message{Role: "assistant", Content: "回复"},
		provider.Message{Role: "user", Content: userMessage})
}

func countPrefixed(msgs []provider.Message, prefix string) int {
	n := 0
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, prefix) {
			n++
		}
	}
	return n
}

// newDedupKBHook wires the store fixture the two KB dedup tests share: one
// wiki page ('milk secrets') that short queries hit through the keyword
// path, searching in augment mode unless searchMode overrides it.
func newDedupKBHook(t *testing.T, searchMode string) func(context.Context, *HookContext) {
	t.Helper()
	db := setupKBVectorTestDB(t)
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	const agent = "a1"
	if _, err := db.ExecContext(ctx, `INSERT INTO wiki_pages (id, agent_id, page_type, slug, title, body, summary, source_ids, tags) VALUES ('source:s1', ?, 'source', 's1', 'milk secrets', 'body about milk', 'milk info', '[]', '[]')`, agent); err != nil {
		t.Fatalf("insert wiki: %v", err)
	}
	store := NewKBStore(db, "sqlite")
	return AutoQueryHook(store, agent, func() AutoQueryCfg {
		return AutoQueryCfg{Enabled: true, AutoMode: "always", MemoryAutoMode: "disabled", SearchMode: searchMode}
	}, nil, nil)
}

func TestAutoQueryHookMemoryLaneSkipsIdenticalRecall(t *testing.T) {
	// The consecutive-turn dedup: a different query that recalls the same
	// hit set with the previous [MEM] block still in scope skips the
	// injection and the synthetic pair — but the search itself still runs
	// (it's the local FTS, not the expensive part).
	mem := &memLaneRecorder{} // returns {ID: 1} for every query
	hook := AutoQueryHook(nil, "a1", dedupCfg(), mem.searcher(), nil)
	hc := rewriteHookCtx()
	hook(context.Background(), hc)
	advanceTurn(hc, "再讲讲 agent 的话题")
	hook(context.Background(), hc)

	if len(mem.queries) != 2 {
		t.Fatalf("lane searches = %d, want 2 (dedup skips the injection, not the search)", len(mem.queries))
	}
	if got := countPrefixed(hc.Messages, "[MEM]"); got != 1 {
		t.Fatalf("[MEM] blocks = %d, want 1 across the two turns", got)
	}
	if got := len(hc.SyntheticToolCalls); got != 1 {
		t.Fatalf("synthetic pairs = %d, want 1 (no duplicate pair for identical recall)", got)
	}
}

func TestAutoQueryHookMemoryLaneReinjectsChangedHits(t *testing.T) {
	// A different recall content injects as before: block replaced (still
	// exactly one), a second synthetic pair appended. The hits must differ
	// in rendered content (topic/summary), not just in row id — the dedup
	// compares the bytes the model sees.
	var calls int
	shifting := func(ctx context.Context, agentID, query string, limit int) ([]MemoryRecallHit, error) {
		calls++
		return []MemoryRecallHit{{ID: int64(calls), Topic: "t", Summary: fmt.Sprintf("第 %d 条总结", calls)}}, nil
	}
	hook := AutoQueryHook(nil, "a1", dedupCfg(), shifting, nil)
	hc := rewriteHookCtx()
	hook(context.Background(), hc)
	advanceTurn(hc, "换个话题")
	hook(context.Background(), hc)

	if got := countPrefixed(hc.Messages, "[MEM]"); got != 1 {
		t.Fatalf("[MEM] blocks = %d, want 1 (replace, not stack)", got)
	}
	if got := len(hc.SyntheticToolCalls); got != 2 {
		t.Fatalf("synthetic pairs = %d, want 2 (changed hits inject again)", got)
	}
}

func TestAutoQueryHookMemoryLaneReinjectsAfterBlockLost(t *testing.T) {
	// The marker guard: compaction removed the [MEM] block from the working
	// set, so an identical hit set must inject again — the dedup only skips
	// while the model can still see the previous block.
	mem := &memLaneRecorder{}
	hook := AutoQueryHook(nil, "a1", dedupCfg(), mem.searcher(), nil)
	hc := rewriteHookCtx()
	hook(context.Background(), hc)

	hc.Messages = slices.DeleteFunc(hc.Messages, func(m provider.Message) bool {
		return strings.HasPrefix(m.Content, "[MEM]")
	})
	advanceTurn(hc, "再讲讲 agent 的话题")
	hook(context.Background(), hc)

	if got := len(hc.SyntheticToolCalls); got != 2 {
		t.Fatalf("synthetic pairs = %d, want 2 (lost block forces a fresh injection)", got)
	}
}

func TestAutoQueryHookKBLaneSkipsIdenticalRecall(t *testing.T) {
	// KB lane, same contract: two queries matching the same single page →
	// the second turn keeps one [KB] block, skips the pair, and still
	// refreshes the citation badges (ordering matched → same [K#] ids).
	hook := newDedupKBHook(t, "")

	hc := &HookContext{Messages: []provider.Message{{Role: "user", Content: "milk secrets"}}}
	hook(context.Background(), hc)
	if len(hc.SyntheticToolCalls) != 1 {
		t.Fatalf("first turn synthetic pairs = %d, want 1", len(hc.SyntheticToolCalls))
	}

	advanceTurn(hc, "milk")
	hook(context.Background(), hc)

	if got := len(hc.SyntheticToolCalls); got != 1 {
		t.Fatalf("synthetic pairs after identical recall = %d, want 1", got)
	}
	if got := countPrefixed(hc.Messages, "[KB]"); got != 1 {
		t.Fatalf("[KB] blocks = %d, want 1", got)
	}
	if len(hc.KnowledgeSources) == 0 {
		t.Fatalf("KnowledgeSources empty on a deduped turn, want refreshed badges")
	}
}

func TestAutoQueryHookKBStrictModeNotDeduped(t *testing.T) {
	// Strict mode never injects a [KB] block — the dedup must stay exempt:
	// an identical hit set still produces the prebuilt answer + SkipLLM.
	hook := newDedupKBHook(t, "strict")

	hc := &HookContext{Messages: []provider.Message{{Role: "user", Content: "milk secrets"}}}
	hook(context.Background(), hc)
	if !hc.SkipLLM {
		t.Fatalf("first strict turn SkipLLM = false, want true")
	}
	hc2 := &HookContext{Messages: []provider.Message{{Role: "user", Content: "milk"}}}
	hook(context.Background(), hc2)
	if !hc2.SkipLLM {
		t.Fatalf("strict mode must stay exempt from the dedup: SkipLLM = false, want true")
	}
	if hc2.PrebuiltContent == "" {
		t.Fatalf("strict turn must carry the prebuilt KB answer")
	}
}
