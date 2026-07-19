package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/skills"
)

func TestSkillManageWritesToPendingNotLive(t *testing.T) {
	home := t.TempDir()
	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterSkillManage(r, home, "fluctio skill approve", nil, nil)

	fn := r.GetFunc("skill_manage")
	if fn == nil {
		t.Fatal("skill_manage not registered")
	}

	body := "---\nname: demo\ndescription: x\n---\nhello\n"
	raw, _ := json.Marshal(map[string]string{
		"action":  "create",
		"name":    "demo",
		"content": body,
	})
	got, err := fn(context.Background(), raw)
	if err != nil {
		t.Fatalf("skill_manage returned error: %v", err)
	}
	// Result must surface the pending state + the exact approve command.
	if !strings.Contains(got, "PENDING") {
		t.Fatalf("result missing PENDING marker: %s", got)
	}
	if !strings.Contains(got, "fluctio skill approve demo") {
		t.Fatalf("result missing approve hint: %s", got)
	}

	// SKILL.md landed under skills-pending, NOT skills.
	pendingPath := filepath.Join(home, "skills-pending", "demo", "SKILL.md")
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("pending SKILL.md not written: %v", err)
	}
	livePath := filepath.Join(home, "skills", "demo", "SKILL.md")
	if _, err := os.Stat(livePath); !os.IsNotExist(err) {
		t.Fatalf("live SKILL.md should not exist, got err=%v", err)
	}

	// ListPending sees it with skill_manage source.
	entries, err := skills.ListPending(home)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "demo" {
		t.Fatalf("ListPending = %+v, want demo", entries)
	}
	if entries[0].Meta.Source != "skill_manage" {
		t.Fatalf("Source = %q, want skill_manage", entries[0].Meta.Source)
	}

	// Effect declared as SideWritesFile — skill_manage persists a file even
	// though it's not live yet.
	if eff := r.SideEffectOf("skill_manage"); eff != SideWritesFile {
		t.Fatalf("SideEffectOf = %v, want SideWritesFile", eff)
	}
}

func TestSkillManageRejectsBadNames(t *testing.T) {
	home := t.TempDir()
	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterSkillManage(r, home, "fluctio skill approve", nil, nil)
	fn := r.GetFunc("skill_manage")

	for _, n := range []string{"", "..", "foo/bar", "a b"} {
		raw, _ := json.Marshal(map[string]string{"action": "create", "name": n, "content": "x"})
		if _, err := fn(context.Background(), raw); err == nil {
			t.Fatalf("skill_manage accepted bad name %q", n)
		}
	}
}

func TestSkillManageRejectsUnknownAction(t *testing.T) {
	home := t.TempDir()
	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterSkillManage(r, home, "fluctio skill approve", nil, nil)
	fn := r.GetFunc("skill_manage")

	raw, _ := json.Marshal(map[string]string{"action": "delete", "name": "x", "content": "x"})
	if _, err := fn(context.Background(), raw); err == nil {
		t.Fatal("skill_manage accepted unknown action")
	}
}

func TestSkillManageParserSurfacesGating(t *testing.T) {
	home := t.TempDir()
	r := NewRegistry(t.TempDir(), t.TempDir())
	parser := func(b []byte) *SkillManifest {
		return &SkillManifest{Name: "demo", Description: "demo skill", Gated: true, GateReason: "requires OS xyz"}
	}
	RegisterSkillManage(r, home, "fluctio skill approve", parser, nil)
	fn := r.GetFunc("skill_manage")
	raw, _ := json.Marshal(map[string]string{
		"action":  "create",
		"name":    "demo",
		"content": "---\nname: demo\n---\nbody",
	})
	got, err := fn(context.Background(), raw)
	if err != nil {
		t.Fatalf("skill_manage error: %v", err)
	}
	if !strings.Contains(got, "requires OS xyz") || !strings.Contains(got, "Heads-up") {
		t.Fatalf("parser output not surfaced: %s", got)
	}
	if !strings.Contains(got, "demo skill") {
		t.Fatalf("description not surfaced: %s", got)
	}
}
