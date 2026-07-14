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
		slug := slugify(plan.Summary)
		pages = append(pages, planPage{
			PageType: PageTypeSource,
			Slug:     slug,
			Title:    plan.Summary,
		})
	}

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
			genPrompt := buildGenerationPrompt(pp, sourceID, sourceText, indexExcerpt, analysisText)
			genText, err := g.invoke(ctx, []provider.Message{
				{Role: "system", Content: generationSystemPrompt},
				{Role: "user", Content: genPrompt},
			})
			if err != nil {
				slog.Warn("wiki page generation failed", "page_id", pageID, "error", err)
				result.PagesFailed++
				continue
			}
			body = stripFrontmatter(stripCodeFences(genText))
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

	// Step 3: Create wikilinks
	for _, wl := range plan.Wikilinks {
		if wl.Src == "" || wl.Dst == "" {
			continue
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
	entries, _, err := g.kbStore.ListAllEntries(ctx, agentID, "", 10000, 0)
	if err != nil {
		return ""
	}
	var chunks []string
	for _, e := range entries {
		if e.SourceID == sourceID {
			chunks = append(chunks, e.Content)
		}
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

const analysisSystemPrompt = `你是一位专业的研究分析员。阅读原始资料并生成结构化分析。内部推理——仅输出简洁的结构化最终分析。不要前言，不要思考标记。

以简体中文撰写所有分析内容。

你必须覆盖以下分析维度：

## 1. 文档定位
- 该文档在领域中的角色与权威性
- 核心主题与目标受众

## 2. 结构识别
按文档自身的层级逐层识别：
- 顶层结构
- 二级结构
- 三级结构
- 列出关键条目

## 3. 关键实体
提取以下实体类型：
- 核心主题（作为顶层实体）
- 主要子主题（当独立成体系时）
- 涉及的人物、组织、产品或概念框架
- 相关的机构或系统

## 4. 关键概念
提取核心概念、原理、定义、理论框架。每个概念标注：
- 名称与简明定义
- 主要出处
- 与该文档其他部分的关联

## 5. 条目关系分析
- "定义"关系：某处定义了某概念，后被其他位置使用
- "引用"关系：某处明确引用另一处
- "补充"关系：某处对另一处做细化或补充说明
- "统领"关系：总述部分统领下属内容

## 6. 现存知识库关联（去重检查）
- 扫描现存知识库索引，找出该来源涉及到的已有页面
- **重要**：若某个实体/概念在已有页面中已存在，必须复用其 slug，不要创建新页面
- 对新来源是对已有页面的强化、挑战还是补充进行标注
- 若两个概念内容高度重叠但名称不同，应合并为一个页面（选用更通用的名称）

## 7. 页面生成建议
- 建议新建的页面——仅当该实体/概念在已有索引中不存在时才创建，优先高质量深度页面，建议 8-15 页，而非大量浅层存根
- 建议更新的已有页面——若已有页面需要补充该来源的信息，标注其 slug 并要求更新
- 禁止创建内容与已有页面重复的新页面。若发现重复，改为建议更新已有页面
- 建议的标签与页面间 wikilinks

分析完成后，在末尾附加 JSON 调度计划：

---DISPATCH PLAN---
` + "```" + `json
{
  "summary": "<一句话总结>",
  "pages": [
    {
      "page_type": "entity|concept|source|query",
      "slug": "kebab-case-ascii",
      "title": "中文标题",
      "rationale": "为何应创建此页面",
      "tags": ["t1"],
      "aliases": ["别名"],
      "sources": ["source-id"]
    }
  ],
  "wikilinks": [
    {"src": "type:slug", "dst": "type:slug", "relation": "defines|cites|supplements|excepts|parent_of|child_of"}
  ]
}
` + "```" + `
每个计划必须包含恰好一个 source 页面。
source 页面必须包含完整原文，逐字照录，绝不摘要。
始终倾向于较少的、更丰富的页面，而非大量浅层存根。`

const generationSystemPrompt = `你是知识库维护者，负责撰写一篇知识库页面。

语言要求：所有输出必须以简体中文撰写。仅 [[type:slug]] wikilinks 保持 ASCII 格式。

输出规则：
- 仅输出页面的完整 markdown 正文，直接以「# 标题」开头。
- 不要解释、不要任何外围包裹。
- 严禁输出 YAML frontmatter（开头的 --- 块），也不要在正文里列出 type、slug、title、tags、aliases、sources 等元数据字段——这些由系统单独存储，写进正文会污染页面。
- 交叉引用使用 [[type:slug]] 语法。对每个提及的知识库页面均慷慨使用。
- 所有主张如源自某来源，必须以括号引用标注来源出处。不得编造来源。

页面结构指导：

【实体页面（entity）—— 如一个主题、人物、组织、产品、事件】：
# 标题
## 概述（是什么、为什么重要、在领域中的位置）
## 核心内容（主要方面、关键要点）
## 关键条目（核心内容与要旨，标注出处）
## 与其他实体的关系（引用、补充、对比）
## 相关概念（链接到概念页面）

【概念页面（concept）—— 如原理、定义、方法论、理论框架】：
# 标题
## 定义
## 出处与依据（标注原始来源的具体位置）
## 适用范围与条件
## 相关概念辨析（与相似概念的区别）
## 实践意义

【来源页面（source）—— 原始资料】：
必须包含完整原文，逐字照录。不摘要。不重组。不写概述。

更新已有页面时：
- 保留仍然正确的事实——不要为了风格而重写。
- 如新信息与已有主张矛盾，保留双方表述，等待人工裁决。
- 如新信息替代旧主张，追加备注，不删除旧主张。`

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
		truncated = truncated[:50000]
	}
	indexSection := ""
	if indexExcerpt != "" {
		indexSection = fmt.Sprintf(`现存知识库索引：
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

撰写完整的 markdown 页面。使用中文。使用 [[wikilinks]] 链接相关页面。标注来源出处。`, pp.PageType, pp.Slug, pp.Title, pp.Tags, pp.Aliases, sources, pp.Rationale, indexSection, analysisSection, truncated)
}

// --- Helpers ---

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
		if m := jsonBlockRe.FindString(s); m != "" {
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
		text = text[:maxChars-1] + "…"
	}
	return text
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
