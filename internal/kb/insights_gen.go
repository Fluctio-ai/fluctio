package kb

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// InsightInvoker is the LLM call callback GenerateInsights uses. Decoupled
// from provider.Provider so the kb package needn't depend on a concrete
// provider — the caller (agent manager / HTTP handler) wires it to prov.Chat,
// applying JSON-mode + no-thinking, mirroring the wiki-autogen invoker pattern.
type InsightInvoker func(ctx context.Context, messages []provider.Message) (string, error)

// insightMaxContentChars caps how much of the source text is fed to the LLM.
// ~30k chars ≈ 12-15k tokens, leaving room for the prompt plus an 8192-token
// output inside a typical 32k+ context window. Longer articles are truncated
// with a marker so the model knows it is seeing a prefix.
const insightMaxContentChars = 30000

// GenerateInsights runs the deep-reading LLM pass over one article source and
// stores the four sections (summary / quotes / actions / sprouts). Returns the
// parsed insights so the caller can echo them without a second DB read. Errors
// when the source is missing, not an article, has no text, the invoker fails,
// or the response is unparseable.
func (s *KBStore) GenerateInsights(ctx context.Context, agentID, sourceID string, invoker InsightInvoker, model string, maxTokens int) (*ArticleInsights, error) {
	if invoker == nil {
		return nil, fmt.Errorf("insight invoker not configured")
	}
	_ = model // reserved for diagnostics / future per-call routing
	if maxTokens <= 0 {
		maxTokens = 8192
	}

	// 1. Validate the source is an article owned by this agent.
	var srcType, title string
	err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT type, title FROM kb_sources WHERE id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		sourceID, agentID).Scan(&srcType, &title)
	if err != nil {
		return nil, fmt.Errorf("source not found: %w", err)
	}
	if srcType == "flash" || srcType == "todo" {
		return nil, fmt.Errorf("source %s is a %s, not an article — insights only apply to articles", sourceID, srcType)
	}

	// 2. Read the full text.
	entries, err := s.ListEntries(ctx, agentID, sourceID, 10000, 0)
	if err != nil {
		return nil, fmt.Errorf("read entries: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("source %s has no text to analyze", sourceID)
	}
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(e.Content)
		sb.WriteString("\n\n")
	}
	content := sb.String()
	truncated := false
	if len(content) > insightMaxContentChars {
		content = clipUTF8(content, insightMaxContentChars) + "\n\n…（原文过长，已截断）"
		truncated = true
	}

	// 3. Build prompt + call the LLM.
	prompt := buildInsightPrompt(title, content, truncated)
	raw, err := invoker(ctx, []provider.Message{{Role: "user", Content: prompt}})
	if err != nil {
		slog.Warn("insights: LLM call failed", "agent", agentID, "source", sourceID, "error", err)
		return nil, fmt.Errorf("insight LLM call: %w", err)
	}

	// 4. Parse — decode into a raw map first so a missing/extra/malformed
	// section degrades to "that section is empty" rather than failing the
	// whole parse.
	cleaned := stripInsightJSONFence(strings.TrimSpace(raw))
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(cleaned), &doc); err != nil {
		preview := cleaned
		if len(preview) > 200 {
			preview = preview[:200] + "…"
		}
		slog.Warn("insights: parse JSON failed", "agent", agentID, "source", sourceID, "preview", preview)
		return nil, fmt.Errorf("parse insight JSON: %w (preview: %s)", err, preview)
	}
	ins := &ArticleInsights{SourceID: sourceID}
	if v, ok := doc["summary"]; ok {
		_ = json.Unmarshal(v, &ins.Summary)
	}
	if v, ok := doc["quotes"]; ok {
		_ = json.Unmarshal(v, &ins.Quotes)
	}
	if v, ok := doc["actions"]; ok {
		_ = json.Unmarshal(v, &ins.Actions)
	}
	if v, ok := doc["sprouts"]; ok {
		_ = json.Unmarshal(v, &ins.Sprouts)
	}

	// Ground each quote against the source text: a quote is "verified" only
	// when it appears verbatim (modulo whitespace) in the article — the
	// contract the prompt asks for but the LLM doesn't always honour. This
	// is a deterministic check (we look for the bytes, not ask the LLM
	// whether its own quote is real), mirroring verify_claim / Hermes's
	// grounded-citations philosophy.
	normContent := normalizeForMatch(content)
	for i := range ins.Quotes {
		q := &ins.Quotes[i]
		if q.Text == "" {
			continue
		}
		q.Verified = strings.Contains(normContent, normalizeForMatch(q.Text))
	}

	// 5. Persist.
	if err := s.SaveInsights(ctx, agentID, sourceID, ins); err != nil {
		return nil, err
	}
	return ins, nil
}

// buildInsightPrompt assembles the deep-reading prompt: title, optional
// truncation notice, the (possibly clipped) full text, and a rigid JSON
// contract. Written in Chinese since the output is Chinese content; the
// contract bans Chinese quotes (they collide with JSON delimiters) per the
// sheng-gen-fa-ya skill notes.
func buildInsightPrompt(title, content string, truncated bool) string {
	truncNote := ""
	if truncated {
		truncNote = "\n（注意：原文过长，已截断，请基于可见部分生成解读）"
	}
	return fmt.Sprintf(`你是深度阅读助手。对下面的文章进行深度解读，严格按指定 JSON 格式输出（只输出 JSON 对象，不要 markdown 代码块，不要使用中文引号）。

文章标题：%s%s

文章全文：
%s

输出 JSON 格式：
{
  "summary": {
    "core": "2-3句话核心总结",
    "topics": [{"heading": "主题名", "points": [{"label": "要点名", "text": "具体说明"}]}],
    "chapters": [{"title": "章节标题", "body": "2-3句章节概要"}]
  },
  "quotes": [{"text": "原文原话（逐字不改写）", "tag": "分类标签"}],
  "actions": ["行动事项"],
  "sprouts": {
    "intro": "发芽引言",
    "items": [{"index": 1, "emoji": "🌱", "title": "拓展点标题", "seed": "与原文的关联", "body": "拓展正文", "aha": "Aha瞬间"}],
    "echo": {
      "seed_quote": "原文金句",
      "seed_comment": "为什么选这句",
      "items": [{"perspective": "哲学/心理学/文学", "label": "标签", "quote": "印证名言", "source": "出处"}]
    }
  }
}

要求：
- summary.core：2-3句概括全文主旨
- summary.topics：按内容逻辑分3-6组主题，每组2-4个要点（label+text）
- summary.chapters：按原文实际结构划分章节，每章2-3句提炼；仅当原文自带时间标记才出现时间，不要编造时间轴
- quotes：只收录原文原话（逐字不改写），3-8句，每句带分类标签
- actions：文中明示或暗示的行动事项；若无则空数组 []
- sprouts.items：3-5个知识拓展点，每个含种子(关联原文)+正文(数据/引用)+Aha瞬间
- sprouts.echo：选一句最有穿透力的原文金句，从哲学、心理学、文学（至少两个）视角用名言交叉印证
- 所有文本字段必须是字符串（不能是数组），需要分段时用 \n
- 不要使用中文引号，以免与 JSON 的双引号冲突
- 只输出上述 JSON 对象，不要任何前后缀文字`, title, truncNote, content)
}

// stripInsightJSONFence trims an optional ```json … ``` fence many tuned
// models wrap around structured output despite the prompt asking for raw JSON.
func stripInsightJSONFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if idx := strings.IndexByte(s, '\n'); idx >= 0 {
			s = s[idx+1:]
		}
		s = strings.TrimSpace(s)
	}
	return strings.TrimSpace(strings.TrimSuffix(s, "```"))
}
