package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// CompareAndSwapAgentFile closes the read-compare-write window: a save
// must only land when the file still matches what the caller read.
func TestCompareAndSwapAgentFile(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer db.Close()

	if err := db.SaveAgentFile(ctx, "agt_cas", "MEMORY.md", []byte("v1")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Stale expectation → no swap.
	swapped, err := db.CompareAndSwapAgentFile(ctx, "agt_cas", "MEMORY.md", []byte("stale"), []byte("v2"))
	if err != nil {
		t.Fatalf("cas stale: %v", err)
	}
	if swapped {
		t.Fatal("CAS swapped on stale expectation")
	}
	got, _ := db.GetAgentFile(ctx, "agt_cas", "MEMORY.md")
	if string(got) != "v1" {
		t.Fatalf("content changed on failed CAS: %q", got)
	}

	// Matching expectation → swap.
	swapped, err = db.CompareAndSwapAgentFile(ctx, "agt_cas", "MEMORY.md", []byte("v1"), []byte("v2"))
	if err != nil || !swapped {
		t.Fatalf("cas match: swapped=%v err=%v", swapped, err)
	}
	got, _ = db.GetAgentFile(ctx, "agt_cas", "MEMORY.md")
	if string(got) != "v2" {
		t.Fatalf("content after swap: %q", got)
	}

	// Missing file (no prior save) with nil/empty expectation → swap.
	swapped, err = db.CompareAndSwapAgentFile(ctx, "agt_cas", "new.md", nil, []byte("first"))
	if err != nil || !swapped {
		t.Fatalf("cas on missing file: swapped=%v err=%v", swapped, err)
	}
}

// The form timeout must anchor on waiting_since (when the run ENTERED
// the wait), not started_at: a run that started days ago and reached its
// form 5 minutes ago must not time out on arrival. Legacy rows without
// a stamp are left alone.
func TestTimeoutWaitingRunsAnchoredOnWaitingSince(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer db.Close()

	if err := db.migrateWorkflowTables(ctx); err != nil {
		t.Fatalf("workflow migrate: %v", err)
	}

	mk := func(t *testing.T, id string) {
		t.Helper()
		if err := db.CreateWorkflowRun(ctx, id, "wf", 1, map[string]any{}, "", ""); err != nil {
			t.Fatalf("create run: %v", err)
		}
	}
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	

	// arrived-long-ago: waiting_since 48h old → timed out.
	mk(t, "run-old")
	if err := db.SetRunWaiting(ctx, "run-old", "form1", map[string]any{}); err != nil {
		t.Fatalf("set waiting: %v", err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE workflow_runs SET waiting_since = `+db.ph(1)+` WHERE id = `+db.ph(2), old, "run-old"); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// arrived-recently: started long ago but waiting_since 5m old → kept.
	mk(t, "run-recent")
	if err := db.SetRunWaiting(ctx, "run-recent", "form1", map[string]any{}); err != nil {
		t.Fatalf("set waiting: %v", err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE workflow_runs SET started_at = ` + db.ph(1) + ` WHERE id = ` + db.ph(2), old, "run-recent"); err != nil {
		t.Fatalf("backdate started_at: %v", err)
	}

	// legacy: no waiting_since stamp → left alone.
	mk(t, "run-legacy")
	if _, err := db.db.ExecContext(ctx, `UPDATE workflow_runs SET status = ` + db.ph(1) + `, waiting_since = '' WHERE id = ` + db.ph(2), "waiting", "run-legacy"); err != nil {
		t.Fatalf("force waiting: %v", err)
	}

	n, err := db.TimeoutWaitingRuns(ctx, time.Now().Add(-24*time.Hour), "form timeout")
	if err != nil {
		t.Fatalf("timeout sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("sweep flipped %d rows, want exactly 1 (run-old)", n)
	}
	for id, want := range map[string]string{
		"run-old":     "needs_intervention",
		"run-recent":  "waiting",
		"run-legacy":  "waiting",
	} {
		row := db.db.QueryRowContext(ctx, `SELECT status FROM workflow_runs WHERE id = ` + db.ph(1), id)
		var got string
		if err := row.Scan(&got); err != nil {
			t.Fatalf("scan %s: %v", id, err)
		}
		if !strings.EqualFold(got, want) {
			t.Errorf("%s status = %q, want %q", id, got, want)
		}
	}
}
