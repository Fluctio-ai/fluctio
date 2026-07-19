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
	RegisterSkillManage(r, home, "fluctio skill approve", nil)

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
	RegisterSkillManage(r, home, "fluctio skill approve", nil)
	fn := r.GetFunc("skill_manage")

	// Match the full pending_test.go isValidSkillName matrix so the agent-
	// facing tool and the low-level validator stay in sync.
	for _, n := range []string{"", "..", ".", "foo/bar", "foo\\bar", "../escape", "a b", "a:b"} {
		raw, _ := json.Marshal(map[string]string{"action": "create", "name": n, "content": "x"})
		if _, err := fn(context.Background(), raw); err == nil {
			t.Fatalf("skill_manage accepted bad name %q", n)
		}
	}
}

func TestSkillManageRejectsUnknownAction(t *testing.T) {
	home := t.TempDir()
	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterSkillManage(r, home, "fluctio skill approve", nil)
	fn := r.GetFunc("skill_manage")

	raw, _ := json.Marshal(map[string]string{"action": "bogus", "name": "x", "content": "x"})
	if _, err := fn(context.Background(), raw); err == nil {
		t.Fatal("skill_manage accepted unknown action")
	}
}

func TestSkillManageDeleteStagesDeletion(t *testing.T) {
	home := t.TempDir()
	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterSkillManage(r, home, "fluctio skill approve", nil)
	fn := r.GetFunc("skill_manage")

	// Pre-existing live skill we want removed.
	liveDir := filepath.Join(home, "skills", "foo")
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(liveDir, "SKILL.md"), []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}

	// delete requires no content.
	raw, _ := json.Marshal(map[string]string{"action": "delete", "name": "foo"})
	got, err := fn(context.Background(), raw)
	if err != nil {
		t.Fatalf("skill_manage delete: %v", err)
	}
	if !strings.Contains(got, "DELETION") || !strings.Contains(got, "fluctio skill approve foo") {
		t.Fatalf("delete result missing markers: %s", got)
	}

	entries, err := skills.ListPending(home)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(entries) != 1 || entries[0].Meta.Action != "delete" {
		t.Fatalf("pending = %+v, want delete", entries)
	}
	// Live still intact pre-approve.
	if _, err := os.Stat(filepath.Join(liveDir, "SKILL.md")); err != nil {
		t.Fatalf("live missing pre-approve: %v", err)
	}

	// Approve actually removes.
	if _, err := skills.ApprovePending(home, "foo"); err != nil {
		t.Fatalf("ApprovePending: %v", err)
	}
	if _, err := os.Stat(liveDir); !os.IsNotExist(err) {
		t.Fatalf("live dir should be gone, got err=%v", err)
	}
}

func TestSkillManageWriteFileStagesSubFile(t *testing.T) {
	home := t.TempDir()
	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterSkillManage(r, home, "fluctio skill approve", nil)
	fn := r.GetFunc("skill_manage")

	raw, _ := json.Marshal(map[string]string{
		"action":  "write_file",
		"name":    "foo",
		"path":    "templates/hello.txt",
		"content": "HI",
	})
	got, err := fn(context.Background(), raw)
	if err != nil {
		t.Fatalf("skill_manage write_file: %v", err)
	}
	if !strings.Contains(got, "templates/hello.txt") || !strings.Contains(got, "fluctio skill approve foo") {
		t.Fatalf("write_file result missing markers: %s", got)
	}
	entries, err := skills.ListPending(home)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(entries) != 1 || entries[0].Meta.Action != "write_file" || entries[0].Meta.File != "templates/hello.txt" {
		t.Fatalf("pending = %+v", entries)
	}
	// Approve merges the file into live.
	if _, err := skills.ApprovePending(home, "foo"); err != nil {
		t.Fatalf("ApprovePending: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(home, "skills", "foo", "templates", "hello.txt"))
	if err != nil {
		t.Fatalf("merged file missing: %v", err)
	}
	if string(b) != "HI" {
		t.Fatalf("merged content = %q, want HI", b)
	}
}

func TestSkillManageWriteFileRejectsBadPath(t *testing.T) {
	home := t.TempDir()
	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterSkillManage(r, home, "fluctio skill approve", nil)
	fn := r.GetFunc("skill_manage")

	for _, p := range []string{"", "..", "../x", "/abs", "a\\b"} {
		raw, _ := json.Marshal(map[string]string{
			"action":  "write_file",
			"name":    "foo",
			"path":    p,
			"content": "x",
		})
		if _, err := fn(context.Background(), raw); err == nil {
			t.Fatalf("skill_manage write_file accepted bad path %q", p)
		}
	}
}

func TestSkillManageRemoveFileStagesRemoval(t *testing.T) {
	home := t.TempDir()
	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterSkillManage(r, home, "fluctio skill approve", nil)
	fn := r.GetFunc("skill_manage")

	// Pre-existing live sub-file.
	liveDir := filepath.Join(home, "skills", "foo", "templates")
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(liveDir, "x.txt"), []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(map[string]string{
		"action": "remove_file",
		"name":   "foo",
		"path":   "templates/x.txt",
	})
	got, err := fn(context.Background(), raw)
	if err != nil {
		t.Fatalf("skill_manage remove_file: %v", err)
	}
	if !strings.Contains(got, "templates/x.txt") || !strings.Contains(got, "fluctio skill approve foo") {
		t.Fatalf("remove_file result missing markers: %s", got)
	}
	entries, err := skills.ListPending(home)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(entries) != 1 || entries[0].Meta.Action != "remove_file" || entries[0].Meta.File != "templates/x.txt" {
		t.Fatalf("pending = %+v", entries)
	}
	// Approve deletes just that file.
	if _, err := skills.ApprovePending(home, "foo"); err != nil {
		t.Fatalf("ApprovePending: %v", err)
	}
	if _, err := os.Stat(filepath.Join(liveDir, "x.txt")); !os.IsNotExist(err) {
		t.Fatalf("file should be gone, got err=%v", err)
	}
}

func TestSkillManageEditAliasActsAsPatch(t *testing.T) {
	home := t.TempDir()
	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterSkillManage(r, home, "fluctio skill approve", nil)
	fn := r.GetFunc("skill_manage")

	raw, _ := json.Marshal(map[string]string{
		"action":  "edit",
		"name":    "foo",
		"content": "---\nname: foo\n---\nbody",
	})
	if _, err := fn(context.Background(), raw); err != nil {
		t.Fatalf("skill_manage edit: %v", err)
	}
	entries, err := skills.ListPending(home)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "foo" {
		t.Fatalf("pending = %+v", entries)
	}
	// SKILL.md staged like create/patch.
	if _, err := os.Stat(filepath.Join(home, "skills-pending", "foo", "SKILL.md")); err != nil {
		t.Fatalf("SKILL.md not staged: %v", err)
	}
}

func TestSkillManageParserSurfacesGating(t *testing.T) {
	home := t.TempDir()
	r := NewRegistry(t.TempDir(), t.TempDir())
	parser := func(b []byte) *SkillManifest {
		return &SkillManifest{Name: "demo", Description: "demo skill", Gated: true, GateReason: "requires OS xyz"}
	}
	RegisterSkillManage(r, home, "fluctio skill approve", parser)
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
