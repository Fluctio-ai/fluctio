package kb

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/google/uuid"
)

// mergeMaxContentChars caps each side fed to the merge LLM so old+new+prompt
// fit a typical 32k context window with room for the merged output.
const mergeMaxContentChars = 30000

const mergeSystemPrompt = `你是知识库编辑。下面会给你两版文章内容（旧版和新版/补充内容）。请合并为一篇去重、完整、连贯的 markdown 文章，遵循以下规则：

1. 保留两版中所有独特的 facts、观点、步骤、示例——不要丢失信息。
2. 去除字面重复的段落、句子、列表项。
3. 新版中更新或更准确的信息覆盖旧版的过时内容。
4. 保持合理的章节结构（标题、列表、段落），不要加入元数据字段或 YAML frontmatter。
5. 只输出合并后的 markdown 正文，不要输出解释、前后说明或任何其他内容。

语言：跟随原文（中文则中文，英文则英文）。`

const mergeUserTemplate = `请合并下面两版文章内容。

旧版（已存在于知识库）：
"""
%s
"""

新版/补充内容（待并入）：
"""
%s
"""

输出合并后的完整 markdown 正文。`

// MergeArticles merges newContent into the existing article sourceID via an
// LLM pass: old text and new content fuse into one de-duplicated, coherent
// article that replaces the source's chunks. After merge the old insights are
// dropped (so they regenerate on demand) and wiki_dirty_at is stamped so the
// next autogen sweep rebuilds derived pages. Returns the merged text. Errors
// when invoker is nil, the source isn't an article, the LLM call fails, or the
// response is empty.
func (s *KBStore) MergeArticles(ctx context.Context, agentID, sourceID, newContent string, invoker InsightInvoker, model string, maxTokens int) (string, error) {
	if invoker == nil {
		return "", fmt.Errorf("merge invoker not configured")
	}
	_ = model
	if maxTokens <= 0 {
		maxTokens = 8192
	}

	var srcType string
	err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT type FROM kb_sources WHERE id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		sourceID, agentID).Scan(&srcType)
	if err != nil {
		return "", fmt.Errorf("source not found: %w", err)
	}
	if srcType == "flash" || srcType == "todo" {
		return "", fmt.Errorf("source %s is a %s, not an article", sourceID, srcType)
	}

	entries, err := s.ListEntries(ctx, agentID, sourceID, 10000, 0)
	if err != nil {
		return "", fmt.Errorf("read entries: %w", err)
	}
	var oldParts []string
	for _, e := range entries {
		oldParts = append(oldParts, e.Content)
	}
	oldText := clipForMerge(strings.Join(oldParts, "\n\n"))
	newText := clipForMerge(newContent)

	merged, err := invoker(ctx, []provider.Message{
		{Role: "system", Content: mergeSystemPrompt},
		{Role: "user", Content: fmt.Sprintf(mergeUserTemplate, oldText, newText)},
	})
	if err != nil {
		return "", fmt.Errorf("merge LLM: %w", err)
	}
	merged = strings.TrimSpace(merged)
	if merged == "" {
		return "", fmt.Errorf("merge returned empty content")
	}

	if err := s.replaceArticleContent(ctx, agentID, sourceID, merged); err != nil {
		return "", fmt.Errorf("replace content: %w", err)
	}
	return merged, nil
}

func clipForMerge(s string) string {
	if len(s) <= mergeMaxContentChars {
		return s
	}
	// clipUTF8 (not s[:n]): a raw byte cut would cleave a multi-byte CJK
	// rune and feed replacement-glyph garbage into the merge prompt.
	return clipUTF8(s, mergeMaxContentChars) + "\n\n...(已截断)"
}

// replaceArticleContent swaps an article source's kb_entries chunks for fresh
// chunks of content and settles the merge side effects in the SAME
// transaction: drops stale chunk embeddings (which reference old entry ids),
// drops old chunks, inserts new ones, refreshes orig_content (so a future
// rewash with a changed pipeline re-chunks the merged text instead of
// silently rolling the merge back), drops stale insights (they regenerate on
// demand), and stamps wiki_dirty_at so the autogen sweep rebuilds derived
// pages. Splitting those steps across transactions could leave insights
// describing pre-merge text, or a merge that rewash would overwrite. Then
// best-effort async re-embeds so vector search reaches the merged text.
func (s *KBStore) replaceArticleContent(ctx context.Context, agentID, sourceID, content string) error {
	chunks := ChunkText(content, 0, 0)
	if len(chunks) == 0 {
		return fmt.Errorf("no content")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM kb_entry_embeddings WHERE entry_id IN (SELECT id FROM kb_entries WHERE source_id = %s AND agent_id = %s)`, s.ph(1), s.ph(2)),
		sourceID, agentID); err != nil {
		return fmt.Errorf("drop old embeddings: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM kb_entries WHERE source_id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		sourceID, agentID); err != nil {
		return fmt.Errorf("drop old chunks: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		fmt.Sprintf(`INSERT INTO kb_entries (uuid, agent_id, source_id, chunk_index, content) VALUES (%s,%s,%s,%s,%s)`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5)))
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, c := range chunks {
		if _, err := stmt.ExecContext(ctx, uuid.New().String(), agentID, sourceID, c.Index, c.Content); err != nil {
			return fmt.Errorf("insert merged chunk: %w", err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	// orig_content tracks the merged text so RewashSources never re-chunks
	// the pre-merge original over the user's confirmed merge.
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE kb_sources SET total_chars = %s, entry_count = %s, updated_at = %s,
			orig_content = %s, wiki_dirty_at = %s WHERE id = %s AND agent_id = %s`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5), s.ph(6), s.ph(7)),
		len(content), len(chunks), now, content, now, sourceID, agentID); err != nil {
		return fmt.Errorf("update source: %w", err)
	}
	// Drop stale insights in the same tx so they can't describe pre-merge
	// text (a concurrent GenerateInsights landing after this delete would
	// still race — per-source single-flight is handled at the call sites).
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM kb_article_insights WHERE source_id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		sourceID, agentID); err != nil {
		return fmt.Errorf("drop stale insights: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.embedder != nil && s.embedder.Available() {
		go s.embedSourceEntries(context.Background(), agentID, sourceID)
	}
	return nil
}
