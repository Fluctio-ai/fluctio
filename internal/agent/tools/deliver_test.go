package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeliverFile(t *testing.T) {
	root := t.TempDir()
	r := NewRegistry(root, root)
	r.SetUserRoot(root)
	RegisterDeliverTools(r)

	// 在可见域外造一个源文件
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "shot.png")
	if err := os.WriteFile(src, []byte("PNGDATA"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := r.Execute(context.Background(), "deliver_file",
		`{"src":"`+filepath.ToSlash(src)+`"}`)
	if err != nil {
		t.Fatalf("deliver_file: %v", err)
	}
	if !strings.Contains(got, "Delivered 7 bytes to") {
		t.Fatalf("unexpected output: %s", got)
	}
	// 目标落在可见域根下
	entries, _ := os.ReadDir(root)
	if len(entries) == 0 {
		t.Fatalf("nothing delivered into visible root %s", root)
	}
}

// TestDeliverFileEscapeRejection verifies a relative dest with .. cannot write
// outside the visible workspace.
func TestDeliverFileEscapeRejection(t *testing.T) {
	root := t.TempDir()
	r := NewRegistry(root, root)
	r.SetUserRoot(root)
	RegisterDeliverTools(r)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "shot.png")
	if err := os.WriteFile(src, []byte("X"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Use a unique escape filename so leftovers from prior unfixed runs can't
	// cause false positives.
	escapeName := "evil-" + strings.ReplaceAll(t.Name(), "/", "-") + ".txt"
	escapeTarget := filepath.Join(filepath.Dir(filepath.Dir(root)), escapeName)
	// Pre-clean: remove a leftover if it exists (defensive — should never be
	// created by this test once the fix lands).
	_ = os.Remove(escapeTarget)

	_, err := r.Execute(context.Background(), "deliver_file",
		`{"src":"`+filepath.ToSlash(src)+`","dest":"../../` + escapeName + `"}`)
	if err == nil {
		t.Fatalf("expected error for escape dest, got nil (escapeTarget=%s)", escapeTarget)
	}
	if !strings.Contains(err.Error(), "within the visible workspace") {
		t.Fatalf("expected error mentioning visible workspace, got: %v", err)
	}
	// Assert no file was created at the escaped location.
	if _, statErr := os.Stat(escapeTarget); statErr == nil {
		t.Fatalf("escape succeeded: file created at %s", escapeTarget)
	}
}

// TestDeliverFileContentIntegrity verifies bytes written equal bytes source
// after a successful deliver (catches truncation from unchecked Close).
func TestDeliverFileContentIntegrity(t *testing.T) {
	root := t.TempDir()
	r := NewRegistry(root, root)
	r.SetUserRoot(root)
	RegisterDeliverTools(r)

	srcDir := t.TempDir()
	payload := strings.Repeat("A", 64*1024) // 64 KiB; larger than many pipe buffers
	src := filepath.Join(srcDir, "big.bin")
	if err := os.WriteFile(src, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := r.Execute(context.Background(), "deliver_file",
		`{"src":"`+filepath.ToSlash(src)+`","dest":"out/big.bin"}`)
	if err != nil {
		t.Fatalf("deliver_file: %v", err)
	}
	if !strings.Contains(got, "Delivered 65536 bytes to") {
		t.Fatalf("unexpected output: %s", got)
	}

	dst := filepath.Join(root, "out", "big.bin")
	gotBytes, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read back delivered file: %v", err)
	}
	if string(gotBytes) != payload {
		t.Fatalf("content mismatch: wrote %d bytes, read back %d bytes", len(payload), len(gotBytes))
	}
}

// TestDeliverFileAbsoluteDestRejection verifies an absolute dest is rejected.
func TestDeliverFileAbsoluteDestRejection(t *testing.T) {
	root := t.TempDir()
	r := NewRegistry(root, root)
	r.SetUserRoot(root)
	RegisterDeliverTools(r)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "shot.png")
	if err := os.WriteFile(src, []byte("X"), 0o644); err != nil {
		t.Fatal(err)
	}

	absDest := filepath.Join(t.TempDir(), "abs.txt")

	_, err := r.Execute(context.Background(), "deliver_file",
		`{"src":"`+filepath.ToSlash(src)+`","dest":"`+filepath.ToSlash(absDest)+`"}`)
	if err == nil {
		t.Fatalf("expected error for absolute dest, got nil")
	}
	if !strings.Contains(err.Error(), "within the visible workspace") {
		t.Fatalf("expected error mentioning visible workspace, got: %v", err)
	}
}

// TestDeliverFileDotDotFilename verifies that a legitimate filename starting
// with ".." (e.g. "..foo") is accepted, while real parent-traversal paths
// ("..", "../etc/passwd") are still rejected. Regression guard for the
// HasPrefix(ToSlash(rel), "..") false-positive that flagged "..foo" as escape.
func TestDeliverFileDotDotFilename(t *testing.T) {
	root := t.TempDir()
	r := NewRegistry(root, root)
	r.SetUserRoot(root)
	RegisterDeliverTools(r)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "payload.bin")
	if err := os.WriteFile(src, []byte("DOTDOTFOO"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Case 1: "..foo" is a legal filename that happens to start with "..";
	// it must be delivered into the visible root, NOT rejected as traversal.
	got, err := r.Execute(context.Background(), "deliver_file",
		`{"src":"`+filepath.ToSlash(src)+`","dest":"..foo"}`)
	if err != nil {
		t.Fatalf("\"..foo\" should be accepted as a filename, got error: %v", err)
	}
	deliveredPath := filepath.Join(root, "..foo")
	if _, statErr := os.Stat(deliveredPath); statErr != nil {
		t.Fatalf("\"..foo\" was not delivered under root: %v (output=%s)", statErr, got)
	}
	// Sanity: the delivered bytes match.
	b, err := os.ReadFile(deliveredPath)
	if err != nil {
		t.Fatalf("read delivered ..foo: %v", err)
	}
	if string(b) != "DOTDOTFOO" {
		t.Fatalf("..foo content = %q, want %q", string(b), "DOTDOTFOO")
	}

	// Case 2: real parent traversal "../<sibling>" must still be rejected.
	escapeName := "evil-" + strings.ReplaceAll(t.Name(), "/", "-") + ".txt"
	escapeTarget := filepath.Join(filepath.Dir(root), escapeName)
	_ = os.Remove(escapeTarget)
	_, err = r.Execute(context.Background(), "deliver_file",
		`{"src":"`+filepath.ToSlash(src)+`","dest":"../`+escapeName+`"}`)
	if err == nil {
		t.Fatalf("../escape dest should be rejected")
	}
	if !strings.Contains(err.Error(), "within the visible workspace") {
		t.Fatalf("expected visible-workspace error for ../escape, got: %v", err)
	}
	if _, statErr := os.Stat(escapeTarget); statErr == nil {
		t.Fatalf("../escape succeeded: file created at %s", escapeTarget)
	}

	// Case 3: ".." exactly must also be rejected (resolves to parent dir).
	_, err = r.Execute(context.Background(), "deliver_file",
		`{"src":"`+filepath.ToSlash(src)+`","dest":".."}`)
	if err == nil {
		t.Fatalf("\"..\" dest should be rejected as parent traversal")
	}
	if !strings.Contains(err.Error(), "within the visible workspace") {
		t.Fatalf("expected visible-workspace error for \"..\", got: %v", err)
	}
}
