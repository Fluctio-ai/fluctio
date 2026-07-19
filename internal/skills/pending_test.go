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

// TestStageDeletePendingThenApproveRemovesLive covers the delete action:
// StageDeletePending writes .pending.json (Action=delete) without a
// SKILL.md body, and ApprovePending removes the live skill dir entirely.
func TestStageDeletePendingThenApproveRemovesLive(t *testing.T) {
	home := t.TempDir()
	// Pre-existing live skill we want gone.
	liveDir := filepath.Join(home, "skills", "foo")
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(liveDir, "SKILL.md"), []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := StageDeletePending(home, "foo", PendingMeta{Source: "test"}); err != nil {
		t.Fatalf("StageDeletePending: %v", err)
	}
	// Pending entry visible with Action=delete.
	entries, err := ListPending(home)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "foo" || entries[0].Meta.Action != "delete" {
		t.Fatalf("ListPending = %+v, want foo/delete", entries)
	}
	// No SKILL.md staged for a delete (only the meta file).
	if _, err := os.Stat(filepath.Join(home, "skills-pending", "foo", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("delete should not stage SKILL.md, got err=%v", err)
	}
	// Live skill still present pre-approve.
	if _, err := os.Stat(filepath.Join(liveDir, "SKILL.md")); err != nil {
		t.Fatalf("live missing pre-approve: %v", err)
	}

	if _, err := ApprovePending(home, "foo"); err != nil {
		t.Fatalf("ApprovePending: %v", err)
	}
	// Live dir gone.
	if _, err := os.Stat(liveDir); !os.IsNotExist(err) {
		t.Fatalf("live dir should be gone after approve, got err=%v", err)
	}
	// Pending entry cleaned up.
	entries, _ = ListPending(home)
	if len(entries) != 0 {
		t.Fatalf("pending not cleaned after approve: %+v", entries)
	}
}

// TestStageDeletePendingApproveMissingLiveIsSuccess: approving a delete
// when no live skill exists is a no-op success (idempotent removal).
func TestStageDeletePendingApproveMissingLiveIsSuccess(t *testing.T) {
	home := t.TempDir()
	if err := StageDeletePending(home, "ghost", PendingMeta{Source: "test"}); err != nil {
		t.Fatalf("StageDeletePending: %v", err)
	}
	if _, err := ApprovePending(home, "ghost"); err != nil {
		t.Fatalf("approve delete on missing live should succeed: %v", err)
	}
}

// TestStageFilePendingThenApproveMergesIntoLive covers the write_file
// action: StageFilePending writes the sub-file + .pending.json, ApprovePending
// ensures the live skill dir exists and copies the file in.
func TestStageFilePendingThenApproveMergesIntoLive(t *testing.T) {
	home := t.TempDir()
	if err := StageFilePending(home, "foo", "templates/x.txt", []byte("HELLO"), PendingMeta{Source: "test"}); err != nil {
		t.Fatalf("StageFilePending: %v", err)
	}
	entries, err := ListPending(home)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(entries) != 1 || entries[0].Meta.Action != "write_file" || entries[0].Meta.File != "templates/x.txt" {
		t.Fatalf("ListPending = %+v, want write_file/templates/x.txt", entries)
	}
	// Live doesn't exist yet pre-approve.
	if _, err := os.Stat(filepath.Join(home, "skills", "foo")); !os.IsNotExist(err) {
		t.Fatalf("live dir should not exist pre-approve, got err=%v", err)
	}

	if _, err := ApprovePending(home, "foo"); err != nil {
		t.Fatalf("ApprovePending: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(home, "skills", "foo", "templates", "x.txt"))
	if err != nil {
		t.Fatalf("read merged file: %v", err)
	}
	if string(got) != "HELLO" {
		t.Fatalf("content = %q, want HELLO", got)
	}
	// Pending entry cleaned up.
	entries, _ = ListPending(home)
	if len(entries) != 0 {
		t.Fatalf("pending not cleaned: %+v", entries)
	}
}

// TestStageFilePendingApproveMergesIntoExistingLive: write_file into an
// already-live skill preserves existing files and adds the new one.
func TestStageFilePendingApproveMergesIntoExistingLive(t *testing.T) {
	home := t.TempDir()
	// Existing live skill with a body.
	liveDir := filepath.Join(home, "skills", "foo")
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(liveDir, "SKILL.md"), []byte("KEEP"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := StageFilePending(home, "foo", "extras/bar.md", []byte("NEW"), PendingMeta{Source: "test"}); err != nil {
		t.Fatalf("StageFilePending: %v", err)
	}
	if _, err := ApprovePending(home, "foo"); err != nil {
		t.Fatalf("ApprovePending: %v", err)
	}
	// Existing SKILL.md untouched, new file present.
	if got, err := os.ReadFile(filepath.Join(liveDir, "SKILL.md")); err != nil || string(got) != "KEEP" {
		t.Fatalf("SKILL.md should be preserved, got %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(liveDir, "extras", "bar.md")); err != nil || string(got) != "NEW" {
		t.Fatalf("extras/bar.md = %q err=%v, want NEW", got, err)
	}
}

// TestStageFilePendingRejectsBadPaths verifies the path sanitizer rejects
// traversal, absolute, drive-letter, backslash, and odd character paths.
func TestStageFilePendingRejectsBadPaths(t *testing.T) {
	home := t.TempDir()
	bad := []string{
		"",       // empty
		"..",     // traversal
		"../x",   // escape
		"a/../b", // mid traversal
		"/abs",   // absolute
		"a\\b",   // backslash (Windows sep)
		"C:/x",   // drive letter
		"C:\\x",  // drive letter windows
		"a b",    // space
		"a:b",    // colon
		"./a",    // leading dot segment
	}
	for _, p := range bad {
		if err := StageFilePending(home, "foo", p, []byte("x"), PendingMeta{}); err == nil {
			t.Fatalf("StageFilePending accepted bad path %q", p)
		}
	}
}

// TestStageRemoveFilePendingThenApproveDeletesFile covers the remove_file
// action: stage a removal of a sub-file, approve deletes just that file
// from the live skill dir (the rest of the skill survives).
func TestStageRemoveFilePendingThenApproveDeletesFile(t *testing.T) {
	home := t.TempDir()
	liveDir := filepath.Join(home, "skills", "foo", "templates")
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(liveDir, "x.txt"), []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Another file in the same skill that must survive.
	if err := os.WriteFile(filepath.Join(home, "skills", "foo", "SKILL.md"), []byte("KEEP"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := StageRemoveFilePending(home, "foo", "templates/x.txt", PendingMeta{Source: "test"}); err != nil {
		t.Fatalf("StageRemoveFilePending: %v", err)
	}
	entries, err := ListPending(home)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(entries) != 1 || entries[0].Meta.Action != "remove_file" || entries[0].Meta.File != "templates/x.txt" {
		t.Fatalf("ListPending = %+v, want remove_file/templates/x.txt", entries)
	}

	if _, err := ApprovePending(home, "foo"); err != nil {
		t.Fatalf("ApprovePending: %v", err)
	}
	// Sub-file removed.
	if _, err := os.Stat(filepath.Join(liveDir, "x.txt")); !os.IsNotExist(err) {
		t.Fatalf("templates/x.txt should be gone, got err=%v", err)
	}
	// Rest of skill survives.
	if got, err := os.ReadFile(filepath.Join(home, "skills", "foo", "SKILL.md")); err != nil || string(got) != "KEEP" {
		t.Fatalf("SKILL.md should be preserved, got %q err=%v", got, err)
	}
}

// TestStageRemoveFilePendingApproveMissingFileIsSuccess: approving
// remove_file when the target is already absent is a no-op success.
func TestStageRemoveFilePendingApproveMissingFileIsSuccess(t *testing.T) {
	home := t.TempDir()
	// Live skill exists but the target sub-file does not.
	liveDir := filepath.Join(home, "skills", "foo")
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := StageRemoveFilePending(home, "foo", "absent.txt", PendingMeta{Source: "test"}); err != nil {
		t.Fatalf("StageRemoveFilePending: %v", err)
	}
	if _, err := ApprovePending(home, "foo"); err != nil {
		t.Fatalf("approve remove_file on absent target should succeed: %v", err)
	}
}

// TestStageRemoveFilePendingRejectsBadPaths: same path rules as write_file.
func TestStageRemoveFilePendingRejectsBadPaths(t *testing.T) {
	home := t.TempDir()
	for _, p := range []string{"", "..", "../x", "/abs", "a\\b", "C:/x"} {
		if err := StageRemoveFilePending(home, "foo", p, PendingMeta{}); err == nil {
			t.Fatalf("StageRemoveFilePending accepted bad path %q", p)
		}
	}
}
