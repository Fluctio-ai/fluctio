package wiki

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func setupWikiTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE wiki_pages (id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, page_type TEXT NOT NULL, slug TEXT NOT NULL, title TEXT NOT NULL, body TEXT NOT NULL, summary TEXT NOT NULL, source_ids TEXT NOT NULL, tags TEXT NOT NULL, created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, revision INTEGER NOT NULL DEFAULT 1)`,
		`CREATE TABLE wiki_page_embeddings (page_id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, embedding BLOB, dim INTEGER NOT NULL DEFAULT 0, model TEXT NOT NULL DEFAULT '', updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	return db
}

type mockEmbedder struct{}

func (mockEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{0.5, 0.5}
	}
	return out, nil
}
func (mockEmbedder) Model() string   { return "mock" }
func (mockEmbedder) Dim() int        { return 2 }
func (mockEmbedder) Available() bool { return true }

// TestReindexEmbeddings embeds every page, stores vectors, and is idempotent
// (a force re-run UPSERTs in place — count stays the same).
func TestReindexEmbeddings(t *testing.T) {
	db := setupWikiTestDB(t)
	defer db.Close()
	ctx := context.Background()
	ws := NewWikiStore(db, "sqlite")

	for _, p := range []struct{ id, pt, slug, title, summary string }{
		{"entity:foo", "entity", "foo", "Foo", "Foo summary"},
		{"concept:bar", "concept", "bar", "Bar", "Bar summary"},
	} {
		if err := ws.UpsertPage(ctx, &WikiPage{ID: p.id, AgentID: "a1", PageType: p.pt, Slug: p.slug, Title: p.title, Body: "body", Summary: p.summary, SourceIDs: []string{}, Tags: []string{}}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	res, err := ReindexEmbeddings(ctx, ws, mockEmbedder{}, "a1", true, 0)
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if res.Processed != 2 {
		t.Errorf("processed = %d, want 2", res.Processed)
	}
	embs, _ := ws.ListPageEmbeddingsByAgent(ctx, "a1")
	if len(embs) != 2 {
		t.Errorf("embeddings stored = %d, want 2", len(embs))
	}

	// force re-run: UPSERT replaces, no duplicates
	res2, _ := ReindexEmbeddings(ctx, ws, mockEmbedder{}, "a1", true, 0)
	if res2.Processed != 2 {
		t.Errorf("second reindex processed = %d, want 2", res2.Processed)
	}
	embs2, _ := ws.ListPageEmbeddingsByAgent(ctx, "a1")
	if len(embs2) != 2 {
		t.Errorf("embeddings after re-run = %d, want 2 (idempotent)", len(embs2))
	}
}
