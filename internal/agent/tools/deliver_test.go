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
