package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// fakeSummarizer captures the summarize-call prompt so tests can
// assert what compaction actually ships off to the LLM. The
// returned Response is whatever the test wants — we don't care
// about the summary content, only the input.
type fakeSummarizer struct {
	gotSummaryRequest string
}

func (f *fakeSummarizer) Chat(_ context.Context, msgs []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	// compressOlderMessages builds the user-role prompt as the
	// second message; the older-history text lives in its Content
	// after the "Summarize this conversation:\n\n" prefix.
	if len(msgs) >= 2 {
		f.gotSummaryRequest = msgs[1].Content
	}
	return &provider.Response{Content: "[fake summary]"}, nil
}

func (f *fakeSummarizer) ChatStream(_ context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	return nil, nil
}

// TestCompactionDropsGoalContextFromSummary pins design §5.3 (b):
// when compaction folds older messages, runtime-injected
// goal_context messages must be excluded from the summary — their
// content is synthetic audit scaffolding and the latest one is
// already preserved verbatim in the recent tail.
func TestCompactionDropsGoalContextFromSummary(t *testing.T) {
	// Build a history that's longer than PruneTurnAge so
	// compression actually runs. Interleave goal_context messages
	// among real user/assistant turns.
	var msgs []provider.Message
	for i := 0; i < PruneTurnAge+5; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: "real user message", Origin: provider.OriginUser},
			provider.Message{Role: "user", Content: "RUNTIME_AUDIT_PROMPT", Origin: provider.OriginGoalContext},
			provider.Message{Role: "assistant", Content: "real assistant reply", Origin: provider.OriginUser},
		)
	}

	f := &fakeSummarizer{}
	out, err := compressOlderMessages(msgs, f, "fake-model")
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if !strings.Contains(f.gotSummaryRequest, "real user message") {
		t.Errorf("summary input lost real user content: %s", f.gotSummaryRequest)
	}
	if strings.Contains(f.gotSummaryRequest, "RUNTIME_AUDIT_PROMPT") {
		t.Errorf("summary input included runtime-injected goal_context — should have been filtered:\n%s",
			f.gotSummaryRequest)
	}
	// The recent tail must still carry whatever was there; in
	// particular if the tail contained a goal_context the model
	// still needs it for the next audit.
	tailHasContext := false
	for _, m := range out[1:] /* skip the summary prepended at [0] */ {
		if m.Origin == provider.OriginGoalContext {
			tailHasContext = true
			break
		}
	}
	if !tailHasContext {
		t.Error("recent tail should still carry the live goal_context message")
	}
}

// TestCompactionPreservesContentWhenShortCircuits: when the input
// is already under PruneTurnAge, compressOlderMessages returns it
// unchanged. Goal_context filtering shouldn't change that fast path.
func TestCompactionPreservesContentWhenShortCircuits(t *testing.T) {
	in := []provider.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	out, err := compressOlderMessages(in, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("short input should pass through; got %d messages", len(out))
	}
}

// --- safeCompactionCutoff coverage ---
//
// The cutoff guard is the load-bearing fix for the OpenAI 400
// "Messages with role 'tool' must be a response to a preceding
// message with 'tool_calls'" — exhaustively pin its behavior, with
// a final end-to-end assertion that the compressed output never
// starts with a tool message.

func TestSafeCompactionCutoffAdvancesPastLeadingTool(t *testing.T) {
	// History tail looks like [..., assistant(tool_calls), tool, tool, assistant_text, user]
	// with cutoff landing on the first `tool` — must advance to the
	// `assistant_text` position so the resulting tail is valid.
	msgs := []provider.Message{
		{Role: "user", Content: "ask"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "t1"}}},
		{Role: "tool", ToolCallID: "t1", Content: "r1"},
		{Role: "tool", ToolCallID: "t2", Content: "r2"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "next"},
	}
	got := safeCompactionCutoff(msgs, 2) // points at first "tool"
	if msgs[got].Role != "assistant" || msgs[got].Content != "ok" {
		t.Errorf("expected cutoff to land on assistant_text; landed on %+v", msgs[got])
	}
}

func TestSafeCompactionCutoffNoAdvanceOnUser(t *testing.T) {
	msgs := []provider.Message{
		{Role: "assistant", Content: "x"},
		{Role: "user", Content: "y"},
	}
	if got := safeCompactionCutoff(msgs, 1); got != 1 {
		t.Errorf("cutoff = %d, want 1 (user is a valid tail start)", got)
	}
}

func TestSafeCompactionCutoffNoAdvanceOnAssistant(t *testing.T) {
	// An assistant message with tool_calls is a valid tail start —
	// its tool replies follow it inside the preserved tail.
	msgs := []provider.Message{
		{Role: "user", Content: "x"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "t1"}}},
		{Role: "tool", ToolCallID: "t1"},
	}
	if got := safeCompactionCutoff(msgs, 1); got != 1 {
		t.Errorf("cutoff = %d, want 1 (assistant w/ tool_calls is a valid tail start)", got)
	}
}

func TestSafeCompactionCutoffAdvancesToEnd(t *testing.T) {
	// Degenerate: every message from cutoff to end is a tool. The
	// guard advances past all of them — the tail ends up empty and
	// the caller emits just [summary], which is valid.
	msgs := []provider.Message{
		{Role: "user"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "t1"}, {ID: "t2"}}},
		{Role: "tool", ToolCallID: "t1"},
		{Role: "tool", ToolCallID: "t2"},
	}
	if got := safeCompactionCutoff(msgs, 2); got != len(msgs) {
		t.Errorf("cutoff = %d, want %d (entire tail was tool messages)", got, len(msgs))
	}
}

func TestSafeCompactionCutoffNegativeIsClamped(t *testing.T) {
	msgs := []provider.Message{{Role: "user"}}
	if got := safeCompactionCutoff(msgs, -5); got != 0 {
		t.Errorf("cutoff = %d, want 0 (negative input clamped)", got)
	}
}

// TestCompressOlderMessagesNeverStartsTailWithTool is the end-to-end
// assertion that closes the loop. Build a history where the naive
// cutoff lands squarely on a tool reply and verify the compressed
// output's first non-summary message is never a "tool" role. This
// mirrors the shape that was producing the OpenAI 400 in production
// /goal sessions.
func TestCompressOlderMessagesNeverStartsTailWithTool(t *testing.T) {
	// Rounds of [assistant(2 tool_calls), tool, tool] — 3 messages
	// each. 7 rounds = 21 messages. With 5 user fillers in front,
	// total len = 26 and naive cutoff = 26-PruneTurnAge = 6, which
	// indexes a tool reply (assistant at 5, tool at 6, tool at 7).
	var msgs []provider.Message
	for i := 0; i < 5; i++ {
		msgs = append(msgs, provider.Message{Role: "user", Content: "filler"})
	}
	for i := 0; i < 7; i++ {
		msgs = append(msgs,
			provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "ta"}, {ID: "tb"}}},
			provider.Message{Role: "tool", ToolCallID: "ta", Content: "ra"},
			provider.Message{Role: "tool", ToolCallID: "tb", Content: "rb"},
		)
	}
	// Pin the fixture: without the fix, the tail would start with a
	// "tool" message and OpenAI would 400.
	naive := len(msgs) - PruneTurnAge
	if msgs[naive].Role != "tool" {
		t.Fatalf("fixture broken: naive cutoff lands on %q (idx %d, len %d), want tool",
			msgs[naive].Role, naive, len(msgs))
	}

	f := &fakeSummarizer{}
	out, err := compressOlderMessages(msgs, f, "fake-model")
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if len(out) < 2 {
		t.Fatalf("expected summary + tail, got %d messages", len(out))
	}
	if out[1].Role == "tool" {
		t.Errorf("compressed tail still starts with a tool message — the fix didn't take:\n%+v", out[1])
	}
	// Stronger invariant: every "tool" in the output must be preceded
	// somewhere upstream by an assistant.tool_calls. Spot-check by
	// looking for any tool that doesn't follow an assistant directly
	// (or after another tool from the same round).
	for i := 1; i < len(out); i++ {
		if out[i].Role != "tool" {
			continue
		}
		// Walk backwards skipping prior tools in the same round.
		j := i - 1
		for j >= 0 && out[j].Role == "tool" {
			j--
		}
		if j < 0 || out[j].Role != "assistant" || len(out[j].ToolCalls) == 0 {
			t.Errorf("tool at idx %d has no parent assistant.tool_calls in output", i)
		}
	}
}

// TestPruneOldToolResultsTruncatesOversizedInTail pins the fix for the
// "model returned an empty response" every-turn loop: a single multi-MB
// tool result landing in the preserved recent tail used to survive
// compaction untouched, keeping the context above the model limit.
// pruneOldToolResults now caps oversized tool results even in the tail.
func TestPruneOldToolResultsTruncatesOversizedInTail(t *testing.T) {
	huge := strings.Repeat("x", maxRetainedToolResultBytes*4) // well over the cap
	msgs := make([]provider.Message, PruneTurnAge+5)
	for i := range msgs {
		msgs[i] = provider.Message{Role: "user", Content: "u"}
	}
	// Oversized tool result inside the retained tail window.
	msgs[len(msgs)-1] = provider.Message{
		Role: "tool", ToolCallID: "big", Content: huge,
	}
	out := pruneOldToolResults(msgs)
	last := out[len(out)-1]
	if last.Role != "tool" || last.ToolCallID != "big" {
		t.Fatalf("tail tool result identity lost: %+v", last)
	}
	if len(last.Content) >= len(huge) {
		t.Errorf("oversized tail tool result was not truncated: got %d bytes", len(last.Content))
	}
	if !strings.Contains(last.Content, "truncated") {
		t.Errorf("truncated tail should carry a marker; got %q", last.Content)
	}
}

// TestPruneOldToolResultsTruncatesOversizedInShortHistory covers the
// len <= PruneTurnAge path: compressOlderMessages short-circuits there,
// so without truncating the oversized tool result the LLM call ships
// with a context that exceeds the model limit.
func TestPruneOldToolResultsTruncatesOversizedInShortHistory(t *testing.T) {
	huge := strings.Repeat("x", maxRetainedToolResultBytes*4)
	msgs := []provider.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "yo", ToolCalls: []provider.ToolCall{{ID: "t1"}}},
		{Role: "tool", ToolCallID: "t1", Content: huge},
	}
	out := pruneOldToolResults(msgs)
	if out[2].Role != "tool" || out[2].ToolCallID != "t1" {
		t.Fatalf("tool result identity lost: %+v", out[2])
	}
	if len(out[2].Content) >= len(huge) {
		t.Errorf("oversized tool result in short history was not truncated: %d bytes", len(out[2].Content))
	}
	if !strings.Contains(out[2].Content, "truncated") {
		t.Errorf("truncated result should carry a marker; got %q", out[2].Content)
	}
}

// TestPruneOldToolResultsKeepsNormalTailToolResults ensures the cap
// only bites on oversized results — normal tool replies in the tail
// pass through verbatim so the model keeps live tool context.
func TestPruneOldToolResultsKeepsNormalTailToolResults(t *testing.T) {
	normal := strings.Repeat("y", 500) // well under the cap
	msgs := make([]provider.Message, PruneTurnAge+2)
	for i := range msgs {
		msgs[i] = provider.Message{Role: "user", Content: "u"}
	}
	msgs[len(msgs)-1] = provider.Message{
		Role: "tool", ToolCallID: "ok", Content: normal,
	}
	out := pruneOldToolResults(msgs)
	if out[len(out)-1].Content != normal {
		t.Errorf("normal-sized tail tool result should be unchanged")
	}
}

// TestCompactMessagesHandlesOversizedToolResult reproduces the production
// failure end-to-end: a long session whose retained tail carries one
// multi-MB tool result. Before the fix compaction left the oversized
// result untouched, the context stayed above the threshold, and the LLM
// returned an empty response every turn. Now compaction caps it and the
// result lands under the threshold via the prune-only path (no LLM call).
func TestCompactMessagesHandlesOversizedToolResult(t *testing.T) {
	huge := strings.Repeat("z", 2_200_000) // ~2.2 MB, matches the prod row
	msgs := make([]provider.Message, PruneTurnAge+5)
	for i := range msgs {
		msgs[i] = provider.Message{Role: "user", Content: "x"}
	}
	msgs[len(msgs)-1] = provider.Message{
		Role: "tool", ToolCallID: "huge", Content: huge,
	}
	out, err := CompactMessages(msgs, t.TempDir(), &fakeSummarizer{}, "", DefaultTokenThreshold)
	if err != nil {
		t.Fatalf("CompactMessages: %v", err)
	}
	if !out.Pruned {
		t.Fatal("expected compaction to prune the oversized tool result")
	}
	if est := EstimateTokens(out.Messages); est >= DefaultTokenThreshold {
		t.Errorf("compaction left context over threshold: %d tokens", est)
	}
}

// TestCompactMessagesThresholdControlsTrigger verifies that the threshold
// parameter controls whether compaction fires. A very high threshold should
// skip compaction entirely; a very low threshold should force it.
func TestCompactMessagesThresholdControlsTrigger(t *testing.T) {
	msgs := make([]provider.Message, PruneTurnAge+5)
	for i := range msgs {
		msgs[i] = provider.Message{Role: "user", Content: strings.Repeat("x", 1000)}
	}

	// High threshold → compaction should NOT trigger.
	out, err := CompactMessages(msgs, t.TempDir(), &fakeSummarizer{}, "", 10_000_000)
	if err != nil {
		t.Fatalf("CompactMessages high threshold: %v", err)
	}
	if out.Pruned {
		t.Error("high threshold should not trigger compaction")
	}
	if out.TokensBefore == 0 {
		t.Error("TokensBefore should be populated even when no compaction runs")
	}

	// Low threshold → compaction SHOULD trigger.
	out2, err := CompactMessages(msgs, t.TempDir(), &fakeSummarizer{}, "", 1000)
	if err != nil {
		t.Fatalf("CompactMessages low threshold: %v", err)
	}
	if !out2.Pruned {
		t.Error("low threshold should trigger compaction")
	}
}

// TestComputeThreshold exercises the pure threshold calculator at
// every priority branch: manual (plain + clamp), dynamic (three modes),
// fallback (unknown context window), and the 1000-token floor.
func TestComputeThreshold(t *testing.T) {
	// manual 优先
	if got := computeThreshold(50000, 200000, 20000, 8192, "balanced"); got != 50000 {
		t.Errorf("manual should win: %d", got)
	}
	// manual clamp
	if got := computeThreshold(300000, 200000, 20000, 8192, "balanced"); got != 200000-8192 {
		t.Errorf("manual should clamp to cw-maxTokens: %d", got)
	}
	// 动态 balanced: 200000 - 20000 - 8192 - 30000 = 141808
	if got := computeThreshold(0, 200000, 20000, 8192, "balanced"); got != 141808 {
		t.Errorf("balanced dynamic: %d", got)
	}
	// conservative margin 30%: 200000-20000-8192-60000=111808
	if got := computeThreshold(0, 200000, 20000, 8192, "conservative"); got != 111808 {
		t.Errorf("conservative: %d", got)
	}
	// aggressive margin 10%: 200000-20000-8192-20000=151808
	if got := computeThreshold(0, 200000, 20000, 8192, "aggressive"); got != 151808 {
		t.Errorf("aggressive: %d", got)
	}
	// ContextWindow 未知 → 80000
	if got := computeThreshold(0, 0, 20000, 8192, "balanced"); got != DefaultTokenThreshold {
		t.Errorf("unknown cw fallback: %d", got)
	}
	// 下限保护
	if got := computeThreshold(0, 30000, 25000, 8192, "balanced"); got != 1000 {
		t.Errorf("floor 1000: %d", got)
	}
}

// TestModeMarginPct verifies the mode→margin-percent mapping used by
// the dynamic compaction threshold (Phase 2 Task 2).
func TestModeMarginPct(t *testing.T) {
	cases := map[string]int{
		"conservative": 30,
		"balanced":     15,
		"aggressive":   10,
		"":             15, // 默认 balanced
		"unknown":      15,
	}
	for mode, want := range cases {
		if got := modeMarginPct(mode); got != want {
			t.Errorf("modeMarginPct(%q) = %d, want %d", mode, got, want)
		}
	}
}

// TestBuildCompactionNotice verifies the Phase 3 notice builder:
// text must mention compaction + retained turns, meta must carry
// before/after/retained_turns, and large token counts must format
// as 万 (Chinese friendly).
func TestBuildCompactionNotice(t *testing.T) {
	r := &CompactResult{TokensBefore: 123456, TokensAfter: 4111}
	text, meta := buildCompactionNotice(r)
	if !strings.Contains(text, "压缩") || !strings.Contains(text, "20") {
		t.Errorf("notice text missing compaction + retained turns: %q", text)
	}
	if meta["before"] != 123456 || meta["after"] != 4111 {
		t.Errorf("meta stats wrong: %v", meta)
	}
	if !strings.Contains(text, "12.3万") {
		t.Errorf("before should be formatted as 万: %q", text)
	}
	if meta["retained_turns"] != PruneTurnAge {
		t.Errorf("retained_turns want %d got %v", PruneTurnAge, meta["retained_turns"])
	}
}
