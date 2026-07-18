package tools

import (
	"context"
	"encoding/json"
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
