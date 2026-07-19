package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWritePendingThenListApproveLiveExists(t *testing.T) {
	home := t.TempDir()
	body := []byte("---\nname: foo\ndescription: test\n---\nbody\n")
	meta := PendingMeta{Source: "test", Description: "demo"}
	if err := WritePending(home, "foo", body, meta); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	// File exists in pending, not live.
	pendingFile := filepath.Join(home, "skills-pending", "foo", "SKILL.md")
	if _, err := os.Stat(pendingFile); err != nil {
		t.Fatalf("pending SKILL.md missing: %v", err)
	}
	liveDir := filepath.Join(home, "skills", "foo")
	liveFile := filepath.Join(liveDir, "SKILL.md")
	if _, err := os.Stat(liveFile); !os.IsNotExist(err) {
		t.Fatalf("live SKILL.md should not exist before approve, got err=%v", err)
	}
	// List shows foo.
	entries, err := ListPending(home)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "foo" {
		t.Fatalf("ListPending = %+v, want foo", entries)
	}
	if entries[0].Meta.CreatedAt.IsZero() {
		t.Fatalf("ListPending: meta.CreatedAt not populated")
	}
	if entries[0].Meta.Source != "test" {
		t.Fatalf("ListPending: meta.Source = %q, want %q", entries[0].Meta.Source, "test")
	}

	// Approve moves to live.
	livePath, err := ApprovePending(home, "foo")
	if err != nil {
		t.Fatalf("ApprovePending: %v", err)
	}
	if livePath != liveDir {
		t.Fatalf("livePath = %q, want %q", livePath, liveDir)
	}
	got, err := os.ReadFile(liveFile)
	if err != nil {
		t.Fatalf("live SKILL.md missing after approve: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("live content mismatch: got %q", got)
	}
	// Pending entry is gone.
	entries, _ = ListPending(home)
	if len(entries) != 0 {
		t.Fatalf("ListPending after approve = %d, want 0", len(entries))
	}
	// .pending.json must NOT follow to the live tree — it's pending-only metadata.
	if _, err := os.Stat(filepath.Join(home, "skills", "foo", ".pending.json")); !os.IsNotExist(err) {
		t.Fatalf(".pending.json should not exist in live, got err=%v", err)
	}
}

func TestApprovePendingOverwritesExistingLive(t *testing.T) {
	home := t.TempDir()
	// Pre-existing live skill.
	oldLive := filepath.Join(home, "skills", "foo", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(oldLive), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldLive, []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Stage a new version and approve.
	newBody := []byte("NEW")
	if err := WritePending(home, "foo", newBody, PendingMeta{Source: "test"}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	if _, err := ApprovePending(home, "foo"); err != nil {
		t.Fatalf("ApprovePending: %v", err)
	}
	got, err := os.ReadFile(oldLive)
	if err != nil {
		t.Fatalf("read live: %v", err)
	}
	if string(got) != "NEW" {
		t.Fatalf("live content = %q, want NEW", got)
	}
}

func TestRejectPendingRemovesEntry(t *testing.T) {
	home := t.TempDir()
	if err := WritePending(home, "bar", []byte("x"), PendingMeta{Source: "test"}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	if err := RejectPending(home, "bar"); err != nil {
		t.Fatalf("RejectPending: %v", err)
	}
	entries, _ := ListPending(home)
	if len(entries) != 0 {
		t.Fatalf("ListPending after reject = %d, want 0", len(entries))
	}
	// Idempotent: second reject returns nil.
	if err := RejectPending(home, "bar"); err != nil {
		t.Fatalf("RejectPending second call: %v", err)
	}
}

func TestWritePendingRejectsBadNames(t *testing.T) {
	home := t.TempDir()
	bad := []string{"", "..", ".", "foo/bar", "foo\\bar", "../escape", "a b", "a:b"}
	for _, n := range bad {
		if err := WritePending(home, n, []byte("x"), PendingMeta{}); err == nil {
			t.Fatalf("WritePending accepted bad name %q", n)
		}
	}
}

func TestWritePendingSetsCreatedAtIfZero(t *testing.T) {
	home := t.TempDir()
	if err := WritePending(home, "baz", []byte("x"), PendingMeta{}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	entries, _ := ListPending(home)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Meta.CreatedAt.IsZero() {
		t.Fatalf("expected CreatedAt to be auto-populated, got %+v", entries[0])
	}
}

func TestListPendingEmptyWhenDirMissing(t *testing.T) {
	home := t.TempDir()
	entries, err := ListPending(home)
	if err != nil {
		t.Fatalf("ListPending on missing dir: %v", err)
	}
	if entries != nil && len(entries) != 0 {
		t.Fatalf("entries = %+v, want empty", entries)
	}
}
