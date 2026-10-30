package store

import (
	"context"
	"testing"
	"time"
)

// TestMigrateDropFailureSweepCursor covers the cleanup of the abandoned
// scan-cursor row: installs that ran the previous sweep keep a
// kind=sweep / name=failure_sweep_cursor config row that nothing reads any
// more. Migrate drops it — and must not touch any other sweep bookkeeping
// row (the notification dedupe map lives in the same namespace).
func TestMigrateDropFailureSweepCursor(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer db.Close()

	if err := db.SaveConfig(ctx, &ConfigRecord{
		Kind: KindSweep, Name: "failure_sweep_cursor", Enabled: true,
		Data: map[string]any{"ts": time.Now().UTC().Format(time.RFC3339)},
	}); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}
	if err := db.SaveConfig(ctx, &ConfigRecord{
		Kind: KindSweep, Name: "failure_notified", Enabled: true,
		Data: map[string]any{"a|s|gave_up": time.Now().UTC().Format(time.RFC3339)},
	}); err != nil {
		t.Fatalf("seed dedupe: %v", err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cur, err := db.GetConfigByName(ctx, KindSweep, "", "failure_sweep_cursor")
	if err != nil && err != ErrNotFound {
		t.Fatalf("get cursor: %v", err)
	}
	if cur != nil {
		t.Errorf("cursor row survived the migration: %+v", cur)
	}

	kept, err := db.GetConfigByName(ctx, KindSweep, "", "failure_notified")
	if err != nil {
		t.Fatalf("get dedupe row: %v", err)
	}
	if kept == nil || len(kept.Data) == 0 {
		t.Errorf("notification dedupe row was dropped: %+v", kept)
	}

	// Idempotent: a second Migrate on the cleaned DB is a no-op.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
}
