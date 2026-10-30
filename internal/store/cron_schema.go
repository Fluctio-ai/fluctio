package store

import (
	"fmt"
	"strings"
)

// cronJobColumnDefs is the single source of truth for the cron_jobs
// schema: the CREATE TABLE statements, the SELECT list, the INSERT list
// and the scan order all derive from it. Adding a column is one edit here —
// the six hand-copied lists this replaced had already drifted once (a
// table rebuild for the user_id drop silently reverted a newer additive
// migration).
//
// Order matters twice: the read list must match scanCronJobs' Scan
// arguments, and the write list must match saveCronJob's argument order.
var cronJobColumnDefs = []struct{ Name, Decl string }{
	{"id", "TEXT PRIMARY KEY"},
	{"agent_id", "TEXT NOT NULL"},
	{"name", "TEXT NOT NULL DEFAULT ''"},
	{"type", "TEXT NOT NULL DEFAULT 'cron'"},
	{"schedule", "TEXT NOT NULL"},
	{"message", "TEXT NOT NULL"},
	{"channel", "TEXT NOT NULL"},
	{"chat_id", "TEXT NOT NULL"},
	{"account_id", "TEXT NOT NULL DEFAULT ''"},
	{"timezone", "TEXT NOT NULL DEFAULT 'UTC'"},
	{"enabled", "BOOLEAN NOT NULL DEFAULT TRUE"},
	{"last_run", "TIMESTAMP"},
	{"next_run", "TIMESTAMP"},
	// failure_count tracks consecutive fire-attempts whose destination
	// channel was missing/unreachable; the scheduler self-deletes a job
	// once it crosses the threshold.
	{"failure_count", "INTEGER NOT NULL DEFAULT 0"},
	// background: run in the job's own cron_<id> session instead of the
	// bound conversation. deliver: push the reply to the bound chat even
	// when background. Orthogonal axes — the old silent bool bound them
	// together, making "background task that still reports back"
	// inexpressible.
	{"background", "BOOLEAN NOT NULL DEFAULT 0"},
	{"deliver", "BOOLEAN NOT NULL DEFAULT 1"},
	{"created_at", "TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP"},
	// Lock columns are written by the scheduler's lock queries only, never
	// by the CRUD path.
	{"locked_by", "TEXT"},
	{"locked_at", "TIMESTAMP"},
}

// cronJobAllCols is every column in definition order — used by the table
// rebuild that copies rows into a fresh table.
var cronJobAllCols = cronJobColumnsExcept()

// cronJobReadCols is the SELECT / Scan order: every column except the lock
// pair.
var cronJobReadCols = cronJobColumnsExcept("locked_by", "locked_at")

// cronJobWriteCols is the CRUD insert set: read columns minus
// failure_count, which saveCronJob deliberately leaves alone so saving a
// job never resets the scheduler's failure streak.
var cronJobWriteCols = cronJobColumnsExcept("locked_by", "locked_at", "failure_count")

func cronJobColumnsExcept(exclude ...string) []string {
	skip := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		skip[e] = true
	}
	out := make([]string, 0, len(cronJobColumnDefs))
	for _, c := range cronJobColumnDefs {
		if !skip[c.Name] {
			out = append(out, c.Name)
		}
	}
	return out
}

// cronJobCreateSQL renders the table definition for the given CREATE
// prefix ("CREATE TABLE IF NOT EXISTS " / "CREATE TABLE ").
func cronJobCreateSQL(prefix, table string) string {
	var b strings.Builder
	b.WriteString(prefix + table + " (")
	for i, c := range cronJobColumnDefs {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n\t\t\t" + c.Name + " " + c.Decl)
	}
	b.WriteString("\n\t\t)")
	return b.String()
}

// cronJobInsertSQL renders the upsert used by saveCronJob. Postgres uses
// numbered placeholders; SQLite uses ?. The ON CONFLICT SET list updates
// every write column except the id, and never touches failure_count.
func cronJobInsertSQL(dialect string) string {
	cols := cronJobWriteCols
	marks := make([]string, len(cols))
	for i := range cols {
		if dialect == "postgres" {
			marks[i] = fmt.Sprintf("$%d", i+1)
		} else {
			marks[i] = "?"
		}
	}
	sets := make([]string, 0, len(cols))
	for i, c := range cols {
		if c == "id" {
			continue
		}
		if dialect == "postgres" {
			sets = append(sets, fmt.Sprintf("%s=$%d", c, i+1))
		} else {
			sets = append(sets, fmt.Sprintf("%s=excluded.%s", c, c))
		}
	}
	return "INSERT INTO cron_jobs (" + strings.Join(cols, ", ") + ")\n\t\t\tVALUES (" +
		strings.Join(marks, ", ") + ")\n\t\t\tON CONFLICT (id) DO UPDATE SET " +
		strings.Join(sets, ", ")
}
