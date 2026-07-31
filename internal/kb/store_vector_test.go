package kb

import (
	"context"
	"database/sql"
	"encoding/binary"
	"math"
	"testing"

	_ "modernc.org/sqlite"
)

func setupKBVectorTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE wiki_pages (
			id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, page_type TEXT NOT NULL,
			slug TEXT NOT NULL, title TEXT NOT NULL, body TEXT NOT NULL, summary TEXT NOT NULL,
			source_ids TEXT NOT NULL, tags TEXT NOT NULL, created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, revision INTEGER NOT NULL DEFAULT 1)`,
		`CREATE TABLE wiki_page_embeddings (
			page_id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, embedding BLOB,
			dim INTEGER NOT NULL DEFAULT 0, model TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE kb_sources (
			id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, title TEXT NOT NULL,
			source_type TEXT NOT NULL, source_ref TEXT NOT NULL,
			entry_count INTEGER NOT NULL DEFAULT 0, total_chars INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, wiki_generated_at TIMESTAMP)`,
		`CREATE TABLE kb_entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT, uuid TEXT, source_id TEXT NOT NULL,
			chunk_index INTEGER NOT NULL DEFAULT 0, content TEXT NOT NULL, agent_id TEXT NOT NULL)`,
		`CREATE TABLE kb_entry_embeddings (
			entry_id INTEGER PRIMARY KEY, agent_id TEXT NOT NULL, embedding BLOB,
			dim INTEGER NOT NULL DEFAULT 0, model TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	return db
}

type mockEmbedder struct{}

func (mockEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}
func (mockEmbedder) Model() string   { return "mock" }
func (mockEmbedder) Dim() int        { return 3 }
func (mockEmbedder) Available() bool { return true }

func vecToBlob(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

// TestSearchWikiByVector verifies the embedding path returns matching pages
// with the same KBResult shape as the keyword path.
func TestSearchWikiByVector(t *testing.T) {
	db := setupKBVectorTestDB(t)
	defer db.Close()
	ctx := context.Background()

	pages := []struct{ id, pt, slug, title, summary string }{
		{"entity:foo", "entity", "foo", "Foo", "Foo summary"},
		{"concept:bar", "concept", "bar", "Bar", "Bar summary"},
	}
	for _, p := range pages {
		if _, err := db.ExecContext(ctx, `INSERT INTO wiki_pages (id, agent_id, page_type, slug, title, body, summary, source_ids, tags) VALUES (?, 'a1', ?, ?, ?, ?, ?, '[]', '[]')`,
			p.id, p.pt, p.slug, p.title, "body "+p.title, p.summary); err != nil {
			t.Fatalf("insert page: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO wiki_page_embeddings (page_id, agent_id, embedding, dim, model) VALUES (?, 'a1', ?, 3, 'mock')`,
			p.id, vecToBlob([]float32{1, 0, 0})); err != nil {
			t.Fatalf("insert emb: %v", err)
		}
	}

	ks := NewKBStore(db, "sqlite")
	ks.SetRetriever(mockEmbedder{}, nil)

	results, err := ks.Search(ctx, "a1", "anything", 5, 0, 0.5, 0.0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 vector results, got %d: %+v", len(results), results)
	}
	if results[0].SourceKind != "wiki" {
		t.Errorf("SourceKind = %q, want wiki", results[0].SourceKind)
	}
	seen := map[string]bool{}
	for _, r := range results {
		seen[r.SourceID] = true
	}
	if !seen["entity:foo"] || !seen["concept:bar"] {
		t.Errorf("missing expected ids, got %v", seen)
	}
}

// TestSearchNoEmbedderFallsBack: with no SetRetriever the keyword path runs
// and must not panic even when the store is empty.
func TestSearchNoEmbedderFallsBack(t *testing.T) {
	db := setupKBVectorTestDB(t)
	defer db.Close()
	ctx := context.Background()
	ks := NewKBStore(db, "sqlite") // no SetRetriever
	results, err := ks.Search(ctx, "a1", "anything", 5, 0, 0.5, 0.0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results on empty store, got %d", len(results))
	}
}

// TestSearchRawByVector verifies the kb_entries embedding path returns chunks
// within the requested sources with SourceKind="kb".
func TestSearchRawByVector(t *testing.T) {
	db := setupKBVectorTestDB(t)
	defer db.Close()
	ctx := context.Background()

	db.ExecContext(ctx, `INSERT INTO kb_sources (id, agent_id, title, source_type, source_ref) VALUES ('src1', 'a1', 'Title1', 'text', 'ref')`)
	db.ExecContext(ctx, `INSERT INTO kb_entries (uuid, agent_id, source_id, chunk_index, content) VALUES ('u1', 'a1', 'src1', 0, 'chunk zero')`)
	db.ExecContext(ctx, `INSERT INTO kb_entries (uuid, agent_id, source_id, chunk_index, content) VALUES ('u2', 'a1', 'src1', 1, 'chunk one')`)
	// AUTOINCREMENT ids are 1 and 2; embed both.
	db.ExecContext(ctx, `INSERT INTO kb_entry_embeddings (entry_id, agent_id, embedding, dim, model) VALUES (1, 'a1', ?, 3, 'mock')`, vecToBlob([]float32{1, 0, 0}))
	db.ExecContext(ctx, `INSERT INTO kb_entry_embeddings (entry_id, agent_id, embedding, dim, model) VALUES (2, 'a1', ?, 3, 'mock')`, vecToBlob([]float32{1, 0, 0}))

	ks := NewKBStore(db, "sqlite")
	ks.SetRetriever(mockEmbedder{}, nil)

	results, err := ks.SearchRawKB(ctx, "a1", "anything", []string{"src1"}, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 vector results, got %d: %+v", len(results), results)
	}
	if results[0].SourceKind != "kb" {
		t.Errorf("SourceKind = %q, want kb", results[0].SourceKind)
	}
}

// TestSearchRawNoEmbedderFallsBack: without SetRetriever the keyword LIKE
// path runs and must not panic on an empty store.
func TestSearchRawNoEmbedderFallsBack(t *testing.T) {
	db := setupKBVectorTestDB(t)
	defer db.Close()
	ctx := context.Background()
	ks := NewKBStore(db, "sqlite") // no SetRetriever
	results, err := ks.SearchRawKB(ctx, "a1", "anything", nil, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results on empty store, got %d", len(results))
	}
}
// model/dim is skipped, the vector path yields nothing, and Search falls
// back to keyword (no LIKE match → empty) without error.
func TestSearchVectorDimMismatchSkipsStale(t *testing.T) {
	db := setupKBVectorTestDB(t)
	defer db.Close()
	ctx := context.Background()
	db.ExecContext(ctx, `INSERT INTO wiki_pages (id, agent_id, page_type, slug, title, body, summary, source_ids, tags) VALUES ('entity:foo', 'a1', 'entity', 'foo', 'Foo', 'b', 's', '[]', '[]')`)
	db.ExecContext(ctx, `INSERT INTO wiki_page_embeddings (page_id, agent_id, embedding, dim, model) VALUES ('entity:foo', 'a1', ?, 2, 'old')`, vecToBlob([]float32{1, 0}))
	ks := NewKBStore(db, "sqlite")
	ks.SetRetriever(mockEmbedder{}, nil)
	results, err := ks.Search(ctx, "a1", "nomatch", 5, 0, 0.5, 0.0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected dim-mismatch skip → 0, got %d", len(results))
	}
}
