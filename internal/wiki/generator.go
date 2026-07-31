package wiki

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/provider"
)

// LLMInvoker is a function that calls the LLM with messages and returns the response.
type LLMInvoker func(ctx context.Context, messages []provider.Message) (string, error)

// Generator creates wiki pages from KB source content using two-step CoT.
type Generator struct {
	store   *WikiStore
	kbStore *kb.KBStore
	invoke  LLMInvoker
}

// NewGenerator creates a wiki generator.
func NewGenerator(ws *WikiStore, kbs *kb.KBStore, invoker LLMInvoker) *Generator {
	return &Generator{store: ws, kbStore: kbs, invoke: invoker}
}

// GenerateResult holds the outcome of a wiki generation run.
type GenerateResult struct {
	PagesCreated int      `json:"pages_created"`
	PagesUpdated int      `json:"pages_updated"`
	PagesFailed  int      `json:"pages_failed"`
	EdgesAdded   int      `json:"edges_added"`
	Error        string   `json:"error,omitempty"`
	PageIDs      []string `json:"page_ids"`
}

// Generate runs the two-step pipeline for one KB source.
func (g *Generator) Generate(ctx context.Context, agentID, sourceID string) *GenerateResult {
	result := &GenerateResult{}

	// Clean up old pages from this source to avoid duplicates on re-generation.
	if n, err := g.store.DeletePagesBySource(ctx, agentID, sourceID); err == nil && n > 0 {
		slog.Info("wiki: removed old pages for source", "source", sourceID, "deleted", n)
	}

	// Read source text from KB entries
	sourceText := g.readSourceText(ctx, agentID, sourceID)
	if sourceText == "" {
		result.Error = "empty source text"
		return result
	}

	// Step 1: Analysis — LLM reads source text + existing index, outputs structured plan
	indexExcerpt := g.buildIndexExcerpt(ctx, agentID)
	sourceTitle := g.readSourceTitle(ctx, agentID, sourceID)
	analysisPrompt := buildAnalysisPrompt(sourceID, sourceTitle, sourceText, indexExcerpt)
	analysisText, err := g.invoke(ctx, []provider.Message{
		{Role: "system", Content: analysisSystemPrompt},
		{Role: "user", Content: analysisPrompt},
	})
	if err != nil {
		result.Error = fmt.Sprintf("analysis: %v", err)
		return result
	}

	// Extract JSON plan from analysis
	plan := extractPlan(analysisText)
	if plan == nil {
		// One corrective retry: tell the model its previous output had no
		// parseable dispatch plan and ask for just the JSON block. This
		// recovers transient analysis misses without failing the source.
		retryText, rerr := g.invoke(ctx, []provider.Message{
			{Role: "system", Content: analysisSystemPrompt},
			{Role: "user", Content: analysisPrompt},
			{Role: "assistant", Content: analysisText},
			{Role: "user", Content: "你上一条输出中没有可解析的调度计划。请只输出 ---DISPATCH PLAN--- 标记和一个合法的 ```json 代码块（schema 与硬性约束同前），不要输出任何其他内容。"},
		})
		if rerr == nil {
			plan = extractPlan(retryText)
		}
	}
	if plan == nil {
		// Log the tail so truncation vs. missing-JSON is diagnosable: a
		// truncated analysis ends mid-sentence with no JSON; a model that
		// skipped the dispatch plan ends with finished prose but no block.
		tail := analysisText
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		slog.Warn("wiki: could not extract plan from analysis",
			"source", sourceID, "analysis_len", len(analysisText), "analysis_tail", tail)
		result.Error = "could not extract plan from analysis"
		return result
	}
	validatePlan(plan, sourceID)
	pages := plan.Pages
	if len(pages) == 0 {
		result.Error = "no pages in plan"
		return result
	}

	// Ensure a source page exists
	hasSource := false
	for _, p := range pages {
		if p.PageType == PageTypeSource {
			hasSource = true
			break
		}
	}
	if !hasSource {
		// Source pages use the KB source id as their slug so that
		// [[source:UUID]] links — which the LLM emits using the source id
		// from the dispatch plan / sources list — actually resolve.
		// slugify(summary) produced a long Chinese slug that never matched
		// the UUID the LLM used in [[links]], leaving every source link
		// dead (22 links, only 4 resolved before this fix).
		pages = append(pages, planPage{
			PageType: PageTypeSource,
			Slug:     sourceID,
			Title:    clipTitle(plan.Summary, 30),
		})
	}

	// Assign short ref labels (E1.. for existing pages, N1.. for new ones)
	// and build a ref→type:slug map plus a ref-based index for the prompt.
	// Generation uses [[ref]] links (short, stable) instead of fragile
	// type:slug strings; postProcessLinks maps them back to canonical
	// [[type:slug]] after the body is generated.
	refMap := map[string]string{}
	validIDs := map[string]bool{}
	var refIndex strings.Builder
	if existing, _, err := g.store.ListPages(ctx, agentID, "", 200, 0); err == nil {
		for i := range existing {
			ref := fmt.Sprintf("E%d", i+1)
			refMap[ref] = existing[i].ID
			validIDs[existing[i].ID] = true
			fmt.Fprintf(&refIndex, "- [[%s]] — %s\n", ref, existing[i].Title)
		}
	}
	for i := range pages {
		ref := fmt.Sprintf("N%d", i+1)
		pages[i].Ref = ref
		id := pages[i].ID()
		refMap[ref] = id
		validIDs[id] = true
		fmt.Fprintf(&refIndex, "- [[%s]] — %s（本次新建）\n", ref, pages[i].Title)
	}
	refIndexExcerpt := strings.TrimSpace(refIndex.String())

	// Step 2: Generate each page
	for _, pp := range pages {
		pageID := pp.ID()
		if pageID == "" {
			pageID = fmt.Sprintf("%s:%s", pp.PageType, pp.Slug)
		}

		// Dedup: if a page with the same title already exists, merge source.
		if existingByTitle, _ := g.store.FindPageByTitle(ctx, agentID, pp.Title); existingByTitle != nil && existingByTitle.ID != pageID {
			if !hasSourceID(existingByTitle.SourceIDs, sourceID) {
				existingByTitle.SourceIDs = append(existingByTitle.SourceIDs, sourceID)
			}
			_ = g.store.UpsertPage(ctx, existingByTitle)
			result.PageIDs = append(result.PageIDs, existingByTitle.ID)
			continue
		}

		var body string
		if pp.PageType == PageTypeSource {
			// Source pages get the full text verbatim
			body = sourceText
		} else {
			// LLM-generated page
			genPrompt := buildGenerationPrompt(pp, sourceID, sourceText, refIndexExcerpt, analysisText)
			genText, err := g.invoke(ctx, []provider.Message{
				{Role: "system", Content: generationSystemPrompt},
				{Role: "user", Content: genPrompt},
			})
			if err != nil {
				slog.Warn("wiki page generation failed", "page_id", pageID, "error", err)
				result.PagesFailed++
				continue
			}
			body = PostProcessLinks(stripFrontmatter(stripCodeFences(genText)), refMap, validIDs)
		}

		summary := firstParagraph(body, 240)

		// Check if page exists
		existing, _ := g.store.GetPage(ctx, pageID)
		isNew := existing == nil

		wikiPage := &WikiPage{
			ID:        pageID,
			AgentID:   agentID,
			PageType:  pp.PageType,
			Slug:      pp.Slug,
			Title:     pp.Title,
			Body:      body,
			Summary:   summary,
			SourceIDs: []string{sourceID},
			Tags:      pp.Tags,
		}
		if err := g.store.UpsertPage(ctx, wikiPage); err != nil {
			slog.Warn("wiki page upsert failed", "page_id", pageID, "error", err)
			result.PagesFailed++
			continue
		}

		if isNew {
			result.PagesCreated++
		} else {
			result.PagesUpdated++
		}
		result.PageIDs = append(result.PageIDs, pageID)
	}

	// Step 3: Create wikilinks. Validate both endpoints exist (in this
	// run's PageIDs or already in the store) before upserting — no
	// dangling edges enter the graph.
	known := map[string]bool{}
	for _, id := range result.PageIDs {
		known[id] = true
	}
	for _, wl := range plan.Wikilinks {
		if wl.Src == "" || wl.Dst == "" {
			continue
		}
		if !known[wl.Src] {
			if p, _ := g.store.GetPage(ctx, wl.Src); p == nil {
				continue
			}
		}
		if !known[wl.Dst] {
			if p, _ := g.store.GetPage(ctx, wl.Dst); p == nil {
				continue
			}
		}
		link := &WikiLink{
			SrcPageID: wl.Src,
			DstPageID: wl.Dst,
			Relation:  wl.Relation,
			Weight:    0.8,
		}
		if err := g.store.UpsertLink(ctx, link); err != nil {
			slog.Debug("wiki link upsert failed", "src", wl.Src, "dst", wl.Dst, "error", err)
		} else {
			result.EdgesAdded++
		}
	}

	// Fallback: link all non-source pages to the source page
	if len(plan.Wikilinks) == 0 {
		sourceIDs := filterByType(result.PageIDs, PageTypeSource)
		otherIDs := filterNotType(result.PageIDs, PageTypeSource)
		for _, sid := range sourceIDs {
			for _, oid := range otherIDs {
				g.store.UpsertLink(ctx, &WikiLink{SrcPageID: sid, DstPageID: oid, Relation: "parent_of", Weight: 0.6})
				result.EdgesAdded++
			}
		}
	}

	return result
}

func (g *Generator) readSourceText(ctx context.Context, agentID, sourceID string) string {
	if g.kbStore == nil {
		return ""
	}
	// ListEntries filters by source_id server-side and orders by chunk_index,
	// so chunks reassemble in original document order. ListAllEntries orders by
	// row id and would scramble the text across slices.
	entries, err := g.kbStore.ListEntries(ctx, agentID, sourceID, 10000, 0)
	if err != nil {
		return ""
	}
	var chunks []string
	for _, e := range entries {
		chunks = append(chunks, e.Content)
	}
	return strings.Join(chunks, "\n\n")
}

// readSourceTitle returns the human-readable title the user gave the KB
// source, so the LLM doesn't fall back to the raw source UUID when titling
// the generated source page.
func (g *Generator) readSourceTitle(ctx context.Context, agentID, sourceID string) string {
	if g.kbStore == nil {
		return ""
	}
	sources, err := g.kbStore.ListSources(ctx, agentID, 200, 0)
	if err != nil {
		return ""
	}
	for _, s := range sources {
		if s.ID == sourceID {
			return s.Title
		}
	}
	return ""
}

// buildIndexExcerpt creates a summary of existing wiki pages for context.
func (g *Generator) buildIndexExcerpt(ctx context.Context, agentID string) string {
	if g.store == nil {
		return ""
	}
	pages, _, err := g.store.ListPages(ctx, agentID, "", 200, 0)
	if err != nil || len(pages) == 0 {
		return ""
	}
	var lines []string
	for _, p := range pages {
		if p.Summary != "" {
			lines = append(lines, fmt.Sprintf("- [[%s:%s]] — %s", p.PageType, p.Slug, p.Summary))
		} else {
			lines = append(lines, fmt.Sprintf("- [[%s:%s]] — %s", p.PageType, p.Slug, p.Title))
		}
	}
	return strings.Join(lines, "\n")
}

// --- Plan types ---

type wikiPlan struct {
	Summary   string     `json:"summary"`
	Pages     []planPage `json:"pages"`
	Wikilinks []planLink `json:"wikilinks"`
}

type planPage struct {
	PageType  string   `json:"page_type"`
	Slug      string   `json:"slug"`
	Title     string   `json:"title"`
	Rationale string   `json:"rationale"`
	Tags      []string `json:"tags"`
	Aliases   []string `json:"aliases"`
	Sources   []string `json:"sources"`
	// Ref is a short label (N1, N2...) assigned by code after analysis,
	// used as the [[ref]] link target during generation so the LLM doesn't
	// have to remember long slugs. Not part of the LLM's JSON output.
	Ref string `json:"-"`
}

func (p planPage) ID() string {
	return fmt.Sprintf("%s:%s", p.PageType, p.Slug)
}

type planLink struct {
	Src      string `json:"src"`
	Dst      string `json:"dst"`
	Relation string `json:"relation"`
}

// --- Prompt templates ---

const analysisSystemPrompt = `你是一位知识库分析员。阅读来源文本，输出「简洁分析 + JSON 调度计划」，供下游程序生成 wiki 页面。

语言：全部使用简体中文（slug、page_type、relation 等枚举值除外）。

# 输出格式（严格两段，顺序固定）

## 第一段：简洁分析（总计不超过 500 字，用要点式短句）
1. **文档定位**：主题、领域、目标受众（1-2 句）
2. **核心实体与概念**：本文档最值得建页的对象清单，各附一句定义
3. **去重检查**：对照「现存知识库索引」，逐项标注——已在库中（复用其 slug）/ 确属新增
4. **关系要点**：条目间的主要关系（定义 / 引用 / 补充 / 统领）

## 第二段：调度计划
另起一行，先输出标记：
---DISPATCH PLAN---
然后输出一个 ` + "```json" + ` 代码块，schema：

{
  "summary": "一句话总结来源内容（≤30 字，越短越好）",
  "pages": [
    {
      "page_type": "entity | concept | source | query",
      "slug": "kebab-case-ascii",
      "title": "中文标题",
      "rationale": "建页理由（一句话）",
      "tags": ["标签1", "标签2"],
      "aliases": ["别名"],
      "sources": ["来源id"]
    }
  ],
  "wikilinks": [
    {"src": "type:slug", "dst": "type:slug", "relation": "defines | cites | supplements | parent_of | child_of | excepts"}
  ]
}

# 硬性约束（违反任何一条即视为废稿）
1. pages 必须包含且仅包含 1 个 source 页面，其 slug 必须等于来源元数据中的 id。
2. 除 source 页外，新建页面 3-8 个。宁缺毋滥：只建有实质内容的深页，禁止一句话存根。
3. slug 规则：全小写 ASCII 字母/数字/连字符，≤48 字符，plan 内全局唯一。实体/概念在「现存知识库索引」中已存在时，必须原样复用其 slug，禁止另造近似 slug。
4. wikilinks 的 src 和 dst 必须采用 "type:slug" 形式，且必须指向 pages 列表内的页面或现存索引中的页面，禁止悬空。
5. relation 只能取：defines / cites / supplements / parent_of / child_of / excepts。
6. 每页 tags 2-5 个；无别名时 aliases 给空数组。
7. JSON 必须严格合法：双引号、无尾逗号、无注释、无省略号。

# 正误示例
来源是《某公司 2025 年财报》时，合格的 pages 规划：
- 1 个 source 页（slug = 来源 id）
- entity: acme-corp（公司本体）、acme-cloud-os（核心产品）
- concept: gross-margin（毛利率）、operating-cash-flow（经营现金流）
合格的 wikilinks：
- {"src": "source:<id>", "dst": "concept:gross-margin", "relation": "defines"}
- {"src": "entity:acme-corp", "dst": "entity:acme-cloud-os", "relation": "parent_of"}
不合格（禁止）：给「毛利」和「毛利率」各建一页；新建页与索引中已有页面仅标题略有差异；wikilinks 指向不在 pages/索引中的 slug。`

// ============================================================================
// 2. 生成阶段系统提示词（替换原 generationSystemPrompt）
// ============================================================================

const generationSystemPrompt = `你是知识库页面撰写者。根据用户消息提供的页面元数据、可引用页面列表、录入分析与来源原文，撰写一篇可直接渲染为网页的 wiki 页面。

# 语言
简体中文。仅 [[ref]] 链接标签、代码、专有名词保持原文。

# 输出契约（严格遵守）
1. 只输出 markdown 正文。第一行必须是 ` + "`# 页面标题`" + `，标题与元数据 title 一致。
2. 标题之后先写一段 2-4 句的概述（不加任何小标题），系统会抽取它作为页面摘要——它必须能独立概括页面主题。
3. 概述之后用 ` + "`##`" + ` / ` + "`###`" + ` 组织章节，层级最深到 ###，不得跳级。
4. 严禁 YAML frontmatter；严禁在正文中列出 type / slug / title / tags / aliases / sources 等元数据字段（系统单独存储，写入正文即污染页面）。
5. 只允许使用以下 markdown 子集（渲染器不支持其他语法）：
   - 标题：` + "`#` `##` `###`" + `
   - 段落、**加粗**、*斜体*
   - 有序 / 无序列表（子项缩进两个空格）
   - GFM 表格（| 列 | 列 |，必须含表头分隔行）
   - > 引用块
   - ` + "`行内代码`" + ` 与带语言标注的代码块
   - [[ref]] 与 [[ref|显示文字]] 链接
   禁止：原始 HTML 标签、图片语法、脚注、参考式链接定义。
6. 交叉引用：凡提及「可引用页面」列表中的页面，一律用其 ref 写成 [[ref]]。ref 必须逐字取自列表（如 [[N2]]、[[E5]])，禁止编造、禁止写成 type:slug。不在列表中的概念用纯文字描述，不套 [[ ]]。
7. 出处标注：事实性主张在句末统一标注为(出处：[[来源页ref]])。来源页的 ref 见可引用列表。禁止编造出处或编号式脚注。
8. 篇幅：concept 页 400-900 字，entity 页 600-1200 字，query 页 300-800 字。写不满就压缩结构，禁止注水重复。

# 页面骨架
【entity —— 主题 / 人物 / 组织 / 产品 / 事件】
# 标题
（概述段）
## 背景与定位
## 核心内容
## 关键事实（列表或表格，逐条标注出处）
## 相关页面（[[ref]] 链接并各附一句关系说明）

【concept —— 原理 / 定义 / 方法论 / 框架】
# 标题
（概述段：一句话定义 + 为什么重要）
## 定义
## 出处与依据
## 适用条件与边界
## 易混淆概念辨析（可用表格对比）
## 实践意义

【query —— 问题 / 主题检索页】
# 标题
（概述段：这个问题在问什么）
## 问题背景
## 要点回答（分条作答，标注出处）
## 延伸阅读（[[ref]] 链接列表）

# 完稿自检（输出前逐项核对，不要输出核对过程）
- 第一行是 # 标题
- 标题后有独立概述段，且无小标题
- 所有 [[ ]] 内的 ref 都逐字来自可引用列表
- 无 frontmatter、无 HTML、无元数据字段、无脚注
- 标题层级 ≤3 且不跳级
- 事实性主张均有(出处：[[ref]])`

func buildAnalysisPrompt(sourceID, sourceTitle, sourceText, indexExcerpt string) string {
	truncated := sourceText
	if len(truncated) > 200000 {
		truncated = truncated[:100000] + "\n\n...(省略中间部分)...\n\n" + truncated[len(truncated)-100000:]
	}
	title := sourceTitle
	if title == "" {
		title = sourceID
	}
	indexSection := ""
	if indexExcerpt != "" {
		indexSection = fmt.Sprintf("\n现存知识库索引：\n%s\n", indexExcerpt)
	}
	return fmt.Sprintf(`知识库目的：
收集并组织知识库源文本的结构化知识。

%s来源元数据：
- id:    %s
- title: %s

完整来源内容：
"""
%s
"""

输出你的结构化分析。`, indexSection, sourceID, title, truncated)
}

func buildGenerationPrompt(pp planPage, sourceID, sourceText, indexExcerpt, analysisText string) string {
	truncated := sourceText
	if len(truncated) > 50000 {
		// Keep head + tail (analysis does the same) so a long document's
		// conclusions near the end survive into page generation.
		truncated = truncated[:35000] + "\n\n...(省略中间部分)...\n\n" + truncated[len(truncated)-15000:]
	}
	indexSection := ""
	if indexExcerpt != "" {
		indexSection = fmt.Sprintf(`可引用页面（正文中用 [[ref]] 链接，ref 必须取自本列表，严禁编造）：
"""
%s
"""

`, indexExcerpt)
	}
	analysisSection := ""
	if analysisText != "" {
		analysisSection = fmt.Sprintf(`录入分析（步骤一输出）：
"""
%s
"""

`, analysisText)
	}
	sources := pp.Sources
	if sources == nil {
		sources = []string{sourceID}
	}
	return fmt.Sprintf(`待撰写页面（以下元数据仅供参考你识别页面身份，严禁写入输出正文）：
- ref:       %s（本页面的引用标签）
- type:      %s
- slug:      %s
- title:     %s
- tags:      %v
- aliases:   %v
- sources:   %v
- rationale: %s

%s%s完整来源内容：
"""
%s
"""

撰写完整的 markdown 页面。使用中文。使用 [[ref]] 链接相关页面（ref 取自上方"可引用页面"列表）。标注来源出处。`, pp.Ref, pp.PageType, pp.Slug, pp.Title, pp.Tags, pp.Aliases, sources, pp.Rationale, indexSection, analysisSection, truncated)
}

// --- Helpers ---

// extractJSONObject scans s for the first brace-balanced JSON object,
// respecting string literals and escapes. Replaces the greedy jsonBlockRe
// "\{.*\}" which grabbed wrong spans when prose contained any {...}.
func extractJSONObject(s string) string {
	for start := strings.Index(s, "{"); start >= 0; {
		depth, inStr, esc := 0, false, false
		for i := start; i < len(s); i++ {
			c := s[i]
			switch {
			case esc:
				esc = false
			case c == '\\' && inStr:
				esc = true
			case c == '"':
				inStr = !inStr
			case inStr:
				// skip string content
			case c == '{':
				depth++
			case c == '}':
				if depth--; depth == 0 {
					return s[start : i+1]
				}
			}
		}
		next := strings.Index(s[start+1:], "{")
		if next < 0 {
			return ""
		}
		start += next + 1
	}
	return ""
}

var jsonBlockRe = regexp.MustCompile("(?s)\\{.*\\}")
var codeFenceRe = regexp.MustCompile("(?s)^```\\w*\\n?|\\n?```$")

func extractPlan(text string) *wikiPlan {
	// Try multiple extraction strategies in order.
	tryParse := func(s string) (*wikiPlan, bool) {
		s = stripCodeFences(s)
		var plan wikiPlan
		if err := json.Unmarshal([]byte(s), &plan); err == nil && len(plan.Pages) > 0 {
			return &plan, true
		}
		if m := extractJSONObject(s); m != "" {
			if err := json.Unmarshal([]byte(m), &plan); err == nil && len(plan.Pages) > 0 {
				return &plan, true
			}
		}
		return nil, false
	}

	// 1. Look for DISPATCH PLAN marker (OmniKB format)
	if idx := strings.Index(text, "---DISPATCH PLAN---"); idx >= 0 {
		if plan, ok := tryParse(text[idx+len("---DISPATCH PLAN---"):]); ok {
			return plan
		}
	}
	// 2. Try the full text (some models skip the marker)
	if plan, ok := tryParse(text); ok {
		return plan
	}
	// 3. Try everything after the last code block
	if idx := strings.LastIndex(text, "```"); idx >= 0 {
		if plan, ok := tryParse(text[idx:]); ok {
			return plan
		}
	}
	return nil
}

// wikiLinkRe matches a wiki [[link]] with an optional |alias.
var wikiLinkRe = regexp.MustCompile(`\[\[([^\]\|]+)(?:\|([^\]]+))?\]\]`)

// postProcessLinks rewrites [[ref]] links in a generated page body back to
// canonical [[type:slug]] using refMap, and degrades any link whose target
// isn't a known page id (unknown ref, or a type:slug the LLM wrote directly
// that doesn't exist) to plain text — so a generated page never ships with
// dead links regardless of how the LLM behaved.
func PostProcessLinks(body string, refMap map[string]string, validIDs map[string]bool) string {
	return wikiLinkRe.ReplaceAllStringFunc(body, func(m string) string {
		sub := wikiLinkRe.FindStringSubmatch(m)
		target := strings.TrimSpace(sub[1])
		alias := sub[2]
		// ref label → canonical type:slug
		if id, ok := refMap[target]; ok {
			target = id
		}
		// Drop dead links (unknown ref / non-existent slug) to plain text.
		if !validIDs[target] {
			text := alias
			if strings.TrimSpace(text) == "" {
				text = target
			}
			return text
		}
		if alias != "" {
			return "[[" + target + "|" + alias + "]]"
		}
		return "[[" + target + "]]"
	})
}

// frontmatterRe matches a leading YAML frontmatter block (a pair of ---
// delimiters at the very start of the text), which some models emit despite
// instructions. Wiki metadata lives in dedicated DB columns, so a frontmatter
// block in the body only pollutes the rendered page.
var frontmatterRe = regexp.MustCompile(`(?s)\A\s*---[^\n]*\n.*?\n---[^\n]*\n?`)

func stripFrontmatter(s string) string {
	return strings.TrimSpace(frontmatterRe.ReplaceAllString(s, ""))
}

func stripCodeFences(text string) string {
	text = strings.TrimSpace(text)
	text = regexp.MustCompile("(?m)^```\\w*\\s*$").ReplaceAllString(text, "")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	return strings.TrimSpace(text)
}

func firstParagraph(body string, maxChars int) string {
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			if len(lines) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(s, "#") {
			continue
		}
		lines = append(lines, s)
		if len(strings.Join(lines, " ")) >= maxChars {
			break
		}
	}
	text := strings.Join(lines, " ")
	if len(text) > maxChars {
		// Back up to a UTF-8 rune boundary so we don't cleave a CJK
		// character and store a trailing U+FFFD in wiki_pages.summary.
		end := maxChars - 1
		for end > 0 && text[end]&0xC0 == 0x80 {
			end--
		}
		text = text[:end] + "…"
	}
	return text
}

var slugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

var allowedPageTypes = map[string]bool{
	"entity": true, "concept": true, "source": true, "query": true,
}

var allowedRelations = map[string]bool{
	"defines": true, "cites": true, "supplements": true,
	"parent_of": true, "child_of": true, "excepts": true,
}

// validatePlan normalizes and filters the LLM plan in place: drops
// invalid/duplicate pages, coerces bad slugs, caps page count, and
// removes dangling or malformed wikilinks. Called right after extractPlan.
func validatePlan(plan *wikiPlan, sourceID string) {
	seen := map[string]bool{}
	valid := map[string]bool{}
	pages := make([]planPage, 0, len(plan.Pages))
	for _, p := range plan.Pages {
		if !allowedPageTypes[p.PageType] || strings.TrimSpace(p.Title) == "" {
			continue
		}
		if p.PageType == PageTypeSource {
			p.Slug = sourceID // force source slug = source id (matches [[source:UUID]])
		} else if !slugRe.MatchString(p.Slug) {
			p.Slug = slugify(p.Slug)
			if !slugRe.MatchString(p.Slug) {
				p.Slug = slugify(p.Title)
			}
		}
		id := p.ID()
		if seen[id] {
			continue
		}
		seen[id] = true
		valid[id] = true
		pages = append(pages, p)
	}
	// Cap: source + at most 8 new pages.
	const maxNew = 8
	out := make([]planPage, 0, len(pages))
	newCount := 0
	for _, p := range pages {
		if p.PageType == PageTypeSource {
			out = append(out, p)
			continue
		}
		if newCount >= maxNew {
			continue
		}
		newCount++
		out = append(out, p)
	}
	plan.Pages = out

	links := make([]planLink, 0, len(plan.Wikilinks))
	for _, wl := range plan.Wikilinks {
		if wl.Src == "" || wl.Dst == "" {
			continue
		}
		if !valid[wl.Src] && !strings.Contains(wl.Src, ":") {
			continue
		}
		if !valid[wl.Dst] && !strings.Contains(wl.Dst, ":") {
			continue
		}
		if !allowedRelations[wl.Relation] {
			wl.Relation = "supplements"
		}
		links = append(links, wl)
	}
	plan.Wikilinks = links
}

// clipTitle truncates s to at most max runes, appending an ellipsis if it
// was trimmed. Used for source page titles so long LLM summaries don't
// overflow the wiki list and the knowledge-graph nodes.
func clipTitle(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func slugify(s string) string {
	s = strings.ToLower(s)
	s = regexp.MustCompile(`[^a-z0-9一-鿿]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = fmt.Sprintf("page-%d", time.Now().UnixMilli()%10000)
	}
	return s
}

func hasSourceID(ids []string, target string) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func filterByType(ids []string, pt string) []string {
	var out []string
	for _, id := range ids {
		if strings.HasPrefix(id, pt+":") {
			out = append(out, id)
		}
	}
	return out
}

func filterNotType(ids []string, pt string) []string {
	var out []string
	for _, id := range ids {
		if !strings.HasPrefix(id, pt+":") {
			out = append(out, id)
		}
	}
	return out
}
