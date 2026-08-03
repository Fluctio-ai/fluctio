package kb

import (
	"context"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// TestMergeArticles seeds an article, runs MergeArticles with a mock invoker
// that fuses old+new, and verifies the source's chunks are replaced with the
// merged text and wiki_dirty_at is stamped so the autogen sweep will rebuild.
func TestMergeArticles(t *testing.T) {
	db := setupKBVectorTestDB(t)
	store := NewKBStore(db, "sqlite")
	ctx := context.Background()
	const agent = "agt_merge"

	sid, err := store.IngestText(ctx, agent, "原标题", "旧文章正文 段落A。步骤一完成。", "text", "manual")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Mock invoker returns a merged body retaining both old and new content.
	invoker := InsightInvoker(func(ctx context.Context, msgs []provider.Message) (string, error) {
		return "# 合并后标题\n\n旧文章正文 段落A。步骤一完成。\n\n新增内容 段落B。补充步骤。", nil
	})

	merged, err := store.MergeArticles(ctx, agent, sid, "新增内容 段落B。补充步骤。", invoker, "test", 1024)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !strings.Contains(merged, "段落A") || !strings.Contains(merged, "段落B") {
		t.Errorf("merged text lost content: %q", merged)
	}

	// Entries should now hold the merged text, not just the old chunk.
	entries, _ := store.ListEntries(ctx, agent, sid, 100, 0)
	var body strings.Builder
	for _, e := range entries {
		body.WriteString(e.Content)
		body.WriteString("\n")
	}
	if !strings.Contains(body.String(), "段落A") || !strings.Contains(body.String(), "段落B") {
		t.Errorf("entries not replaced with merged text: %q", body.String())
	}

	// wiki_dirty_at must be stamped so autogen rebuilds derived pages.
	var dirty *string
	_ = db.QueryRow("SELECT wiki_dirty_at FROM kb_sources WHERE id = ?", sid).Scan(&dirty)
	if dirty == nil || *dirty == "" {
		t.Errorf("wiki_dirty_at not set after merge")
	}
}
