// Package backup creates and rotates self-contained snapshots of the
// Fluctio SQLite database via VACUUM INTO. Used by the gateway's
// scheduled backup ticker (auto) and the setup HTTP handlers (manual).
package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/diary"
	"github.com/fluctio-ai/fluctio/internal/store"
)

const (
	filePrefix = "fluctio-"
)

// validSuffixes — one backup extension per dialect: .db for SQLite
// (VACUUM INTO writes a self-contained file), .dump for Postgres
// (pg_dump -Fc compressed custom format). ValidName admits both so
// List/Rotate/Remove keep working across a dialect migration or in a
// dir that happens to hold a mix.
var validSuffixes = []string{".db", ".dump"}

// suffixForDialect maps a store dialect to its backup file extension.
func suffixForDialect(dialect string) (string, error) {
	switch dialect {
	case "sqlite":
		return ".db", nil
	case "postgres":
		return ".dump", nil
	default:
		return "", fmt.Errorf("backup: unsupported dialect %q", dialect)
	}
}

// ValidName reports whether name is a safe backup filename: no path
// separators and matching the fluctio-*.db pattern. Guards the HTTP
// download/delete handlers against path traversal.
func ValidName(name string) bool {
	if name == "" || filepath.Base(name) != name {
		return false
	}
	if !strings.HasPrefix(name, filePrefix) {
		return false
	}
	for _, suf := range validSuffixes {
		if strings.HasSuffix(name, suf) {
			return len(name) > len(filePrefix)+len(suf)
		}
	}
	return false
}

// Dir returns <home>/backups, creating it on first call.
func Dir() (string, error) {
	home, err := config.HomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// Info describes one backup file.
type Info struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"` // unix seconds
}

// Create snapshots the database at <dir>/fluctio-<ts>.<ext>. SQLite uses
// VACUUM INTO (.db) — the snapshot is self-contained, committed WAL
// changes folded in, so it is consistent even while the gateway holds
// fluctio.db open. Postgres uses pg_dump -Fc (.dump) — a compressed
// custom-format dump restorable via pg_restore. Returns the filename
// (not full path) and size bytes.
func Create(ctx context.Context, st store.Store, ts time.Time) (string, int64, error) {
	dbs, ok := st.(*store.DBStore)
	if !ok || dbs == nil {
		return "", 0, fmt.Errorf("backup: store is not DBStore")
	}
	suffix, err := suffixForDialect(dbs.Dialect())
	if err != nil {
		return "", 0, err
	}
	dir, err := Dir()
	if err != nil {
		return "", 0, err
	}
	base := ts.Format("20060102-150405")
	name := filePrefix + base + suffix
	path := filepath.Join(dir, name)
	// Neither VACUUM INTO nor pg_dump should clobber an existing file; bump
	// the name with a counter if a same-second backup already exists
	// (rapid double-click on "back up now").
	for i := 1; ; i++ {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		} else if err != nil {
			return "", 0, err
		}
		name = fmt.Sprintf("%s%s-%d%s", filePrefix, base, i, suffix)
		path = filepath.Join(dir, name)
	}
	switch dbs.Dialect() {
	case "sqlite":
		quoted := "'" + strings.ReplaceAll(path, "'", "''") + "'"
		if _, err := dbs.DB().ExecContext(ctx, "VACUUM INTO "+quoted); err != nil {
			return "", 0, fmt.Errorf("VACUUM INTO: %w", err)
		}
	case "postgres":
		if err := dumpPostgres(ctx, dbs.Source(), path); err != nil {
			return "", 0, err
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	return name, info.Size(), nil
}

// dumpPostgres runs `pg_dump -Fc` to produce a compressed custom-format
// dump at path. dsn is the store's connection string — pg_dump accepts
// both libpq conninfo ("host=… dbname=…") and URL ("postgres://…")
// forms, so whatever shape the gateway opened, the backup mirrors. The
// DSN may carry a password; as a positional arg it is briefly visible in
// the process list (same as any CLI pg_dump) — acceptable in a single-
// tenant container; harden with ~/.pgpass + a passwordless DSN if the
// deploy model differs. Requires the pg_dump binary on PATH
// (postgresql-client in the deploy image); a missing binary fails fast
// with a clear error rather than silently skipping a scheduled backup.
func dumpPostgres(ctx context.Context, dsn, path string) error {
	cmd := exec.CommandContext(ctx, "pg_dump", "--format=custom", "--file="+path, dsn)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pg_dump: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// List returns all backup files, newest first. Filenames carry the
// timestamp so lexical order == time order.
func List() ([]Info, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !ValidName(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Info{
			Name:     e.Name(),
			Size:     fi.Size(),
			Modified: fi.ModTime().Unix(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// Rotate keeps only the newest maxKeep backups, removing the rest.
// maxKeep < 1 is a no-op (keep everything).
func Rotate(maxKeep int) {
	if maxKeep < 1 {
		return
	}
	items, err := List()
	if err != nil {
		return
	}
	dir, _ := Dir()
	for i := maxKeep; i < len(items); i++ {
		_ = os.Remove(filepath.Join(dir, items[i].Name))
	}
}

// Remove deletes a single backup by name (validated).
func Remove(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("backup: invalid name %q", name)
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, name))
}

// FullPath returns the absolute path for a validated name.
func FullPath(name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("backup: invalid name %q", name)
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// TodayHasBackup reports whether any backup filename carries today's
// date (UTC+8). Used by the scheduler for once-per-day idempotency —
// if today's snapshot already exists (auto or manual), the ticker skips.
func TodayHasBackup() bool {
	today := time.Now().In(diary.CST).Format("20060102")
	items, err := List()
	if err != nil {
		return false
	}
	for _, it := range items {
		// fluctio-20060102-150405.db → date at [8:16]
		if len(it.Name) >= 16 && it.Name[8:16] == today {
			return true
		}
	}
	return false
}
