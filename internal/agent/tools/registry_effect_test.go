package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
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

	// Case 1: 相对路径（workspace 内）→ 可见
	vis, vr := r.ReachabilityVerdict("notes/x.txt")
	if !vis {
		t.Fatalf("relative path should be visible")
	}
	if vr != root {
		t.Fatalf("visibleRoot = %q, want %q", vr, root)
	}

	// Case 2: 绝对路径 INSIDE visibleRoot → 可见（deliver_file 的产物落点）
	insideAbs := filepath.Join(root, "shot.png")
	vis, vr = r.ReachabilityVerdict(insideAbs)
	if !vis {
		t.Fatalf("absolute path inside visibleRoot (%s) should be visible", insideAbs)
	}
	if vr != root {
		t.Fatalf("visibleRoot = %q, want %q", vr, root)
	}

	// Case 3: 绝对路径 OUTSIDE visibleRoot → 不可见（跨平台：t.TempDir() 在 Windows/Linux 均返回绝对路径）
	outsideAbs := filepath.Join(t.TempDir(), "other.png")
	// Guard against the unlikely case where t.TempDir() returns the same dir twice.
	if strings.HasPrefix(filepath.ToSlash(outsideAbs)+"/", filepath.ToSlash(root)+"/") {
		t.Skipf("outsideAbs %s unexpectedly inside root %s; cannot verify outside case", outsideAbs, root)
	}
	vis, _ = r.ReachabilityVerdict(outsideAbs)
	if vis {
		t.Fatalf("absolute path outside visibleRoot (%s) should not be visible", outsideAbs)
	}
}

func TestRegisterFromWithEffect(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())
	fn := func(ctx context.Context, args json.RawMessage) (string, error) { return "ok", nil }
	r.RegisterFromWithEffect("mcp_x_screenshot", "d", map[string]interface{}{"type": "object"}, fn, SourceMCP, SideWritesFile)
	if got := r.SideEffectOf("mcp_x_screenshot"); got != SideWritesFile {
		t.Fatalf("effect = %v, want SideWritesFile", got)
	}
	// source 也应是 SourceMCP（验证没误用 RegisterWithEffect 的 SourceBuiltin）
	if t2, ok := r.tools["mcp_x_screenshot"]; !ok || t2.source != SourceMCP {
		t.Fatalf("source not SourceMCP")
	}
}
