package store

import (
	"context"
	"testing"
)

// TestMigrateKBEntryOffsets verifies the migration adds the offset columns
// to kb_entries (fresh and existing DBs share the idempotent path via
// tableHasColumn).
func TestMigrateKBEntryOffsets(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	for _, col := range []string{"start_offset", "end_offset"} {
		has, err := db.tableHasColumn(ctx, "kb_entries", col)
		if err != nil {
			t.Fatalf("check %s: %v", col, err)
		}
		if !has {
			t.Errorf("kb_entries missing column %s after migrate", col)
		}
	}
	// Second Migrate pass must stay idempotent (the ALTER is gated).
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}
