package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/skills"
)

func TestPendingSkillNamesEmpty(t *testing.T) {
	// TempDir with no skills-pending subdir → empty, no error.
	dir := t.TempDir()
	names, err := pendingSkillNames(dir)
	if err != nil {
		t.Fatalf("pendingSkillNames: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("expected empty, got %v", names)
	}
}

func TestPendingSkillNamesNoDir(t *testing.T) {
	// Non-existent pending dir → empty (ListPending swallows IsNotExist),
	// and the helper must not create the dir as a side-effect.
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	names, err := pendingSkillNames(dir)
	if err != nil {
		t.Fatalf("pendingSkillNames: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("expected empty for missing dir, got %v", names)
	}
	if _, err := os.Stat(filepath.Join(dir, "skills-pending")); !os.IsNotExist(err) {
		t.Fatalf("pending dir should not have been created: %v", err)
	}
}

func TestPendingSkillNamesStaged(t *testing.T) {
	dir := t.TempDir()
	meta := skills.PendingMeta{Source: "test", Action: skills.ActionCreate}
	if err := skills.WritePending(dir, "alpha", []byte("---\nname: alpha\n---\nbody"), meta); err != nil {
		t.Fatalf("WritePending alpha: %v", err)
	}
	if err := skills.WritePending(dir, "beta", []byte("---\nname: beta\n---\nbody"), meta); err != nil {
		t.Fatalf("WritePending beta: %v", err)
	}
	names, err := pendingSkillNames(dir)
	if err != nil {
		t.Fatalf("pendingSkillNames: %v", err)
	}
	want := []string{"alpha", "beta"}
	if len(names) != 2 || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("got %v, want %v", names, want)
	}
}
