package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestSideEffectRegistration(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())
	fn := func(ctx context.Context, args json.RawMessage) (string, error) {
		return "ok", nil
	}
	r.RegisterWithEffect("dummy_writes", "desc", map[string]interface{}{"type": "object"}, fn, SideWritesFile)
	r.Register("dummy_pure", "desc", map[string]interface{}{"type": "object"}, fn)

	if got := r.SideEffectOf("dummy_writes"); got != SideWritesFile {
		t.Fatalf("dummy_writes effect = %v, want SideWritesFile", got)
	}
	if got := r.SideEffectOf("dummy_pure"); got != SidePure {
		t.Fatalf("dummy_pure effect = %v, want SidePure (default)", got)
	}
	if got := r.SideEffectOf("nonexistent"); got != SidePure {
		t.Fatalf("nonexistent effect = %v, want SidePure", got)
	}
}

func TestReachabilityVerdict(t *testing.T) {
	root := t.TempDir()
	r := NewRegistry(root, root)
	r.SetUserRoot(root)

	// 相对路径（workspace 内）→ 可见
	vis, vr := r.ReachabilityVerdict("notes/x.txt")
	if !vis {
		t.Fatalf("relative path should be visible")
	}
	if vr != root {
		t.Fatalf("visibleRoot = %q, want %q", vr, root)
	}

	// 绝对路径 → 不可见（跨平台：t.TempDir() 在 Windows/Linux 均返回绝对路径）
	absPath := filepath.Join(t.TempDir(), "shot.png")
	vis, _ = r.ReachabilityVerdict(absPath)
	if vis {
		t.Fatalf("absolute path %s should not be visible", absPath)
	}
}
