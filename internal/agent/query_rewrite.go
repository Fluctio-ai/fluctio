package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/llmjson"
	"github.com/fluctio-ai/fluctio/internal/provider"
)

// Query-rewrite knobs. The rewrite rides the same cheap-call conventions
// as the other background extractions (conversation summaries, insights):
// summary model, thinking off, JSON mode, small output budget. The
// timeout is short on purpose — the call sits on the hot path before
// every auto-recall search, so a slow one must not stall the turn;
// timing out falls back to the raw message (and need=true, i.e. the
// lanes still search). Observed on glm-5.3: 2–3s for a bare rewrite,
// ~4.3s now that the same call also answers the need verdict — 6s
// leaves headroom without letting a degraded provider add a 10s stall
// to time-to-first-token.
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
// low-relevance input the rewrite exists to fix. The call answers two
// questions at once (one call on the hot path, not two): whether the
// message is retrieval-worthy at all (need — the "smart" auto-recall
// lanes consume this to skip searching pure acknowledgements), and the
// standalone query for the ones that are.
const queryRewritePrompt = `You judge whether the user's LATEST message warrants searching their personal memory/knowledge base, and if so turn it into one standalone search query (keyword + vector search).

First decide "need": true only when the message references something that might exist in the user's memory or knowledge base — a person, project, event, plan, fact, or a question whose answer might be stored. false for pure acknowledgements, greetings, emotional small talk and keep-me-company chitchat, and meta-talk ("好的" / "嗯嗯" / "陪我聊两句" / "那先这样"). When genuinely in doubt, need=true; a wasted search is cheaper than a missed one.

When need=true, write the query: name concrete entities instead of pronouns and keep the distinctive keywords. A bad query copies the user's sentence with its filler still in. The query must be non-empty — topic keywords are enough.

Rules:
- Resolve pronouns/ellipsis from the conversation context: "那个方案" → the plan's actual subject; "他" → the person's name.
- Keep the user's language (Chinese in → Chinese out).
- Drop greetings, filler and meta-talk (你觉得呢 / 帮我看看 / 谢谢 / 那先这样).
- If the latest message is already standalone, return it trimmed of filler only — keep its exact keywords, do NOT paraphrase them away.
- Keywords over sentences; keep it short (under ~50 characters).

Output JSON only: {"need": true, "query": "..."}

Examples:
- Context: discussed migrating deployment from Docker to Kubernetes. Latest: "那个迁移现在什么状态了" → {"need": true, "query": "Docker 迁移 Kubernetes 状态"}
- Context mentioned 小毛驴飞天图片. Latest: "帮我把上次说的那个图发我一下呗" → {"need": true, "query": "小毛驴飞天图片"}
- No context. Latest: "芃芃最近学习怎么样" → {"need": true, "query": "芃芃 学习情况"}
- Latest: "嗯嗯好那先这样" → {"need": false, "query": ""}
- Latest: "好的" → {"need": false, "query": ""}
- Latest: "唉，今天有点累，随便陪我聊两句，放松一下" → {"need": false, "query": ""}

Conversation context (most recent last, may be empty):
%s

Latest message to rewrite:
%s`

// rewriteQuery judges and rewrites the raw last-user-message in one cheap
// LLM call: the verdict (need=false → nothing retrieval-worthy in this
// message, the "smart" auto-recall lanes skip searching) plus the
// standalone query for the messages that are. Total best-effort: on any
// failure (no provider, call error, timeout, unparsable or unusable
// output) it returns `query` with need=true — recall quality only, never
// recall availability. Need is a *bool so a response that omits the
// field (older models, JSON drift) reads as true, not as a skip verdict.
func rewriteQuery(ctx context.Context, prov provider.Provider, model string, messages []provider.Message, query string) (string, bool) {
	if prov == nil || strings.TrimSpace(query) == "" {
		return query, true
	}

	prompt := fmt.Sprintf(queryRewritePrompt, clipRewriteContext(messages), query)
	rctx, cancel := context.WithTimeout(provider.WithJSONMode(provider.WithNoThinking(ctx)), queryRewriteTimeout)
	defer cancel()
	resp, err := prov.Chat(rctx, []provider.Message{{Role: "user", Content: prompt}}, nil, model, queryRewriteMaxTok, 0.3)
	if err != nil {
		slog.Debug("query rewrite: llm call failed, using raw query", "error", err)
		return query, true
	}

	var out struct {
		Need  *bool  `json:"need"`
		Query string `json:"query"`
	}
	if err := llmjson.UnmarshalLLM(resp.Content, &out); err != nil {
		slog.Debug("query rewrite: parse failed, using raw query", "error", err)
		return query, true
	}
	if out.Need != nil && !*out.Need {
		// Explicit "nothing retrieval-worthy" verdict — the smart lanes'
		// skip signal. The raw query comes back so always/keyword lanes
		// (which ignore the verdict) keep their existing raw-search path.
		return query, false
	}
	q := strings.TrimSpace(out.Query)
	if q == "" || len(q) > queryRewriteMaxBytes {
		// Unusable output → raw + need=true (defensive: the prompt forbids
		// an empty query when need=true, so this is model drift, not a
		// semantic signal — the lanes still gate on their own floors).
		return query, true
	}
	if q != query {
		slog.Debug("query rewrite", "raw", query, "rewritten", q)
	}
	return q, true
}

// clipRewriteContext renders the recent non-system messages as the
// prompt's resolution context: last queryRewriteCtxMsgs messages, each
// clipped to queryRewriteClip bytes. The message being rewritten is the
// LAST user message, so it appears here too — harmless duplication that
// keeps the helper independent of slice surgery at the caller.
func clipRewriteContext(messages []provider.Message) string {
	// Walk back to the start of the last N non-system messages, then
	// write them forward in conversational order.
	start, seen := len(messages), 0
	for i := len(messages) - 1; i >= 0 && seen < queryRewriteCtxMsgs; i-- {
		if messages[i].Role != "system" {
			start = i
			seen++
		}
	}
	if seen == 0 {
		return "(none)"
	}
	var sb strings.Builder
	for _, m := range messages[start:] {
		if m.Role == "system" {
			continue
		}
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
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// makeQueryRewriter adapts rewriteQuery into the kb.QueryRewriter
// contract for the auto-recall hook. Nil provider → nil rewriter (the
// hook then searches with the raw message and every "smart" lane
// degrades to always-fire — the documented no-rewrite path where the
// verdict defaults to need=true). overrideModel (AgentKBCfg.QueryRewriteModel)
// beats the summary model; the chain resolves per call so
// summary-config changes apply without an agent rebuild.
func (a *Agent) makeQueryRewriter(overrideModel string) kb.QueryRewriter {
	if a.provider == nil {
		return nil
	}
	return func(ctx context.Context, messages []provider.Message, query string) (string, bool) {
		model := overrideModel
		if model == "" {
			model = a.summaryModelFor()
		}
		return rewriteQuery(ctx, a.bgProvider(), model, messages, query)
	}
}
