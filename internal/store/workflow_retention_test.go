package store

import (
	"context"
	"testing"
	"time"
)

// seedRun inserts a finalized run with an optional backdated finished_at, so a
// test can place it inside or outside a retention window without waiting.
func seedRun(t *testing.T, db *DBStore, id, status string, finishedOffset time.Duration) {
	t.Helper()
	ctx := context.Background()
	if err := db.CreateWorkflowRun(ctx, id, "def-1", 1, nil, "", "owner-1"); err != nil {
		t.Fatalf("CreateWorkflowRun(%s): %v", id, err)
	}
	if err := db.FinalizeWorkflowRun(ctx, id, status, ""); err != nil {
		t.Fatalf("FinalizeWorkflowRun(%s): %v", id, err)
	}
	if finishedOffset != 0 {
		old := time.Now().Add(finishedOffset).UTC().Format(time.RFC3339)
		if _, err := db.db.ExecContext(ctx, `UPDATE workflow_runs SET finished_at = ? WHERE id = ?`, old, id); err != nil {
			t.Fatalf("backdate(%s): %v", id, err)
		}
	}
}

// AC1 — a succeeded run is kept inside the success window, deleted past it,
// and its node outputs are cascaded.
func TestPruneWorkflowRunsSuccessWindow(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	seedRun(t, db, "recent-ok", "succeeded", -time.Hour)   // 1h ago — within 7d
	seedRun(t, db, "old-ok", "succeeded", -8*24*time.Hour) // 8d ago — past 7d
	if err := db.AppendWorkflowNodeOutput(ctx, "old-ok", "n1", 1, "succeeded", map[string]any{"v": 1}, ""); err != nil {
		t.Fatalf("AppendWorkflowNodeOutput: %v", err)
	}

	n, err := db.PruneWorkflowRuns(ctx, time.Now().Add(-7*24*time.Hour), time.Time{}, 100)
	if err != nil {
		t.Fatalf("PruneWorkflowRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted = %d, want 1 (old-ok)", n)
	}
	if _, _, err := db.GetWorkflowRun(ctx, "recent-ok"); err != nil {
		t.Errorf("recent-ok pruned unexpectedly: %v", err)
	}
	if _, _, err := db.GetWorkflowRun(ctx, "old-ok"); err != ErrNotFound {
		t.Errorf("old-ok want ErrNotFound, got %v", err)
	}
	if rows, _ := db.ListWorkflowNodeOutputs(ctx, "old-ok"); len(rows) != 0 {
		t.Errorf("old-ok node outputs not cascaded: %d rows left", len(rows))
	}
}

// AC2 — a failed run survives the (shorter) success window and is only removed
// past the (longer) failed window. needs_intervention counts as failure.
func TestPruneWorkflowRunsFailedWindow(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	seedRun(t, db, "failed-10d", "failed", -10*24*time.Hour)             // past success(7d), within failed(30d)
	seedRun(t, db, "failed-40d", "failed", -40*24*time.Hour)             // past failed(30d)
	seedRun(t, db, "needs-int-40d", "needs_intervention", -40*24*time.Hour)

	// success-only sweep (failed disabled) → nothing deleted, the failed runs
	// outlive the success window.
	if n, err := db.PruneWorkflowRuns(ctx, time.Now().Add(-7*24*time.Hour), time.Time{}, 100); err != nil || n != 0 {
		t.Fatalf("success-only sweep deleted %d (want 0): %v", n, err)
	}
	// failed sweep at 30d → the two 40d failures go, the 10d stays.
	n, err := db.PruneWorkflowRuns(ctx, time.Time{}, time.Now().Add(-30*24*time.Hour), 100)
	if err != nil {
		t.Fatalf("PruneWorkflowRuns: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted = %d, want 2 (failed-40d, needs-int-40d)", n)
	}
	if _, _, err := db.GetWorkflowRun(ctx, "failed-10d"); err != nil {
		t.Errorf("failed-10d pruned unexpectedly: %v", err)
	}
}

// AC1/2 edge — a 'running' run (finished_at IS NULL) is never pruned, even
// when both cutoffs are in the past.
func TestPruneWorkflowRunsRunningNotPruned(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	if err := db.CreateWorkflowRun(ctx, "live", "def-1", 1, nil, "", ""); err != nil {
		t.Fatalf("CreateWorkflowRun: %v", err)
	}
	// Not finalized → finished_at stays NULL.

	n, err := db.PruneWorkflowRuns(ctx, time.Now().Add(-time.Minute), time.Now().Add(-time.Minute), 100)
	if err != nil {
		t.Fatalf("PruneWorkflowRuns: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted = %d, want 0 (running run must not be pruned)", n)
	}
	if _, _, err := db.GetWorkflowRun(ctx, "live"); err != nil {
		t.Errorf("running run pruned unexpectedly: %v", err)
	}
}

// AC4 — DeleteWorkflowRun removes the run + its node outputs, leaving other
// runs untouched.
func TestDeleteWorkflowRun(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	seedRun(t, db, "r1", "succeeded", -time.Hour)
	if err := db.AppendWorkflowNodeOutput(ctx, "r1", "n1", 1, "succeeded", map[string]any{}, ""); err != nil {
		t.Fatalf("AppendWorkflowNodeOutput: %v", err)
	}
	seedRun(t, db, "r2", "succeeded", -time.Hour) // survivor

	if err := db.DeleteWorkflowRun(ctx, "r1"); err != nil {
		t.Fatalf("DeleteWorkflowRun: %v", err)
	}
	if _, _, err := db.GetWorkflowRun(ctx, "r1"); err != ErrNotFound {
		t.Errorf("r1 want ErrNotFound, got %v", err)
	}
	if rows, _ := db.ListWorkflowNodeOutputs(ctx, "r1"); len(rows) != 0 {
		t.Errorf("r1 node outputs not cascaded: %d rows left", len(rows))
	}
	if _, _, err := db.GetWorkflowRun(ctx, "r2"); err != nil {
		t.Errorf("r2 should survive: %v", err)
	}
}

// AC5 — DeleteWorkflowRunsBy deletes runs matching a status filter, and an
// empty filter is a no-op (won't wipe the table).
func TestDeleteWorkflowRunsBy(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	seedRun(t, db, "ok-1", "succeeded", -time.Hour)
	seedRun(t, db, "ok-2", "succeeded", -time.Hour)
	seedRun(t, db, "fail-1", "failed", -time.Hour)

	// delete all failed runs.
	n, err := db.DeleteWorkflowRunsBy(ctx, "failed", time.Time{}, "", 100)
	if err != nil {
		t.Fatalf("DeleteWorkflowRunsBy: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1 (fail-1)", n)
	}
	if _, _, err := db.GetWorkflowRun(ctx, "ok-1"); err != nil {
		t.Errorf("ok-1 pruned unexpectedly: %v", err)
	}

	// empty filter is a no-op.
	n, err = db.DeleteWorkflowRunsBy(ctx, "", time.Time{}, "", 100)
	if err != nil || n != 0 {
		t.Errorf("empty filter: deleted=%d err=%v, want 0/nil", n, err)
	}
}

// AC3 — both cutoffs zero means the sweep is a no-op (both states disabled),
// so nothing is deleted even for obviously-old runs.
func TestPruneWorkflowRunsBothDisabled(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	seedRun(t, db, "old-ok", "succeeded", -8*24*time.Hour)
	seedRun(t, db, "old-fail", "failed", -40*24*time.Hour)

	n, err := db.PruneWorkflowRuns(ctx, time.Time{}, time.Time{}, 100)
	if err != nil {
		t.Fatalf("PruneWorkflowRuns: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted = %d, want 0 (both cutoffs zero = disabled)", n)
	}
	if _, _, err := db.GetWorkflowRun(ctx, "old-ok"); err != nil {
		t.Errorf("old-ok pruned while disabled: %v", err)
	}
}
