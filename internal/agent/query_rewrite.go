package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/llmjson"
	"github.com/fluctio-ai/fluctio/internal/provider"
)

// Query-rewrite knobs. The rewrite rides the same cheap-call conventions
// as the other background extractions (conversation summaries, insights):
// summary model, thinking off, JSON mode, small output budget. The
// timeout is short on purpose — the rewrite sits on the hot path before
// every auto-recall search, so a slow rewrite must not stall the turn;
// timing out falls back to the raw message.
const (
	queryRewriteTimeout  = 6 * time.Second
	queryRewriteMaxTok   = 256
	queryRewriteCtxMsgs  = 6   // recent messages fed as resolution context
	queryRewriteClip     = 200 // per-message byte clip inside the prompt
	queryRewriteMaxBytes = 300 // longer "query" output is rejected → raw
)

// queryRewritePrompt is deliberately example-driven (positive AND
// negative): the failure mode to avoid is the model echoing the user's
// sentence verbatim — filler, pronouns and all — which is exactly the
// low-relevance input the rewrite exists to fix.
const queryRewritePrompt = `You turn the user's LATEST message into one standalone search query for their personal memory/knowledge base (keyword + vector search).

A good query names concrete entities instead of pronouns and keeps the distinctive keywords. A bad one copies the user's sentence with its filler still in.

Rules:
- Resolve pronouns/ellipsis from the conversation context: "那个方案" → the plan's actual subject; "他" → the person's name.
- Keep the user's language (Chinese in → Chinese out).
- Drop greetings, filler and meta-talk (你觉得呢 / 帮我看看 / 谢谢 / 那先这样).
- If the latest message is already standalone, return it trimmed of filler only — keep its exact keywords, do NOT paraphrase them away.
- Keywords over sentences; keep it short (under ~50 characters).

Output JSON only: {"query": "..."}

Examples:
- Context: discussed migrating deployment from Docker to Kubernetes. Latest: "那个迁移现在什么状态了" → {"query": "Docker 迁移 Kubernetes 状态"}
- Context mentioned 小毛驴飞天图片. Latest: "帮我把上次说的那个图发我一下呗" → {"query": "小毛驴飞天图片"}
- No context. Latest: "芃芃最近学习怎么样" → {"query": "芃芃 学习情况"}
- Latest: "嗯嗯好那先这样" → {"query": ""}

Conversation context (most recent last, may be empty):
%s

Latest message to rewrite:
%s`

// rewriteQuery rewrites the raw last-user-message into a standalone
// retrieval query using one cheap LLM call. Total best-effort: on any
// failure (no provider, call error, timeout, unparsable or unusable
// output) it returns `query` unchanged — recall quality only, never
// recall availability.
func rewriteQuery(ctx context.Context, prov provider.Provider, model string, messages []provider.Message, query string) string {
	if prov == nil || strings.TrimSpace(query) == "" {
		return query
	}

	prompt := fmt.Sprintf(queryRewritePrompt, clipRewriteContext(messages), query)
	rctx, cancel := context.WithTimeout(provider.WithJSONMode(provider.WithNoThinking(ctx)), queryRewriteTimeout)
	defer cancel()
	resp, err := prov.Chat(rctx, []provider.Message{{Role: "user", Content: prompt}}, nil, model, queryRewriteMaxTok, 0.3)
	if err != nil {
		slog.Debug("query rewrite: llm call failed, using raw query", "error", err)
		return query
	}

	var out struct {
		Query string `json:"query"`
	}
	if err := llmjson.UnmarshalLLM(resp.Content, &out); err != nil {
		slog.Debug("query rewrite: parse failed, using raw query", "error", err)
		return query
	}
	q := strings.TrimSpace(out.Query)
	if q == "" || len(q) > queryRewriteMaxBytes {
		// Empty means "nothing retrieval-worthy in this message" — the
		// downstream lanes still gate on their own floors, so the raw
		// message is the safe common denominator for both cases.
		return query
	}
	if q != query {
		slog.Debug("query rewrite", "raw", query, "rewritten", q)
	}
	return q
}

// clipRewriteContext renders the recent non-system messages as the
// prompt's resolution context: last queryRewriteCtxMsgs messages, each
// clipped to queryRewriteClip bytes. The message being rewritten is the
// LAST user message, so it appears here too — harmless duplication that
// keeps the helper independent of slice surgery at the caller.
func clipRewriteContext(messages []provider.Message) string {
	var recent []provider.Message
	for i := len(messages) - 1; i >= 0 && len(recent) < queryRewriteCtxMsgs; i-- {
		if messages[i].Role != "system" {
			recent = append(recent, messages[i])
		}
	}
	if len(recent) == 0 {
		return "(none)"
	}
	var sb strings.Builder
	for i := len(recent) - 1; i >= 0; i-- { // oldest first for readability
		m := recent[i]
		fmt.Fprintf(&sb, "%s: %s\n", m.Role, clipBytes(m.Content, queryRewriteClip))
	}
	return sb.String()
}

// clipBytes truncates s to at most max bytes without splitting a rune.
func clipBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// makeQueryRewriter adapts rewriteQuery into the kb.QueryRewriter
// contract for the auto-recall hook. Nil provider → nil rewriter (the
// hook then searches with the raw message, its documented no-rewrite
// path).
func (a *Agent) makeQueryRewriter() kb.QueryRewriter {
	if a.provider == nil {
		return nil
	}
	return func(ctx context.Context, messages []provider.Message, query string) string {
		return rewriteQuery(ctx, a.bgProvider(), a.summaryModelFor(), messages, query)
	}
}
