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

// AutoQueryHook returns a function suitable for use as a BeforeModelCall
// hook. The cfgFn callback reads the agent's current KB config on each
// call so changes take effect without restart.
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
func AutoQueryHook(store *KBStore, agentID string, cfgFn func() AutoQueryCfg, memSearch MemoryAutoSearcher) func(context.Context, *HookContext) {
	var lastQuery string
	var lastMemQuery string
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

		// Each group triggers independently. A group fires when it is
		// enabled, its AutoMode isn't "disabled", and (always mode, or
		// keyword mode matches one of its keywords). KB lanes need the
		// KBStore; the memory lane only needs its own searcher.
		wikiOn := store != nil && cfg.Enabled && cfg.AutoMode != "disabled" && groupTriggered(cfg.AutoMode, query, cfg.Keywords)
		ftOn := store != nil && cfg.FlashTodoEnabled && cfg.FlashTodoAutoMode != "disabled" && groupTriggered(cfg.FlashTodoAutoMode, query, cfg.FlashTodoKeywords)
		// Memory lane defaults ON: empty mode = "always". The trigger-gap
		// fix only works if recall fires without the LLM choosing to ask.
		memMode := cfg.MemoryAutoMode
		if memMode == "" {
			memMode = "always"
		}
		memOn := memSearch != nil && groupTriggered(memMode, query, cfg.MemoryKeywords)
		if !wikiOn && !ftOn && !memOn {
			return
		}

		// Cache hit: same query already processed AND the injection it
		// produced is still in hc.Messages. Per-lane caches: a KB lane
		// checks its [KB] injection, the memory lane its [MEM] one.
		kbDone := !wikiOn && !ftOn || lastQuery == query && messagesContainPrefixed(hc.Messages, "[KB]")
		memDone := !memOn || lastMemQuery == query && messagesContainPrefixed(hc.Messages, "[MEM]")
		if kbDone && memDone {
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
			memHits, memErr := memSearch(ctx, agentID, query, memLimit)
			// Memoize BEFORE branching on results so an empty result also
			// short-circuits later iterations (same rationale as lastQuery).
			lastMemQuery = query
			if memErr != nil {
				slog.Debug("kb auto-query memory lane failed", "agent", agentID, "error", memErr)
			} else if len(memHits) > 0 {
				injectMEMContext(hc, memHits)
				hc.SyntheticToolCalls = append(hc.SyntheticToolCalls, SyntheticToolCall{
					Name:   "memory_search",
					Args:   fmt.Sprintf(`{"query":%q,"limit":%d}`, query, memLimit),
					Result: buildMemoryResultSummary(memHits),
				})
				slog.Info("kb auto-query memory lane", "agent", agentID, "query", query, "hits", len(memHits))
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
		}
		ftLimit := 0
		if ftOn {
			ftLimit = cfg.FlashTodoMaxResults
			if ftLimit <= 0 {
				ftLimit = 3
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

		results, err := store.SearchSplit(ctx, agentID, query, wikiLimit, wikiThreshold, ftLimit, ftThreshold)
		slog.Info("kb auto-query search", "agent", agentID, "query", query, "results", len(results), "err", err)

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
				Args:   fmt.Sprintf(`{"query":"%s","limit":%d}`, query, wikiLimit+ftLimit),
				Result: buildToolResultSummary(results, citations),
			})
			switch cfg.SearchMode {
			case "strict":
				content := buildKBAnswer(results, query)
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

// injectMEMContext inserts a [MEM]-prefixed context message carrying the
// recalled memories, replacing any previous [MEM] injection (no stacking
// across ReAct iterations). Parallel to injectKBContext.
func injectMEMContext(hc *HookContext, hits []MemoryRecallHit) {
	var sb strings.Builder
	sb.WriteString("[MEM] The following are memories recalled from past conversations with this user (summaries, not verbatim quotes). Use them if relevant to the user's message.\n\n")
	for i, h := range hits {
		fmt.Fprintf(&sb, "--- [M%d] %s ---\n", i+1, h.Topic)
		content := h.Summary
		if len(content) > 300 {
			content = softClipUTF8(content, 300) + "..."
		}
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}

	memMsg := provider.Message{
		Role:    "user",
		Content: sb.String(),
	}

	insertContextMessage(hc, "[MEM]", memMsg)
}

// buildMemoryResultSummary renders the memory lane's synthetic
// memory_search result (UI visibility of the auto-recall).
func buildMemoryResultSummary(hits []MemoryRecallHit) string {
	var sb strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&sb, "%d. **%s** — %s", i+1, h.Topic, h.Summary)
		if i < len(hits)-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// groupTriggered reports whether an auto-recall group should fire for this
// query, given its AutoMode ("always" / "keyword" / "disabled") and keywords.
func groupTriggered(autoMode, query string, keywords []string) bool {
	switch autoMode {
	case "always":
		return true
	case "keyword":
		return containsAnyKeyword(query, keywords)
	default:
		return false
	}
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
	}

	insertContextMessage(hc, "[KB]", kbMsg)
}

func indicatorNotFoundMsg() string {
	return "[KB] 知识库中未找到相关信息。"
}
