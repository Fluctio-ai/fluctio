package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Sandbox containers bind-mount the scope dir read-write, so a
// chatter-driven `ln -s` inside the container lands on the host side of
// the mount. Reads/writes through such a link must not reach files
// outside the scope root.
func TestResolvePathBlocksSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	fs := NewLocalFS(root)

	// Baseline: ordinary put/get inside the scope.
	if err := fs.Put(context.Background(), "a1", "", "", "hello.txt", strings.NewReader("hi"), 0, ""); err != nil {
		t.Fatalf("put baseline: %v", err)
	}
	rc, err := fs.Get(context.Background(), "a1", "", "", "hello.txt")
	if err != nil {
		t.Fatalf("get baseline: %v", err)
	}
	rc.Close()

	// Secret material OUTSIDE the a1 scope root (a sibling of it).
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("operator-only"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "a1", "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}

	if _, err := fs.Get(context.Background(), "a1", "", "", "link.txt"); err == nil {
		t.Fatal("Get through escape symlink succeeded, want blocked")
	}

	// A symlinked PARENT must not smuggle a write outside the root.
	otherDir := filepath.Join(root, "other")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(otherDir, filepath.Join(root, "a1", "sub")); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	if err := fs.Put(context.Background(), "a1", "", "", "sub/x.txt", strings.NewReader("nope"), 0, ""); err == nil {
		t.Fatal("Put through symlinked parent succeeded, want blocked")
	}

	// Symlinks that stay INSIDE the scope keep working.
	if err := os.Symlink(filepath.Join(root, "a1", "hello.txt"), filepath.Join(root, "a1", "alias.txt")); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	rc, err = fs.Get(context.Background(), "a1", "", "", "alias.txt")
	if err != nil {
		t.Fatalf("Get through inside-root symlink failed: %v", err)
	}
	rc.Close()
}
