package store

import (
	"context"
	"testing"
	"time"
)

// TestPruneSessionEventsDeletesOlderRows verifies the retention prune removes
// only rows older than the cutoff, leaving recent events intact. Uses a small
// batch size to exercise the multi-batch DELETE loop.
func TestPruneSessionEventsDeletesOlderRows(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Seed three events for one session; all get created_at = now via
	// DEFAULT CURRENT_TIMESTAMP.
	for _, typ := range []string{"content", "tool_call", "done"} {
		if _, err := db.AppendSessionEvent(ctx, "a", "s-1", typ, []byte("{}")); err != nil {
			t.Fatalf("AppendSessionEvent(%s): %v", typ, err)
		}
	}

	// Backdate two of them into the "old" window. Format matches SQLite's
	// CURRENT_TIMESTAMP ("YYYY-MM-DD HH:MM:SS" UTC) so the comparison agrees
	// with how the rows were written.
	oldStr := time.Now().Add(-48 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	if _, err := db.db.ExecContext(ctx,
		`UPDATE session_events SET created_at = ? WHERE type IN ('content','tool_call')`, oldStr); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Prune everything older than one hour ago, in batches of 2 (two old
	// rows + a third empty pass that terminates the loop).
	n, err := db.PruneSessionEvents(ctx, time.Now().Add(-time.Hour), 2)
	if err != nil {
		t.Fatalf("PruneSessionEvents: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted = %d, want 2 (the two backdated rows)", n)
	}

	// Only the recent 'done' event survives.
	rows, err := db.ListSessionEventsSince(ctx, "a", "s-1", -1)
	if err != nil {
		t.Fatalf("ListSessionEventsSince: %v", err)
	}
	if len(rows) != 1 || rows[0].Type != "done" {
		t.Errorf("surviving rows = %+v, want one 'done' event", rows)
	}
}

// TestPruneSessionEventsNoOpWhenNothingOlder confirms a cutoff behind all rows
// deletes nothing — the sweep's steady state on an idle instance.
func TestPruneSessionEventsNoOpWhenNothingOlder(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	if _, err := db.AppendSessionEvent(ctx, "a", "s-1", "done", []byte("{}")); err != nil {
		t.Fatalf("AppendSessionEvent: %v", err)
	}

	// Cutoff one hour in the past — the just-written row is newer, so it lives.
	n, err := db.PruneSessionEvents(ctx, time.Now().Add(-time.Hour), 1000)
	if err != nil {
		t.Fatalf("PruneSessionEvents: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted = %d, want 0 (row is newer than cutoff)", n)
	}
	rows, _ := db.ListSessionEventsSince(ctx, "a", "s-1", -1)
	if len(rows) != 1 {
		t.Errorf("row was deleted unexpectedly; remaining = %d", len(rows))
	}
}

// TestPruneSessionEventsIndexExists verifies the retention index was created
// during migration — without it the time-based DELETE scans the whole table.
func TestPruneSessionEventsIndexExists(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	rows, err := db.db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='session_events'`)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, n)
	}
	for _, n := range names {
		if n == "idx_session_events_created" {
			return // found
		}
	}
	t.Errorf("idx_session_events_created missing; indexes = %v", names)
}
