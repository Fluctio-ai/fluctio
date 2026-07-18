package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSkillRegisteredByDefaultAndLoadsFullContent(t *testing.T) {
	home := t.TempDir()
	skillDir := filepath.Join(home, "skills", "chart-maker")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: chart-maker
description: Build charts from tabular data.
---

Run {baseDir}/scripts/render.py with JSON input.`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterLoadSkill(r, []string{filepath.Join(home, "skills")}, nil)

	fn := r.GetFunc("load_skill")
	if fn == nil {
		t.Fatal("load_skill was not registered")
	}
	rawArgs, err := json.Marshal(map[string]string{"name": "chart-maker"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := fn(context.Background(), rawArgs)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(got, "Run "+skillDir+"/scripts/render.py") {
		t.Fatalf("load_skill did not return full content with baseDir replaced:\n%s", got)
	}
	if !strings.Contains(got, "INTERNAL CONTEXT") {
		t.Fatalf("load_skill output missing internal wrapper:\n%s", got)
	}
}

func TestLoadSkillUsesDirectoryPrecedence(t *testing.T) {
	agentSkills := filepath.Join(t.TempDir(), "skills")
	userSkills := filepath.Join(t.TempDir(), "skills")
	for _, dir := range []string{agentSkills, userSkills} {
		if err := os.MkdirAll(filepath.Join(dir, "shared"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(userSkills, "shared", "SKILL.md"), []byte("user version"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentSkills, "shared", "SKILL.md"), []byte("agent version"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterLoadSkill(r, []string{agentSkills, userSkills}, nil)
	rawArgs, err := json.Marshal(map[string]string{"name": "shared"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.GetFunc("load_skill")(context.Background(), rawArgs)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(got, "agent version") {
		t.Fatalf("load_skill did not use first matching directory:\n%s", got)
	}
	if strings.Contains(got, "user version") {
		t.Fatalf("load_skill should not include lower-priority skill:\n%s", got)
	}
}

func TestLoadSkillMarksMissingEnvRequirement(t *testing.T) {
	home := t.TempDir()
	skillDir := filepath.Join(home, "skills", "deepcoin-trade")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: deepcoin-trade
description: Place orders.
metadata:
  openclaw:
    requires:
      env: ["DC_API_KEY", "DC_SECRET_KEY"]
---

Authenticated instructions.`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(t.TempDir(), t.TempDir())
	gate := map[string]SkillGate{
		"deepcoin-trade": {
			Gated:  true,
			Reason: "missing required env var(s): DC_API_KEY, DC_SECRET_KEY",
		},
	}
	RegisterLoadSkill(r, []string{filepath.Join(home, "skills")}, gate)
	rawArgs, err := json.Marshal(map[string]string{"name": "deepcoin-trade"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.GetFunc("load_skill")(context.Background(), rawArgs)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(got, "SKILL CURRENTLY UNAVAILABLE") {
		t.Fatalf("load_skill output missing unavailable warning:\n%s", got)
	}
	if !strings.Contains(got, "DC_API_KEY, DC_SECRET_KEY") {
		t.Fatalf("load_skill output missing env names:\n%s", got)
	}
}

func TestLoadSkillOnMissingFallback(t *testing.T) {
	home := t.TempDir()
	skillDir := filepath.Join(home, "skills", "article-to-image")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: article-to-image\ndescription: Make images.\n---\nRun {baseDir}/render.sh"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(t.TempDir(), t.TempDir())
	gate := map[string]SkillGate{
		"article-to-image": {
			Gated:    true,
			Reason:   "required binary \"bash\" not found on PATH",
			OnMissing: "use powershell",
		},
	}
	RegisterLoadSkill(r, []string{filepath.Join(home, "skills")}, gate)
	rawArgs, err := json.Marshal(map[string]string{"name": "article-to-image"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.GetFunc("load_skill")(context.Background(), rawArgs)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(got, "[SKILL FALLBACK: use powershell]") {
		t.Fatalf("load_skill output missing OnMissing fallback banner:\n%s", got)
	}
	// OnMissing takes priority over the bare unavailable banner per the
	// gate-banner spec; verify the priority holds (no UNAVAILABLE line).
	if strings.Contains(got, "SKILL CURRENTLY UNAVAILABLE") {
		t.Fatalf("OnMissing should suppress the generic UNAVAILABLE banner:\n%s", got)
	}
}
