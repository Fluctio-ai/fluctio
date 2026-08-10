package kb

import (
	"context"
	"testing"
)

// TestSearchSplitVectorRecallsBoth verifies that with an embedder wired up,
// SearchSplit returns wiki hits from wiki_page_embeddings and flash/todo
// hits from kb_entry_embeddings — each group independently.
func TestSearchSplitVectorRecallsBoth(t *testing.T) {
	db := setupKBVectorTestDB(t)
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	const agent = "a1"

	if _, err := db.ExecContext(ctx, `INSERT INTO wiki_pages (id, agent_id, page_type, slug, title, body, summary, source_ids, tags) VALUES ('concept:foo', ?, 'concept', 'foo', 'Foo', 'b', 's', '[]', '[]')`, agent); err != nil {
		t.Fatalf("insert wiki: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO wiki_page_embeddings (page_id, agent_id, embedding, dim, model) VALUES ('concept:foo', ?, ?, 3, 'mock')`, agent, vecToBlob([]float32{1, 0, 0})); err != nil {
		t.Fatalf("insert wiki emb: %v", err)
	}

	store := NewKBStore(db, "sqlite")
	store.SetRetriever(mockEmbedder{}, nil)
	flashID, err := store.SaveFlash(ctx, agent, "灵感 about milk")
	if err != nil {
		t.Fatalf("SaveFlash: %v", err)
	}
	store.embedSourceEntries(ctx, agent, flashID)

	results, err := store.SearchSplit(ctx, agent, "milk", 5, 0.0, 5, 0.0)
	if err != nil {
		t.Fatalf("SearchSplit: %v", err)
	}
	var sawWiki, sawFlash bool
	for _, r := range results {
		if r.SourceKind == "wiki" {
			sawWiki = true
		}
		if r.ContentType == "flash" {
			sawFlash = true
		}
	}
	if !sawWiki {
		t.Errorf("expected wiki result, got %+v", results)
	}
	if !sawFlash {
		t.Errorf("expected flash result, got %+v", results)
	}
}

// TestSearchSplitNoEmbedderSkipsFlashTodo is the core guard for this change:
// without an embedder, SearchSplit's wiki group still falls back to the
// keyword path, but the flash/todo group yields NOTHING — no keyword
// fallback, because that path has no relevance gate and would surface
// loosely-related ideas into every turn.
func TestSearchSplitNoEmbedderSkipsFlashTodo(t *testing.T) {
	db := setupKBVectorTestDB(t)
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	const agent = "a1"

	// wiki page WITHOUT embedding → keyword path can still find it by
	// title+summary match (both fields scored so normalized clears 0.45).
	if _, err := db.ExecContext(ctx, `INSERT INTO wiki_pages (id, agent_id, page_type, slug, title, body, summary, source_ids, tags) VALUES ('source:s1', ?, 'source', 's1', 'milk secrets', 'body about milk', 'milk info', '[]', '[]')`, agent); err != nil {
		t.Fatalf("insert wiki: %v", err)
	}
	store := NewKBStore(db, "sqlite") // no SetRetriever
	flashID, err := store.SaveFlash(ctx, agent, "灵感 about milk and cookies")
	if err != nil {
		t.Fatalf("SaveFlash: %v", err)
	}
	// NOTE: no embedder → flash has no vector; SearchSplit must not fall back
	// to keyword for it.
	_ = flashID

	results, err := store.SearchSplit(ctx, agent, "milk", 5, 0.4, 5, 0.0)
	if err != nil {
		t.Fatalf("SearchSplit: %v", err)
	}
	var sawWiki bool
	for _, r := range results {
		if r.ContentType == "flash" || r.ContentType == "todo" {
			t.Errorf("flash/todo must NOT be recalled without embedder, got ContentType=%q title=%q", r.ContentType, r.SourceTitle)
		}
		if r.SourceKind == "wiki" {
			sawWiki = true
		}
	}
	if !sawWiki {
		t.Errorf("expected wiki keyword-path result, got %+v", results)
	}
}

// TestSearchSplitGroupLimitZero verifies that a 0 limit skips that group
// entirely: ftLimit=0 → no flash/todo even with an embedder on; wikiLimit=0
// → no wiki.
func TestSearchSplitGroupLimitZero(t *testing.T) {
	db := setupKBVectorTestDB(t)
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	const agent = "a1"

	if _, err := db.ExecContext(ctx, `INSERT INTO wiki_pages (id, agent_id, page_type, slug, title, body, summary, source_ids, tags) VALUES ('concept:foo', ?, 'concept', 'foo', 'Foo', 'b', 's', '[]', '[]')`, agent); err != nil {
		t.Fatalf("insert wiki: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO wiki_page_embeddings (page_id, agent_id, embedding, dim, model) VALUES ('concept:foo', ?, ?, 3, 'mock')`, agent, vecToBlob([]float32{1, 0, 0})); err != nil {
		t.Fatalf("insert wiki emb: %v", err)
	}

	store := NewKBStore(db, "sqlite")
	store.SetRetriever(mockEmbedder{}, nil)
	flashID, _ := store.SaveFlash(ctx, agent, "灵感 milk")
	store.embedSourceEntries(ctx, agent, flashID)

	// ftLimit=0 → flash/todo skipped, wiki present
	res1, err := store.SearchSplit(ctx, agent, "milk", 5, 0.0, 0, 0.6)
	if err != nil {
		t.Fatalf("SearchSplit ftLimit=0: %v", err)
	}
	for _, r := range res1 {
		if r.ContentType == "flash" || r.ContentType == "todo" {
			t.Errorf("flash/todo leaked with ftLimit=0: %+v", r)
		}
	}
	if len(res1) == 0 {
		t.Errorf("expected wiki result with ftLimit=0, got none")
	}

	// wikiLimit=0 → wiki skipped, only flash/todo
	res2, err := store.SearchSplit(ctx, agent, "milk", 0, 0.45, 5, 0.0)
	if err != nil {
		t.Fatalf("SearchSplit wikiLimit=0: %v", err)
	}
	for _, r := range res2 {
		if r.SourceKind == "wiki" {
			t.Errorf("wiki leaked with wikiLimit=0: %+v", r)
		}
	}
}
