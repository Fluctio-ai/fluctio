package backup

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

func cstNow() time.Time {
	return time.Now().In(time.FixedZone("CST", 8*3600))
}

func setupTestStore(t *testing.T) *store.DBStore {
	t.Helper()
	home := t.TempDir()
	t.Setenv("FLUCTIO_HOME", home)
	st, err := store.New(&store.StorageConfig{Type: store.StorageSQLite, AutoMigrate: true}, home)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st.(*store.DBStore)
}

// TestBackupLifecycle exercises the full file-level backup flow: create a
// VACUUM INTO snapshot, confirm it is content-complete (seeded row
// survives), list/rotate/remove, and that traversal names are rejected.
func TestBackupLifecycle(t *testing.T) {
	ctx := context.Background()
	dbs := setupTestStore(t)
	db := dbs.DB()

	// Seed a table so the snapshot can be checked for content completeness.
	if _, err := db.ExecContext(ctx, "CREATE TABLE smoke (id INTEGER, val TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO smoke (id, val) VALUES (1, 'hello')"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if TodayHasBackup() {
		t.Fatal("TodayHasBackup=true on empty dir, want false")
	}

	// Create a snapshot.
	name, size, err := Create(ctx, dbs, cstNow())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if size <= 0 {
		t.Fatalf("size=%d, want >0", size)
	}
	if !ValidName(name) {
		t.Fatalf("Create returned invalid name %q", name)
	}
	if !TodayHasBackup() {
		t.Fatal("TodayHasBackup=false after Create, want true")
	}

	// The snapshot must open independently and contain the seeded row —
	// VACUUM INTO folds WAL commits into a self-contained file.
	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	bak, err := store.NewDBStore("sqlite", "file:"+filepath.Join(dir, name)+"?mode=ro")
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	var n int
	if err := bak.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM smoke").Scan(&n); err != nil {
		bak.Close()
		t.Fatalf("query backup: %v", err)
	}
	if n != 1 {
		bak.Close()
		t.Fatalf("backup row count=%d, want 1 (snapshot not content-complete)", n)
	}
	bak.Close() // release the Windows file handle before later Remove/Rotate

	// List shows the one snapshot.
	items, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List len=%d, want 1", len(items))
	}

	// Two more snapshots at distinct timestamps (avoids same-second name
	// collisions so lexical order == time order).
	base := cstNow()
	Create(ctx, dbs, base.Add(time.Second))
	Create(ctx, dbs, base.Add(2*time.Second))
	items, _ = List()
	if len(items) != 3 {
		t.Fatalf("List len=%d, want 3", len(items))
	}

	// Rotate keeps only the newest 2.
	Rotate(2)
	items, _ = List()
	if len(items) != 2 {
		t.Fatalf("after Rotate(2) len=%d, want 2", len(items))
	}
	// Rotate(0) is a no-op (keep everything).
	Rotate(0)
	items, _ = List()
	if len(items) != 2 {
		t.Fatalf("after Rotate(0) len=%d, want 2", len(items))
	}

	// Remove the newest, leaving one.
	target := items[0].Name
	if err := Remove(target); err != nil {
		t.Fatalf("Remove(%q): %v", target, err)
	}
	items, _ = List()
	if len(items) != 1 {
		t.Fatalf("after Remove len=%d, want 1", len(items))
	}

	// Remove rejects traversal / malformed names.
	if err := Remove("../evil.db"); err == nil {
		t.Fatal("Remove accepted traversal name")
	}
}

func TestValidName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"fluctio-20260102-030000.db", true},
		{"", false},
		{"../etc/passwd", false},
		{"/abs/fluctio-20260102-030000.db", false},
		{"fluctio-.db", false},            // empty middle (len == prefix+suffix)
		{"evil.db", false},                // wrong prefix
		{"fluctio-20260102-030000.txt", false}, // wrong suffix
	}
	for _, c := range cases {
		if got := ValidName(c.name); got != c.want {
			t.Errorf("ValidName(%q)=%v, want %v", c.name, got, c.want)
		}
	}
}
