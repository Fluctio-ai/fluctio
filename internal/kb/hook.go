package kb

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// HookContext is a subset of the agent's HookContext that KB needs.
type HookContext struct {
	Messages           []provider.Message
	Source             string
	// ChatOnly mirrors the session's pure-conversation flag: when true,
	// "always" recall lanes degrade to keyword-triggered (disabled lanes
	// stay disabled) so brainstorming turns stay quiet unless the user
	// explicitly names something recallable.
	ChatOnly           bool
	SkipLLM            bool
	PrebuiltContent    string
	SyntheticToolCalls []SyntheticToolCall
	// KnowledgeSources carries the [K#]-numbered sources for this turn's
	// retrieval so the agent loop can attach them to the assistant message
	// and the web UI can render citations as clickable badges.
	KnowledgeSources []KnowledgeSource
}

// SyntheticToolCall mirrors agent.SyntheticToolCall without importing agent.
type SyntheticToolCall struct {
	Name   string
	Args   string
	Result string
}

// AutoQueryCfg is the config the auto-query hook reads. Mirrors the
// fields from config.KBCfg to avoid importing the config package.
type AutoQueryCfg struct {
	// Wiki auto-recall group.
	Enabled    bool
	AutoMode   string
	Keywords   []string
	MaxResults int
	Threshold  float64
	// Flash/todo auto-recall group (independent trigger/limit/threshold).
	FlashTodoEnabled    bool
	FlashTodoAutoMode   string
	FlashTodoKeywords   []string
	FlashTodoMaxResults int
	FlashTodoThreshold  float64
	// Memory auto-recall group — conversation summaries, lexical FTS only.
	// Empty MemoryAutoMode = "always" (default ON); "disabled" opts out.
	MemoryAutoMode   string
	MemoryKeywords   []string
	MemoryMaxResults int
	// QueryRewrite runs the shared query-rewrite step before any lane
	// searches (see QueryRewriter). Mirrors AgentKBCfg.QueryRewrite with
	// the nil-default resolved to true by the wiring layer.
	QueryRewrite bool
	// Shared.
	SearchMode  string // "augment" (default), "strict"
	EmptyAction string // "llm" (default), "stop"
}

// MemoryRecallHit is one conversation-summary memory surfaced by the
// auto-recall memory lane. kb must not import the store package (agent
// wires this from DBStore), so the hit is a local projection.
type MemoryRecallHit struct {
	ID      int64
	Topic   string
	Summary string
}

// MemoryAutoSearcher recalls conversation summaries for the memory lane.
// The agent manager wires this over the store's FTS path (precise lexical
// match; superseded rows filtered inside the store).
type MemoryAutoSearcher func(ctx context.Context, agentID, query string, limit int) ([]MemoryRecallHit, error)

// QueryRewriter judges and rewrites the raw last-user-message before any
// auto-recall lane searches: one cheap LLM call returns whether the
// message is retrieval-worthy at all (need=false — the "smart" lanes'
// skip signal) and, when it is, the standalone query with pronouns
// resolved against recent context and filler dropped. Wired by the agent
// manager over a background LLM call (summary model, no thinking, JSON
// mode). The contract is total: on ANY failure (error, timeout, unusable
// output) the implementation returns the input query with need=true —
// the rewrite only improves quality, and the verdict only ever narrows
// lanes that opted into it, never blocks recall outright. On need=false
// the searchQuery return is UNSPECIFIED (a compliant producer returns
// the raw message, but consumers must not rely on it) — the consumer
// normalizes to the raw message itself, so lanes that still search
// (always/keyword hits, which ignore the verdict) never query with a
// meaningless rewrite.
type QueryRewriter func(ctx context.Context, messages []provider.Message, query string) (searchQuery string, need bool)

// AutoQueryHook returns a function suitable for use as a BeforeModelCall
// hook. The cfgFn callback reads the agent's current KB config on each
// call so changes take effect without restart. rewriter may be nil (no
// rewrite; the raw message searches directly) — the wiring layer passes
// nil when the agent has no provider.
//
// The returned closure holds a per-agent cache of the last query it ran
// for. Within a ReAct loop the same user message drives every iteration,
// so without this cache the hook would re-search the FTS index, re-emit
// a synthetic knowledgebase_search tool_call/result pair, and re-inject
// a [KB] context message on every model call — once per loop iteration,
// even though the previous iteration's results are already in the
// session. The cache short-circuits repeat iterations so the work runs
// exactly once per distinct user query.
//
// Staleness tradeoff: the cache is keyed by query string only. If the
// agent calls a knowledgebase_ingest_* tool mid-loop with the same user
// query, auto-query will NOT pick up the new content until the user
// sends a different message. The LLM still sees the ingest result in
// its tool-result stream and can call knowledgebase_search explicitly
// to refresh — auto-query is a convenience layer, not the only path.
func AutoQueryHook(store *KBStore, agentID string, cfgFn func() AutoQueryCfg, memSearch MemoryAutoSearcher, rewriter QueryRewriter) func(context.Context, *HookContext) {
	var lastQuery string
	var lastMemQuery string
	// Rewrite memo: keyed by the raw query, independent of whether any lane
	// produced an injection (see the call site). The need verdict memoizes
	// with it, so ReAct iterations of a need=false turn never re-pay the
	// judge call.
	var lastRewriteQuery, lastRewriteResult string
	var lastRewriteNeed bool
	return func(ctx context.Context, hc *HookContext) {
		cfg := cfgFn()
		slog.Debug("kb auto-query hook", "agent", agentID, "enabled", cfg.Enabled, "mode", cfg.AutoMode, "store_nil", store == nil, "source", hc.Source)
		if hc.Source != "" {
			return
		}

		query := extractLastUserMessage(hc.Messages)
		if query == "" {
			return
		}

		// Layer 0, before any lane gate: whole-message acknowledgements
		// ("好的" / "嗯嗯" / "ok") short-circuit every mode, always
		// included — nothing in them is retrieval-worthy, so no mode (not
		// even "always") should pay the judge call, let alone a search.
		// Whole-match only, so "好的，那 X 再整理一下" falls through.
		if isPureAckMessage(query) {
			return
		}

		// Chat-only turns degrade proactive recall: "always" AND "smart"
		// lanes become keyword-triggered so a quiet brainstorming turn
		// injects nothing (and pays no judge call), while an explicit
		// keyword still recalls. "disabled" stays off, and the wiki/flash
		// "" (off) modes are untouched — degrading "" would silently turn
		// lanes on.
		if hc.ChatOnly {
			cfg.AutoMode = degradeAutoModeForChatOnly(cfg.AutoMode)
			cfg.FlashTodoAutoMode = degradeAutoModeForChatOnly(cfg.FlashTodoAutoMode)
		}

		// Each group triggers independently, in three states. gateFire:
		// search now (always mode, or keyword/smart with a keyword hit —
		// hits bypass the judge). gateNeedsLLM: "smart" with no keyword
		// hit — the shared judge call below decides. gateSkip: off or not
		// triggered (laneGateFor's default branch covers "disabled" and
		// every unknown mode). KB lanes need the KBStore; the memory lane
		// only needs its own searcher.
		wikiGate := gateSkip
		if store != nil && cfg.Enabled {
			wikiGate = laneGateFor(cfg.AutoMode, query, cfg.Keywords)
		}
		ftGate := gateSkip
		if store != nil && cfg.FlashTodoEnabled {
			ftGate = laneGateFor(cfg.FlashTodoAutoMode, query, cfg.FlashTodoKeywords)
		}
		// Memory lane defaults ON: empty mode = "always". The trigger-gap
		// fix only works if recall fires without the LLM choosing to ask.
		memMode := cfg.MemoryAutoMode
		if memMode == "" {
			memMode = "always"
		}
		if hc.ChatOnly {
			memMode = degradeAutoModeForChatOnly(memMode)
		}
		memGate := gateSkip
		if memSearch != nil {
			memGate = laneGateFor(memMode, query, cfg.MemoryKeywords)
		}
		if wikiGate == gateSkip && ftGate == gateSkip && memGate == gateSkip {
			return
		}

		// Cache hit: same query already processed AND the injection it
		// produced is still in hc.Messages. Per-lane caches: a KB lane
		// checks its [KB] injection, the memory lane its [MEM] one.
		kbDone := wikiGate == gateSkip && ftGate == gateSkip || lastQuery == query && messagesContainPrefixed(hc.Messages, "[KB]")
		memDone := memGate == gateSkip || lastMemQuery == query && messagesContainPrefixed(hc.Messages, "[MEM]")
		if kbDone && memDone {
			return
		}

		// Query rewrite: one shared judge+rewrite call serves every lane
		// that is about to search, and only when at least one lane will
		// actually search (the lane gates above run on the RAW message —
		// trigger semantics belong to the user's own words, not to a
		// rewritten paraphrase). It answers need (does this message
		// warrant a search at all?) and the rewritten query in the same
		// call. Memoized by raw query on its own key rather than riding
		// the injection caches above: those require a "[KB]"/"[MEM]"
		// marker, which a zero-hit lane never produces — so without this
		// memo the common "always mode, nothing relevant" turn would
		// re-pay the LLM call on every ReAct iteration. Best-effort by
		// contract: any failure returns (raw, need=true).
		searchQuery, need := query, true
		if rewriter != nil && cfg.QueryRewrite {
			if lastRewriteQuery == query {
				searchQuery, need = lastRewriteResult, lastRewriteNeed
			} else {
				searchQuery, need = rewriter(ctx, hc.Messages, query)
				lastRewriteQuery, lastRewriteResult, lastRewriteNeed = query, searchQuery, need
			}
		}
		// The judge's verdict only narrows lanes that deferred to it
		// ("smart", gateNeedsLLM). always/keyword lanes fired
		// deterministically and search regardless — with the raw query,
		// normalized here: a need=false reply carries no usable rewrite
		// (the contract leaves its query field empty), so the hook never
		// trusts it for the lanes that still search.
		if !need {
			searchQuery = query
		}
		// gateSkip → off; gateFire → search; gateNeedsLLM → search only
		// when the judge approved.
		wikiOn := wikiGate == gateFire || (wikiGate == gateNeedsLLM && need)
		ftOn := ftGate == gateFire || (ftGate == gateNeedsLLM && need)
		memOn := memGate == gateFire || (memGate == gateNeedsLLM && need)
		if !wikiOn && !ftOn && !memOn {
			return
		}

		// Memory lane runs first and unconditionally of the KB branches
		// below — its injection must not be skipped by a KB early return
		// (e.g. strict-mode SkipLLM).
		if !memDone {
			memLimit := cfg.MemoryMaxResults
			if memLimit <= 0 {
				memLimit = 3
			}
			// Hard cap: each hit injects up to ~300 bytes (per-item clip
			// in injectMEMContext), so an unclamped config (e.g. 50)
			// would push 15KB of [MEM] context into every qualifying
			// turn. 10 matches the memory_search tool's own hard cap.
			if memLimit > 10 {
				memLimit = 10
			}
			memHits, memErr := memSearch(ctx, agentID, searchQuery, memLimit)
			// Memoize BEFORE branching on results so an empty result also
			// short-circuits later iterations (same rationale as lastQuery).
			lastMemQuery = query
			if memErr != nil {
				slog.Debug("kb auto-query memory lane failed", "agent", agentID, "error", memErr)
			} else if len(memHits) > 0 {
				injectMEMContext(hc, memHits)
				hc.SyntheticToolCalls = append(hc.SyntheticToolCalls, SyntheticToolCall{
					Name:   "memory_search",
					Args:   fmt.Sprintf(`{"query":%q,"limit":%d}`, searchQuery, memLimit),
					Result: buildMemoryResultSummary(memHits),
				})
				slog.Info("kb auto-query memory lane", "agent", agentID, "query", searchQuery, "hits", len(memHits))
			}
		}

		if kbDone {
			return
		}

		if cfg.SearchMode == "" {
			cfg.SearchMode = "augment"
		}
		if cfg.EmptyAction == "" {
			cfg.EmptyAction = "llm"
		}

		wikiLimit := 0
		if wikiOn {
			wikiLimit = cfg.MaxResults
			if wikiLimit <= 0 {
				wikiLimit = 5
			}
			// Hard cap, same rationale as the memory lane's memLimit: each
			// KB hit injects up to ~500 bytes, so an unclamped config would
			// push tens of KB into every qualifying turn.
			if wikiLimit > 10 {
				wikiLimit = 10
			}
		}
		ftLimit := 0
		if ftOn {
			ftLimit = cfg.FlashTodoMaxResults
			if ftLimit <= 0 {
				ftLimit = 3
			}
			if ftLimit > 10 {
				ftLimit = 10
			}
		}
		wikiThreshold := cfg.Threshold
		if wikiThreshold <= 0 || wikiThreshold > 1 {
			wikiThreshold = 0.45
		}
		ftThreshold := cfg.FlashTodoThreshold
		if ftThreshold <= 0 || ftThreshold > 1 {
			ftThreshold = 0.6
		}

		results, err := store.SearchSplit(ctx, agentID, searchQuery, wikiLimit, wikiThreshold, ftLimit, ftThreshold)
		slog.Info("kb auto-query search", "agent", agentID, "query", searchQuery, "results", len(results), "err", err)

		if err != nil {
			slog.Debug("kb auto-query failed", "agent", agentID, "error", err)
			return
		}

		// Mark the query as cached BEFORE branching on results so a
		// search that returned 0 hits is also memoized — otherwise
		// an empty-result query in "always" mode would re-search on
		// every iteration.
		lastQuery = query

		if len(results) > 0 {
			// Results found — number them as [K#] citation sources and
			// attach to the hook so the loop can carry them on the
			// assistant message for the web UI's clickable badges.
			citations, sources := numberKBResults(results)
			hc.KnowledgeSources = sources
			hc.SyntheticToolCalls = append(hc.SyntheticToolCalls, SyntheticToolCall{
				Name:   "knowledgebase_search",
				Args:   fmt.Sprintf(`{"query":"%s","limit":%d}`, searchQuery, wikiLimit+ftLimit),
				Result: buildToolResultSummary(results, citations),
			})
			switch cfg.SearchMode {
			case "strict":
				content := buildKBAnswer(results, searchQuery)
				hc.PrebuiltContent = content
				hc.SkipLLM = true
				return
			default: // augment
				injectKBContext(hc, results, citations)
				return
			}
		}

		// No results — apply emptyAction.
		if cfg.EmptyAction == "stop" {
			content := indicatorNotFoundMsg()
			hc.PrebuiltContent = content
			hc.SkipLLM = true
		}
	}
}

func buildToolResultSummary(results []KBResult, citations []string) string {
	var sb strings.Builder
	for i, r := range results {
		fmt.Fprintf(&sb, "%d. **[%s] %s**", i+1, citations[i], r.SourceTitle)
		if len(r.Content) > 200 {
			sb.WriteString("\n" + softClipUTF8(r.Content, 200) + "...")
		} else {
			sb.WriteString("\n" + r.Content)
		}
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String())
}

// numberKBResults assigns each result a stable [K#] citation id by result
// order and builds the KnowledgeSource list the web UI renders as clickable
// badges. citations[i] aligns with results[i].
func numberKBResults(results []KBResult) (citations []string, sources []KnowledgeSource) {
	citations = make([]string, len(results))
	for i, r := range results {
		id := fmt.Sprintf("K%d", i+1)
		citations[i] = id
		sources = append(sources, KnowledgeSource{
			ID:       id,
			File:     r.SourceTitle,
			Kind:     r.SourceKind,
			PageType: r.PageType,
			Chunk:    r.ChunkIndex,
		})
	}
	return citations, sources
}

func extractLastUserMessage(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

// messagesContainPrefixed reports whether any message in msgs starts with
// the given lane marker ("[KB]" / "[MEM]"). injectKBContext and
// injectMEMContext prefix every injection with their marker, so a prefix
// scan is enough to confirm the previously injected context is still in
// scope for the current conversation.
func messagesContainPrefixed(msgs []provider.Message, prefix string) bool {
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, prefix) {
			return true
		}
	}
	return false
}

// insertContextMessage places msg right after a leading system message
// (or first when there is none), replacing any previous injection carrying
// the same marker — context blocks never stack across ReAct iterations.
func insertContextMessage(hc *HookContext, marker string, msg provider.Message) {
	insertAt := 0
	if len(hc.Messages) > 0 && hc.Messages[0].Role == "system" {
		insertAt = 1
	}
	var filtered []provider.Message
	filtered = append(filtered, hc.Messages[:insertAt]...)
	for _, m := range hc.Messages[insertAt:] {
		if !strings.HasPrefix(m.Content, marker) {
			filtered = append(filtered, m)
		}
	}
	tail := make([]provider.Message, len(filtered)-insertAt)
	copy(tail, filtered[insertAt:])
	hc.Messages = append(filtered[:insertAt:insertAt], msg)
	hc.Messages = append(hc.Messages, tail...)
}

// memoryRecallDirective is the framing both memory surfaces share — the
// [MEM] context injection and the synthetic memory_search result below.
// Without it the model treats recalled items as background trivia instead
// of applying them (observed on sub-flagship models). One const, so the two
// renderings of the same hits in the same request cannot drift apart.
const memoryRecallDirective = "Entries that record how this user wants things done are durable preferences: apply them to your current work by default, unless the user's latest message overrides them."

// injectMEMContext inserts a [MEM]-prefixed context message carrying the
// recalled memories, replacing any previous [MEM] injection (no stacking
// across ReAct iterations). Parallel to injectKBContext.
func injectMEMContext(hc *HookContext, hits []MemoryRecallHit) {
	var sb strings.Builder
	sb.WriteString("[MEM] Memories recalled from past conversations with this user (summaries, not verbatim quotes). " +
		memoryRecallDirective +
		" If you need more memories or the full text of one, call memory_search / memory_fetch.\n\n")
	// Total budget on top of the per-item clip: even clipped, a large hit
	// set can inject several KB into every qualifying turn. Stop at the
	// budget (always keeping at least one hit) and tell the model how to
	// see the rest. sb.Len()+len(item) matches fetch_messages.go's result
	// builder idiom; the budget includes the fixed header above.
	const memInjectBudgetBytes = 2048
	for i, h := range hits {
		content := h.Summary
		if len(content) > 300 {
			content = softClipUTF8(content, 300) + "..."
		}
		item := fmt.Sprintf("--- [M%d] %s ---\n%s\n\n", i+1, h.Topic, content)
		if sb.Len()+len(item) > memInjectBudgetBytes && i > 0 {
			fmt.Fprintf(&sb, "(+%d more memories not shown — call memory_search to see them.)\n", len(hits)-i)
			break
		}
		sb.WriteString(item)
	}

	memMsg := provider.Message{
		Role:    "user",
		Content: sb.String(),
		// Runtime injection, not a user turn: keeps the block out of web
		// history, the FTS index, the diary, and the auto-persist /
		// auto-title turn counts. The synthetic memory_search pair emitted
		// alongside it is the visible form.
		Origin: provider.OriginInjected,
	}

	insertContextMessage(hc, "[MEM]", memMsg)
}

// buildMemoryResultSummary renders the memory lane's synthetic
// memory_search result (UI visibility of the auto-recall).
func buildMemoryResultSummary(hits []MemoryRecallHit) string {
	var sb strings.Builder
	// Shared framing const rather than a second copy: the [MEM] injection
	// above and this tool_result are seen by the model in the same request,
	// and two hand-maintained directives would drift.
	sb.WriteString("Recalled from past conversations. " + memoryRecallDirective + "\n\n")
	for i, h := range hits {
		// Same 300-byte clip as the [MEM] injection: this renders into the
		// synthetic tool_result the model also sees, and it must not be the
		// unbounded copy of a bounded injection.
		content := h.Summary
		if len(content) > 300 {
			content = softClipUTF8(content, 300) + "..."
		}
		fmt.Fprintf(&sb, "%d. **%s** — %s", i+1, h.Topic, content)
		if i < len(hits)-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// laneGate is the three-state trigger verdict for one auto-recall group.
type laneGate uint8

const (
	gateSkip     laneGate = iota // off, or trigger conditions not met
	gateFire                     // search now — no judge call needed
	gateNeedsLLM                 // "smart" without a keyword hit — the shared judge call decides
)

// laneGateFor classifies one auto-recall group's trigger, given its
// AutoMode ("always" / "smart" / "keyword" / "disabled") and keywords.
// "smart": a keyword hit fires immediately (the deterministic fast
// path); a miss defers to the shared judge call's need verdict.
func laneGateFor(autoMode, query string, keywords []string) laneGate {
	switch autoMode {
	case "always":
		return gateFire
	case "smart":
		if containsAnyKeyword(query, keywords) {
			return gateFire
		}
		return gateNeedsLLM
	case "keyword":
		if containsAnyKeyword(query, keywords) {
			return gateFire
		}
		return gateSkip
	default:
		return gateSkip
	}
}

// degradeAutoModeForChatOnly maps "always" and "smart" to "keyword" and
// leaves every other mode untouched. Used by chat-only turns to silence
// proactive recall (and skip its judge calls) without turning off lanes
// the user configured, and without enabling lanes that were off (""
// stays "").
func degradeAutoModeForChatOnly(mode string) string {
	if mode == "always" || mode == "smart" {
		return "keyword"
	}
	return mode
}

// ackWords is the whole-message acknowledgement set for the layer-0
// short-circuit: pure reactions that never warrant a recall search, an
// injection, or the judge call itself. Whole-match only — checked by
// exact equality after trim/punctuation strip, never substring, so
// "好的，那 X 再整理一下" falls through to the normal gates.
var ackWords = map[string]bool{
	"好的": true, "好": true, "好嘞": true, "好吧": true,
	"嗯": true, "嗯嗯": true, "嗯呢": true, "哦": true, "噢": true,
	"ok": true, "okay": true, "行": true, "对": true, "是的": true,
	"收到": true, "明白": true, "知道了": true, "清楚了": true,
	"继续": true, "接着说": true, "那先这样": true, "先这样": true,
	"谢谢": true, "感谢": true, "辛苦了": true,
}

// isPureAckMessage reports whether s is nothing but an acknowledgement,
// optionally followed by trailing punctuation. Layer 0 of the trigger
// stack: cheaper than every other gate and applies to all modes.
func isPureAckMessage(s string) bool {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimRight(s, "。！!？?，,、~～…;. ")
	// No re-trim needed: the cutset already contains the space, so nothing
	// can trail the stripped punctuation.
	return ackWords[s]
}

func containsAnyKeyword(text string, keywords []string) bool {
	lower := strings.ToLower(text)
	for _, kw := range keywords {
		if kw != "" && strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

func buildKBAnswer(results []KBResult, query string) string {
	var sb strings.Builder
	for i, r := range results {
		fmt.Fprintf(&sb, "--- Source: %s (chunk %d) ---\n", r.SourceTitle, r.ChunkIndex)
		sb.WriteString(r.Content)
		if i < len(results)-1 {
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

func injectKBContext(hc *HookContext, results []KBResult, citations []string) {
	var sb strings.Builder
	// Fixed [KB] marker prefix so messagesContainKBContext's cache-hit
	// check still recognizes this injection across ReAct iterations.
	sb.WriteString("[KB] The following information was retrieved from the knowledge base and may be relevant to the user's question. Use it to enhance your response if relevant.\n\n")
	for i, r := range results {
		fmt.Fprintf(&sb, "--- [%s] Source: %s (chunk %d) ---\n", citations[i], r.SourceTitle, r.ChunkIndex)
		content := r.Content
		if len(content) > 500 {
			content = softClipUTF8(content, 500) + "..."
		}
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}
	switch {
	case len(citations) >= 2:
		sb.WriteString(fmt.Sprintf("When you use a fact from the sources above, cite it inline with the bracketed id, e.g. [%s]; if several sources support a point, cite all of them, e.g. [%s][%s].\n\n", citations[0], citations[0], citations[1]))
	case len(citations) == 1:
		sb.WriteString(fmt.Sprintf("When you use a fact from the sources above, cite it inline with the bracketed id, e.g. [%s].\n\n", citations[0]))
	default:
		sb.WriteString("When you use a fact from the sources above, cite it inline with the bracketed id.\n\n")
	}

	kbMsg := provider.Message{
		Role:    "user",
		Content: sb.String(),
		// Same as the [MEM] injection: runtime context, not a user turn.
		Origin: provider.OriginInjected,
	}

	insertContextMessage(hc, "[KB]", kbMsg)
}

func indicatorNotFoundMsg() string {
	return "[KB] 知识库中未找到相关信息。"
}
