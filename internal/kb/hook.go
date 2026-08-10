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
	// Shared.
	SearchMode  string // "augment" (default), "strict"
	EmptyAction string // "llm" (default), "stop"
}

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
func AutoQueryHook(store *KBStore, agentID string, cfgFn func() AutoQueryCfg) func(context.Context, *HookContext) {
	var lastQuery string
	return func(ctx context.Context, hc *HookContext) {
		cfg := cfgFn()
		slog.Debug("kb auto-query hook", "agent", agentID, "enabled", cfg.Enabled, "mode", cfg.AutoMode, "store_nil", store == nil, "source", hc.Source)
		if store == nil {
			return
		}
		if hc.Source != "" {
			return
		}

		query := extractLastUserMessage(hc.Messages)
		if query == "" {
			return
		}

		// Each group triggers independently. A group fires when it is
		// enabled, its AutoMode isn't "disabled", and (always mode, or
		// keyword mode matches one of its keywords).
		wikiOn := cfg.Enabled && cfg.AutoMode != "disabled" && groupTriggered(cfg.AutoMode, query, cfg.Keywords)
		ftOn := cfg.FlashTodoEnabled && cfg.FlashTodoAutoMode != "disabled" && groupTriggered(cfg.FlashTodoAutoMode, query, cfg.FlashTodoKeywords)
		if !wikiOn && !ftOn {
			return
		}

		// Cache hit: same query already processed AND the [KB]
		// injection it produced is still in hc.Messages. The second
		// condition matters because the cache is per-agent-lifetime
		// (the hook closure outlives any one session): if the user
		// starts a brand-new chat with the same query, the new
		// session's messages won't carry the old [KB] injection, so
		// we must re-search to give the LLM its KB context.
		//
		// Within a single ReAct loop the prior injection is always
		// present (iter 1 put it there, iter 2+ reads it back), so
		// this hits and skips duplicate search + synth emission.
		// Across turns in the SAME session, the injection is still
		// in session_messages, so this also hits — and the LLM
		// keeps operating on the same KB context.
		if lastQuery == query && messagesContainKBContext(hc.Messages) {
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
			hc.SyntheticToolCalls = []SyntheticToolCall{{
				Name:   "knowledgebase_search",
				Args:   fmt.Sprintf(`{"query":"%s","limit":%d}`, query, wikiLimit+ftLimit),
				Result: buildToolResultSummary(results, citations),
			}}
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

// messagesContainKBContext reports whether any message in msgs carries
// an injected KB context block. injectKBContext prefixes every injection
// with "[KB]" (or the custom IndicatorFound text), so a simple prefix
// scan is enough. Used by the cache-hit check to confirm the previously
// cached injection is still in scope for the current conversation.
func messagesContainKBContext(msgs []provider.Message) bool {
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, "[KB]") {
			return true
		}
	}
	return false
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

	insertAt := 0
	if len(hc.Messages) > 0 && hc.Messages[0].Role == "system" {
		insertAt = 1
	}

	// Remove any previous KB context messages to avoid stacking across ReAct iterations.
	var filtered []provider.Message
	filtered = append(filtered, hc.Messages[:insertAt]...)
	for _, m := range hc.Messages[insertAt:] {
		if !strings.HasPrefix(m.Content, "[KB]") {
			filtered = append(filtered, m)
		}
	}
	tail := make([]provider.Message, len(filtered)-insertAt)
	copy(tail, filtered[insertAt:])
	hc.Messages = append(filtered[:insertAt:insertAt], kbMsg)
	hc.Messages = append(hc.Messages, tail...)
}

func indicatorNotFoundMsg() string {
	return "[KB] 知识库中未找到相关信息。"
}
