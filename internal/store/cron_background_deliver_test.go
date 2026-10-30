package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestMigrateCronJobsBackgroundDeliver simulates a legacy install whose
// cron_jobs table still carries the silent bool, runs the full Migrate
// sequence, and verifies the orthogonal-axes mapping:
//
//	silent=1 → (background=1, deliver=0)   — old background job stays mute
//	silent=0 → (background=0, deliver=1)   — old foreground job still reports
//
// and that the silent column itself is gone (single-user product, no
// external schema consumers — dropped for good per the design call).
func TestMigrateCronJobsBackgroundDeliver(t *testing.T) {
	ctx := context.Background()
	db, err := NewDBStore("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	// Pre-migration legacy schema (the shape migrateCronJobsAddSilent-era
	// installs carry), with one row per silent value.
	for _, s := range []string{
		`CREATE TABLE cron_jobs (
			id TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			type TEXT NOT NULL DEFAULT 'cron',
			schedule TEXT NOT NULL,
			message TEXT NOT NULL,
			channel TEXT NOT NULL,
			chat_id TEXT NOT NULL,
			account_id TEXT NOT NULL DEFAULT '',
			timezone TEXT NOT NULL DEFAULT 'UTC',
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			last_run TIMESTAMP,
			next_run TIMESTAMP,
			locked_by TEXT,
			locked_at TIMESTAMP,
			failure_count INTEGER NOT NULL DEFAULT 0,
			silent BOOLEAN NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO cron_jobs (id, agent_id, name, schedule, message, channel, chat_id, silent)
			VALUES ('fg', 'a1', 'foreground', '0 9 * * *', 'hi', 'wechat', 'wx-chat', 0)`,
		`INSERT INTO cron_jobs (id, agent_id, name, schedule, message, channel, chat_id, silent)
			VALUES ('bg', 'a1', 'background', '0 7 * * *', 'tidy', '', '', 1)`,
	} {
		if _, err := db.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed legacy schema: %v\nSQL: %s", err, s)
		}
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The silent column must be gone.
	if has, err := db.tableHasColumn(ctx, "cron_jobs", "silent"); err != nil || has {
		t.Fatalf("silent column still present (has=%v err=%v) — migration must drop it", has, err)
	}

	jobs, err := db.ListCronJobsByAgent(ctx, "a1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("want 2 jobs, got %d", len(jobs))
	}
	for _, j := range jobs {
		switch j.ID {
		case "fg":
			if j.Background || !j.Deliver {
				t.Errorf("fg (silent=0): background=%v deliver=%v, want false/true", j.Background, j.Deliver)
			}
		case "bg":
			if !j.Background || j.Deliver {
				t.Errorf("bg (silent=1): background=%v deliver=%v, want true/false", j.Background, j.Deliver)
			}
		}
	}

	// Idempotent: a second Migrate on the already-migrated table is a no-op.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
}
