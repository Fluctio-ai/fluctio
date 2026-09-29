package maintenance

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

func newTestDBStore(t *testing.T) *store.DBStore {
	t.Helper()
	t.Setenv("FLUCTIO_HOME", t.TempDir()) // backup.Dir() writes here
	path := filepath.Join(t.TempDir(), "fluctio.db")
	st, err := store.NewDBStore("sqlite", "file:"+path+"?cache=shared")
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

func waitForStage(t *testing.T, c *Coordinator, stage string, within time.Duration) Status {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		s := c.Status()
		if s.Stage == stage {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	return c.Status()
}

// TestFullRun walks the happy path on a real temp sqlite file: idle
// window passes instantly, a backup snapshot exists afterwards, the run
// ends done, and the db file did not grow.
func TestFullRun(t *testing.T) {
	st := newTestDBStore(t)
	ctx := context.Background()
	// Seed some rows so the db has content worth vacuuming.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO session_events (agent_id, session_key, seq, type, data, created_at)
		 VALUES ('agt_x', 's', 0, 'test', '{"a":1}', ?)`, time.Now().UTC()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	c := New(st, nil)
	c.poll = time.Millisecond
	c.vacuumBudget = 30 * time.Second
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := c.Start(); err != ErrAlreadyRunning {
		t.Fatalf("second Start = %v, want ErrAlreadyRunning", err)
	}

	s := waitForStage(t, c, StageDone, 30*time.Second)
	if s.Stage != StageDone {
		t.Fatalf("stage = %s, error = %q", s.Stage, s.Error)
	}
	if s.BackupName == "" {
		t.Fatal("BackupName empty after done")
	}
	if s.SizeAfter == 0 || s.SizeBefore == 0 {
		t.Fatalf("sizes not measured: before=%d after=%d", s.SizeBefore, s.SizeAfter)
	}
	if s.SizeAfter > s.SizeBefore {
		t.Fatalf("db grew: before=%d after=%d", s.SizeBefore, s.SizeAfter)
	}
	if c.Running() {
		t.Fatal("running still true after done")
	}
	// A second run after completion must be accepted again.
	if err := c.Start(); err != nil {
		t.Fatalf("restart after done: %v", err)
	}
	s2 := waitForStage(t, c, StageDone, 30*time.Second)
	if s2.Stage != StageDone {
		t.Fatalf("second run stage = %s, error = %q", s2.Stage, s2.Error)
	}
}

// TestWaitingTimeout verifies the fail-honest path: no idle window
// within the budget → failed, nothing changed, no backup written.
func TestWaitingTimeout(t *testing.T) {
	st := newTestDBStore(t)
	c := New(st, func() bool { return true }) // always busy
	c.waitWindow = 50 * time.Millisecond
	c.poll = 5 * time.Millisecond
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s := waitForStage(t, c, StageFailed, 5*time.Second)
	if s.Stage != StageFailed {
		t.Fatalf("stage = %s, want failed", s.Stage)
	}
	if s.BackupName != "" {
		t.Fatalf("backup written despite timeout: %q", s.BackupName)
	}
	if !strings.Contains(s.Error, "no idle window") {
		t.Fatalf("error = %q, want idle-window timeout", s.Error)
	}
}

// TestDBStats checks the bloat picture degrades gracefully: core
// numbers present, dbstat optional.
func TestDBStats(t *testing.T) {
	st := newTestDBStore(t)
	c := New(st, nil)
	stats, err := c.DBStats(context.Background())
	if err != nil {
		t.Fatalf("DBStats: %v", err)
	}
	if stats.Dialect != "sqlite" {
		t.Fatalf("dialect = %s", stats.Dialect)
	}
	if stats.PageCount == 0 || stats.PageSize == 0 {
		t.Fatalf("page stats missing: %+v", stats)
	}
	if stats.SessionEventsRows != 0 {
		t.Fatalf("events rows = %d, want 0 on a fresh db", stats.SessionEventsRows)
	}
}

func TestDBPath(t *testing.T) {
	cases := map[string]string{
		"file:/data/x.db?_pragma=busy_timeout(5000)": "/data/x.db",
		"/data/x.db":                                 "/data/x.db",
		"file::memory:?cache=shared":                 "",
		":memory:":                                   "",
	}
	for in, want := range cases {
		if got := dbPath(in); got != want {
			t.Errorf("dbPath(%q) = %q, want %q", in, got, want)
		}
	}
}
