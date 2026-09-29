// Package maintenance implements the online SQLite maintenance
// coordinator: wait for an idle window → snapshot backup → VACUUM →
// report. Modeled on Tencent Octop's memory-slim coordinator: honest
// staged status, no fake progress percentages, backup before rewrite,
// fail-closed on any step, and state lives in memory only — a restart
// mid-run leaves the database untouched-or-backed-up and the status
// back at idle, which is the honest answer.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fluctio-ai/fluctio/internal/backup"
	"github.com/fluctio-ai/fluctio/internal/store"
)

var (
	ErrNotBound       = errors.New("maintenance: store not bound")
	ErrAlreadyRunning = errors.New("maintenance: a run is already in progress")
	ErrSQLiteOnly     = errors.New("maintenance: online vacuum is SQLite-only; PostgreSQL manages its own bloat")
)

// Stages are deliberately linear and few — callers poll, they don't
// subscribe. "vacuum_skipped" does not exist: a dialect that cannot be
// vacuumed fails at Start instead of reporting fake success.
const (
	StageIdle    = "idle"
	StageWaiting = "waiting"
	StageBackup  = "backup"
	StageVacuum  = "vacuum"
	StageDone    = "done"
	StageFailed  = "failed"
)

// Status is the full, honest state of the coordinator. Elapsed is wall
// time since the run started — it grows while waiting for the idle
// window and is NOT a progress indicator, because none can be given
// honestly for VACUUM.
type Status struct {
	Stage          string  `json:"stage"`
	StartedAt      int64   `json:"startedAt,omitempty"` // unix seconds
	UpdatedAt      int64   `json:"updatedAt,omitempty"`
	ElapsedSeconds float64 `json:"elapsedSeconds,omitempty"`
	BackupName     string  `json:"backupName,omitempty"`
	SizeBefore     int64   `json:"sizeBefore,omitempty"` // db + wal bytes, measured after the idle window
	SizeAfter      int64  `json:"sizeAfter,omitempty"`
	Error          string  `json:"error,omitempty"`
}

// Coordinator serializes maintenance runs for the whole process (the
// database is process-global, so per-agent runs would fight over the
// same file).
type Coordinator struct {
	mu  sync.Mutex
	db  *store.DBStore
	st  store.Store // for backup.Create
	// busy is the advisory in-flight-turn probe (true = something is
	// running, e.g. session.AnyTurnActive); nil means "assume idle"
	// (single-user dev, tests).
	busy func() bool
	cur  Status

	waitWindow   time.Duration // idle-window budget; 0 = default 2m
	poll         time.Duration // busy-probe interval; 0 = default 2s
	vacuumBudget time.Duration // VACUUM ctx timeout; 0 = default 10m
}

// New builds a Coordinator over st. busy is the advisory activity
// probe (true = an agent turn is in-flight, maintenance must wait);
// nil means "assume idle". A non-DBStore store still binds (status/
// DBStats work) but Start returns ErrNotBound.
func New(st store.Store, busy func() bool) *Coordinator {
	c := &Coordinator{st: st, busy: busy, cur: Status{Stage: StageIdle}}
	if dbs, ok := st.(*store.DBStore); ok && dbs != nil {
		c.db = dbs
	}
	return c
}

// Start launches one maintenance run in the background and returns
// immediately. A second call while running fails with
// ErrAlreadyRunning; nothing about the database is touched before the
// idle window is acquired, so a rejected or failed Start is a no-op.
func (c *Coordinator) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runningLocked() {
		return ErrAlreadyRunning
	}
	if c.st == nil {
		return ErrNotBound
	}
	if c.db == nil {
		return ErrNotBound
	}
	if c.db.Dialect() != "sqlite" {
		return ErrSQLiteOnly
	}
	c.cur = Status{Stage: StageWaiting, StartedAt: time.Now().Unix()}
	c.touchLocked()
	go c.run()
	return nil
}

func (c *Coordinator) run() {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("maintenance panic", "error", r)
			c.fail(fmt.Sprintf("panic: %v", r))
		}
	}()

	// 1. Idle window. Never interrupts an in-flight turn; gives up
	// honestly instead of forcing the migration through.
	waitWindow := orDefault(c.waitWindow, 2*time.Minute)
	poll := orDefault(c.poll, 2*time.Second)
	deadline := time.Now().Add(waitWindow)
	for c.busy != nil && c.busy() {
		if time.Now().After(deadline) {
			c.fail(fmt.Sprintf("no idle window within %s (a turn stayed active); nothing was changed", waitWindow))
			return
		}
		time.Sleep(poll)
	}

	// 2. Backup first, always. Reuses the daily-snapshot machinery, so
	// the pre-maintenance snapshot follows the same rotation/retention
	// as scheduled backups and TodayHasBackup().
	src := c.db.Source()
	sizeBefore := totalSize(src)
	c.set(func(s *Status) {
		s.Stage = StageBackup
		s.SizeBefore = sizeBefore
	})
	ctx := context.Background()
	name, _, err := backup.Create(ctx, c.st, time.Now())
	if err != nil {
		c.fail("backup failed, vacuum not attempted: " + err.Error())
		return
	}
	c.set(func(s *Status) { s.BackupName = name })

	// 3. Vacuum. Runs on the process's single SQLite connection, so
	// concurrent DB access queues behind it rather than erroring — the
	// idle window exists to keep that queue short, not for correctness.
	c.set(func(s *Status) { s.Stage = StageVacuum })
	vctx, cancel := context.WithTimeout(ctx, orDefault(c.vacuumBudget, 10*time.Minute))
	defer cancel()
	if _, err := c.db.DB().ExecContext(vctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		c.fail("wal checkpoint failed: " + err.Error())
		return
	}
	if _, err := c.db.DB().ExecContext(vctx, "VACUUM"); err != nil {
		c.fail("vacuum failed (database unchanged, backup intact): " + err.Error())
		return
	}
	// Best-effort WAL truncate after vacuum; failure leaves only a
	// slightly larger -wal file, not a maintenance failure.
	if _, err := c.db.DB().ExecContext(vctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		slog.Warn("maintenance: post-vacuum wal checkpoint", "error", err)
	}

	sizeAfter := totalSize(src)
	c.set(func(s *Status) {
		s.Stage = StageDone
		s.SizeAfter = sizeAfter
	})
	slog.Info("maintenance done",
		"backup", name, "before", sizeBefore, "after", sizeAfter)
}

// Status returns a snapshot of the current (or last) run.
func (c *Coordinator) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.cur
	if s.StartedAt > 0 {
		s.ElapsedSeconds = time.Since(time.Unix(s.StartedAt, 0)).Seconds()
	}
	return s
}

// IdleNow reports whether the busy probe currently reads quiet (true
// when no probe is wired). Used by the status response to explain the
// waiting stage.
func (c *Coordinator) IdleNow() bool { return c.busy == nil || !c.busy() }

// Running reports whether a run is in flight (derived from the stage —
// there is deliberately no separate flag to fall out of sync).
func (c *Coordinator) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runningLocked()
}

// TableSize is one row of the dbstat top-tables breakdown.
type TableSize struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// DBStats is the read-only bloat picture surfaced by the status
// endpoint. Everything here is cheap to compute except dbstat, which is
// omitted silently on builds/DBs that lack the virtual table.
type DBStats struct {
	Dialect            string      `json:"dialect"`
	DBBytes            int64       `json:"dbBytes"`
	WALBytes           int64       `json:"walBytes,omitempty"`
	PageCount          int64       `json:"pageCount,omitempty"`
	FreePages          int64       `json:"freePages,omitempty"`
	PageSize           int64       `json:"pageSize,omitempty"`
	FreeRatio          float64     `json:"freeRatio,omitempty"` // free pages / total pages
	SessionEventsRows  int64       `json:"sessionEventsRows"`
	SessionEventsBytes int64       `json:"sessionEventsBytes"`
	TopTables          []TableSize `json:"topTables,omitempty"`
}

// DBStats gathers the current bloat picture. Fails only on the core
// queries; optional parts (dbstat) degrade to omitted.
func (c *Coordinator) DBStats(ctx context.Context) (DBStats, error) {
	var out DBStats
	if c.db == nil {
		return out, ErrNotBound
	}
	out.Dialect = c.db.Dialect()
	p := dbPath(c.db.Source())
	out.DBBytes, out.WALBytes = fileSize(p), fileSize(p+"-wal")

	conn := c.db.DB()
	if out.Dialect == "sqlite" {
		var pageCount, freePages, pageSize int64
		if err := conn.QueryRowContext(ctx,
			"SELECT * FROM pragma_page_count, pragma_freelist_count, pragma_page_size").
			Scan(&pageCount, &freePages, &pageSize); err == nil {
			out.PageCount, out.FreePages, out.PageSize = pageCount, freePages, pageSize
			if pageCount > 0 {
				out.FreeRatio = float64(freePages) / float64(pageCount)
			}
		}
		if rows, err := conn.QueryContext(ctx,
			"SELECT name, SUM(pgsize) AS sz FROM dbstat GROUP BY name ORDER BY sz DESC LIMIT 10"); err == nil {
			for rows.Next() {
				var t TableSize
				if err := rows.Scan(&t.Name, &t.Bytes); err == nil {
					out.TopTables = append(out.TopTables, t)
				}
			}
			rows.Close()
		}
	}
	if err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*), COALESCE(SUM(LENGTH(data)),0) FROM session_events").
		Scan(&out.SessionEventsRows, &out.SessionEventsBytes); err != nil {
		// Not fatal: an install that never wrote events still gets stats.
		slog.Warn("maintenance: session_events stats", "error", err)
	}
	return out, nil
}

// --- internals ---

func (c *Coordinator) fail(msg string) {
	c.set(func(s *Status) {
		s.Stage = StageFailed
		s.Error = msg
	})
	slog.Warn("maintenance failed", "error", msg)
}

func (c *Coordinator) set(f func(*Status)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(&c.cur)
	c.touchLocked()
}

func (c *Coordinator) touchLocked() {
	c.cur.UpdatedAt = time.Now().Unix()
}

// runningLocked reports whether a run is in flight; caller holds c.mu.
// Derived from the stage so there is no separate flag to fall out of
// sync.
func (c *Coordinator) runningLocked() bool {
	switch c.cur.Stage {
	case StageWaiting, StageBackup, StageVacuum:
		return true
	}
	return false
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// dbPath extracts the on-disk path from a SQLite DSN ("file:/x/y.db?
// _pragma=..." or a bare path). Returns "" for DSNs it cannot localize
// (":memory:", "file::memory:?cache=shared"), where sizes report 0.
func dbPath(source string) string {
	p := strings.TrimPrefix(source, "file:")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if p == "" || p == ":memory:" || strings.HasPrefix(p, ":memory:") {
		return ""
	}
	return p
}

func fileSize(p string) int64 {
	if p == "" {
		return 0
	}
	info, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return info.Size()
}

// totalSize reports db + wal bytes for a SQLite DSN source — the number
// SizeBefore/SizeAfter report for a maintenance run.
func totalSize(src string) int64 {
	p := dbPath(src)
	return fileSize(p) + fileSize(p+"-wal")
}
