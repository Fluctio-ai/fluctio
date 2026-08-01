package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestMigrateKBWikiAddsContentTypeColumns simulates an older database whose
// kb_sources predates the content-type columns, runs migrateKBWiki twice, and
// verifies (a) the five new columns appear, (b) an existing pre-migration row
// defaults to type='article', and (c) the migration is idempotent. This is the
// smoke check for the stage-1 schema change.
func TestMigrateKBWikiAddsContentTypeColumns(t *testing.T) {
	dir := t.TempDir()
	d, err := NewDBStore("sqlite", filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	ctx := context.Background()

	// Pre-create kb_sources in its OLD shape (no type/status/time columns) and
	// seed a row, the way a real upgraded install would look before this change.
	oldCreate := `CREATE TABLE kb_sources (
		id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, title TEXT NOT NULL,
		source_type TEXT NOT NULL, source_ref TEXT NOT NULL,
		entry_count INTEGER NOT NULL DEFAULT 0, total_chars INTEGER NOT NULL DEFAULT 0,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, wiki_generated_at TIMESTAMP)`
	if _, err := d.db.ExecContext(ctx, oldCreate); err != nil {
		t.Fatalf("old create: %v", err)
	}
	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO kb_sources (id, agent_id, title, source_type, source_ref) VALUES ('s1','a1','old article','text','manual')`); err != nil {
		t.Fatalf("seed old row: %v", err)
	}

	for pass := 1; pass <= 2; pass++ {
		if err := d.migrateKBWiki(ctx); err != nil {
			t.Fatalf("migrateKBWiki pass %d: %v", pass, err)
		}
	}

	for _, col := range []string{"type", "status", "start_at", "end_at", "reminded_at"} {
		has, err := d.tableHasColumn(ctx, "kb_sources", col)
		if err != nil {
			t.Fatalf("check column %s: %v", col, err)
		}
		if !has {
			t.Errorf("column %s missing after migrate", col)
		}
	}

	var typ, status string
	if err := d.db.QueryRowContext(ctx, `SELECT type, status FROM kb_sources WHERE id='s1'`).Scan(&typ, &status); err != nil {
		t.Fatalf("query old row: %v", err)
	}
	if typ != "article" {
		t.Errorf("old row type = %q, want article", typ)
	}
	if status != "" {
		t.Errorf("old row status = %q, want empty", status)
	}
}
