package wiki

import (
	"context"
	"database/sql"
	"testing"
)

// setupDeleteTestDB starts from the shared in-package wiki tables, then
// adds the two extra tables the delete paths touch: wiki_links and the
// kb_cards family the card cascade writes to (mirrored DDL — see
// internal/kb/store_cards_test.go).
func setupDeleteTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := setupWikiTestDB(t)
	for _, stmt := range []string{
		`CREATE TABLE wiki_links (src_page_id TEXT NOT NULL, dst_page_id TEXT NOT NULL, relation TEXT NOT NULL DEFAULT '', weight REAL NOT NULL DEFAULT 0, PRIMARY KEY (src_page_id, dst_page_id))`,
		`CREATE TABLE kb_cards (id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, question TEXT NOT NULL, answer TEXT NOT NULL DEFAULT '', source_type TEXT NOT NULL DEFAULT 'manual', source_ref TEXT NOT NULL DEFAULT '', source_excerpt TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'active', interval_index INTEGER NOT NULL DEFAULT 0, due_at TEXT NOT NULL DEFAULT '', last_reviewed_at TEXT NOT NULL DEFAULT '', review_count INTEGER NOT NULL DEFAULT 0, lapse_count INTEGER NOT NULL DEFAULT 0, created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE kb_card_reviews (id INTEGER PRIMARY KEY AUTOINCREMENT, card_id TEXT NOT NULL, agent_id TEXT NOT NULL, grade TEXT NOT NULL, prev_interval_index INTEGER NOT NULL DEFAULT 0, new_interval_index INTEGER NOT NULL DEFAULT 0, new_due_at TEXT NOT NULL DEFAULT '', reviewed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE kb_card_embeddings (card_id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, embedding BLOB, dim INTEGER NOT NULL DEFAULT 0, model TEXT NOT NULL DEFAULT '', updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	return db
}

// insertCardForPage seeds one wiki-sourced card + one review row + one
// embedding row so the cascade can prove it clears all three tables.
func insertCardForPage(t *testing.T, db *sql.DB, cardID, agent, pageID string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO kb_cards (id, agent_id, question, answer, source_type, source_ref) VALUES (?,?,?,?,'wiki',?)`,
		cardID, agent, "q-"+cardID, "a", pageID); err != nil {
		t.Fatalf("insert card: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO kb_card_reviews (card_id, agent_id, grade) VALUES (?,?,'remembered')`,
		cardID, agent); err != nil {
		t.Fatalf("insert review: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO kb_card_embeddings (card_id, agent_id) VALUES (?,?)`,
		cardID, agent); err != nil {
		t.Fatalf("insert embedding: %v", err)
	}
}

func countCards(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// TestDeletePageCascadesCards: deleting one page removes its cards,
// reviews, and embeddings — but leaves another page's cards untouched.
func TestDeletePageCascadesCards(t *testing.T) {
	db := setupDeleteTestDB(t)
	defer db.Close()
	ctx := context.Background()
	ws := NewWikiStore(db, "sqlite")

	for _, p := range []struct{ id, title string }{
		{"page-a", "Page A"},
		{"page-b", "Page B"},
	} {
		if err := ws.UpsertPage(ctx, &WikiPage{ID: p.id, AgentID: "a1", PageType: "concept", Slug: p.id, Title: p.title, Body: "b", Summary: "s", SourceIDs: []string{"src"}, Tags: []string{}}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	insertCardForPage(t, db, "card-a", "a1", "page-a")
	insertCardForPage(t, db, "card-b", "a1", "page-b")

	if err := ws.DeletePage(ctx, "page-a"); err != nil {
		t.Fatalf("DeletePage: %v", err)
	}
	if n := countCards(t, db, `SELECT COUNT(*) FROM kb_cards WHERE source_ref = 'page-a'`); n != 0 {
		t.Fatalf("cards for page-a survived: %d", n)
	}
	if n := countCards(t, db, `SELECT COUNT(*) FROM kb_card_reviews WHERE card_id = 'card-a'`); n != 0 {
		t.Fatalf("reviews for card-a survived: %d", n)
	}
	if n := countCards(t, db, `SELECT COUNT(*) FROM kb_card_embeddings WHERE card_id = 'card-a'`); n != 0 {
		t.Fatalf("embeddings for card-a survived: %d", n)
	}
	// The untouched page keeps everything, and the page itself is gone.
	if n := countCards(t, db, `SELECT COUNT(*) FROM kb_cards WHERE source_ref = 'page-b'`); n != 1 {
		t.Fatalf("page-b cards should survive: %d", n)
	}
	if p, _ := ws.GetPage(ctx, "page-a"); p != nil {
		t.Fatalf("page-a still present")
	}
}

// TestDeletePagesBySourceCascadesCards: the re-generation cleanup path
// (delete every page fed by source X) also drops the cards those pages
// distilled.
func TestDeletePagesBySourceCascadesCards(t *testing.T) {
	db := setupDeleteTestDB(t)
	defer db.Close()
	ctx := context.Background()
	ws := NewWikiStore(db, "sqlite")

	// src-1 feeds two pages; src-2 feeds one. Deleting src-1's pages must
	// cascade both pages' cards and leave src-2's alone.
	for _, p := range []struct{ id, src string }{
		{"p1", "src-1"},
		{"p2", "src-1"},
		{"p3", "src-2"},
	} {
		if err := ws.UpsertPage(ctx, &WikiPage{ID: p.id, AgentID: "a1", PageType: "source", Slug: p.id, Title: p.id, Body: "b", Summary: "s", SourceIDs: []string{p.src}, Tags: []string{}}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		insertCardForPage(t, db, "card-"+p.id, "a1", p.id)
	}

	n, err := ws.DeletePagesBySource(ctx, "a1", "src-1")
	if err != nil {
		t.Fatalf("DeletePagesBySource: %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted pages = %d, want 2", n)
	}
	if got := countCards(t, db, `SELECT COUNT(*) FROM kb_cards WHERE source_type = 'wiki'`); got != 1 {
		t.Fatalf("wiki cards left = %d, want 1 (only p3's)", got)
	}
	if got := countCards(t, db, `SELECT COUNT(*) FROM kb_card_reviews`); got != 1 {
		t.Fatalf("reviews left = %d, want 1", got)
	}
}
