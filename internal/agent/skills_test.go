package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/config"
)

func TestBuildSkillsSummaryUsesProgressiveDisclosureByDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLUCTIO_HOME", home)
	skillDir := filepath.Join(home, "skills", "chart-maker")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: chart-maker
description: Build charts from tabular data.
---

SECRET_INLINE_BODY_SHOULD_NOT_APPEAR
Run scripts/render.py with JSON input.`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := NewSkillsLoaderWithGlobal(home, t.TempDir(), config.SkillsConfig{}, config.SkillsCfg{})
	summary := loader.BuildSkillsSummary(loader.LoadSkills())

	if !strings.Contains(summary, "chart-maker") {
		t.Fatalf("summary missing skill name:\n%s", summary)
	}
	if !strings.Contains(summary, "Build charts from tabular data") {
		t.Fatalf("summary missing skill description:\n%s", summary)
	}
	if strings.Contains(summary, "SECRET_INLINE_BODY_SHOULD_NOT_APPEAR") {
		t.Fatalf("summary leaked SKILL.md body:\n%s", summary)
	}
	if !strings.Contains(summary, "load_skill") {
		t.Fatalf("summary should tell the model to call load_skill:\n%s", summary)
	}
}

func TestLoadSkillsDoesNotKeepBodyContentByDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLUCTIO_HOME", home)
	skillDir := filepath.Join(home, "skills", "chart-maker")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: chart-maker
description: Build charts from tabular data.
---

BODY_SHOULD_STAY_ON_DISK_UNTIL_LOAD_SKILL`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := NewSkillsLoaderWithGlobal(home, t.TempDir(), config.SkillsConfig{}, config.SkillsCfg{})
	skills := loader.LoadSkills()

	if len(skills) != 1 {
		t.Fatalf("skills len = %d, want 1", len(skills))
	}
	if skills[0].Content != "" {
		t.Fatalf("LoadSkills should not keep default skill body in memory, got:\n%s", skills[0].Content)
	}
}

func TestBuildSkillsSummaryKeepsAlwaysLoadSkillsInline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLUCTIO_HOME", home)
	skillDir := filepath.Join(home, "skills", "always-inline")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: always-inline
description: Needs full instructions immediately.
---

ALWAYS_LOAD_BODY_SHOULD_APPEAR`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := NewSkillsLoaderWithGlobal(
		home,
		t.TempDir(),
		config.SkillsConfig{AlwaysLoad: []string{"always-inline"}},
		config.SkillsCfg{},
	)
	summary := loader.BuildSkillsSummary(loader.LoadSkills())

	if !strings.Contains(summary, "ALWAYS_LOAD_BODY_SHOULD_APPEAR") {
		t.Fatalf("summary should inline explicitly always-loaded skill:\n%s", summary)
	}
}

func TestGatedSkillsStayInCatalogWithUnavailableReason(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLUCTIO_HOME", home)
	skillDir := filepath.Join(home, "skills", "deepcoin-trade")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: deepcoin-trade
description: Place and manage Deepcoin orders.
metadata:
  openclaw:
    requires:
      env: ["DC_API_KEY"]
---

BODY_SHOULD_NOT_INLINE_WHEN_GATED`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := NewSkillsLoaderWithGlobal(home, t.TempDir(), config.SkillsConfig{}, config.SkillsCfg{})
	skills := loader.LoadSkills()
	if len(skills) != 1 {
		t.Fatalf("skills len = %d, want 1", len(skills))
	}
	if !skills[0].Gated {
		t.Fatalf("skill should be gated: %+v", skills[0])
	}

	summary := loader.BuildSkillsSummary(skills)
	if !strings.Contains(summary, "deepcoin-trade") {
		t.Fatalf("summary missing gated skill:\n%s", summary)
	}
	if !strings.Contains(summary, `currently unavailable: required env var "DC_API_KEY" not set`) {
		t.Fatalf("summary missing unavailable reason:\n%s", summary)
	}
	if strings.Contains(summary, "BODY_SHOULD_NOT_INLINE_WHEN_GATED") {
		t.Fatalf("summary should not inline gated skill body:\n%s", summary)
	}
}

// TestBuildSkillsSummaryOnMissingFallback asserts that a gated skill with
// an `on_missing` frontmatter hint surfaces a user-facing fallback next to
// the "currently unavailable" annotation, while gated skills without the
// hint keep the legacy single-line format. All gated skills MUST stay
// listed (policy: do not hide).
func TestBuildSkillsSummaryOnMissingFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLUCTIO_HOME", home)

	// (a) available skill — no gating.
	availDir := filepath.Join(home, "skills", "plain-skill")
	if err := os.MkdirAll(availDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(availDir, "SKILL.md"), []byte(`---
name: plain-skill
description: An available skill.
---

BODY_AVAIL
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// (b) gated skill WITHOUT on_missing — legacy format.
	gatedNoHintDir := filepath.Join(home, "skills", "gated-no-hint")
	if err := os.MkdirAll(gatedNoHintDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gatedNoHintDir, "SKILL.md"), []byte(`---
name: gated-no-hint
description: Gated skill without on_missing.
metadata:
  openclaw:
    requires:
      env: ["MISSING_ENV_A"]
---

BODY_GATED_NO_HINT
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// (c) gated skill WITH on_missing — fallback suffix.
	gatedHintDir := filepath.Join(home, "skills", "gated-with-hint")
	if err := os.MkdirAll(gatedHintDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gatedHintDir, "SKILL.md"), []byte(`---
name: gated-with-hint
description: Gated skill with on_missing.
metadata:
  openclaw:
    on_missing: use powershell
    requires:
      env: ["MISSING_ENV_B"]
---

BODY_GATED_WITH_HINT
`), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := NewSkillsLoaderWithGlobal(home, t.TempDir(), config.SkillsConfig{}, config.SkillsCfg{})
	skills := loader.LoadSkills()
	if len(skills) != 3 {
		t.Fatalf("skills len = %d, want 3", len(skills))
	}

	summary := loader.BuildSkillsSummary(skills)

	// (a) plain skill listed without unavailable annotation.
	if !strings.Contains(summary, "- plain-skill — An available skill.\n") {
		t.Fatalf("(a) plain skill missing/incorrect in summary:\n%s", summary)
	}

	// (b) gated skill without hint: listed with unavailable, NO fallback suffix, NOT hidden.
	if !strings.Contains(summary, "gated-no-hint") {
		t.Fatalf("(b) gated-no-hint should stay listed (not hidden):\n%s", summary)
	}
	if !strings.Contains(summary, `currently unavailable: required env var "MISSING_ENV_A" not set`) {
		t.Fatalf("(b) gated-no-hint missing unavailable reason:\n%s", summary)
	}
	for _, line := range strings.Split(summary, "\n") {
		if strings.Contains(line, "gated-no-hint") && strings.Contains(line, "fallback:") {
			t.Fatalf("(b) gated-no-hint line should NOT have fallback suffix, got: %q", line)
		}
	}

	// (c) gated skill with hint: listed with unavailable + fallback suffix, NOT hidden.
	if !strings.Contains(summary, "gated-with-hint") {
		t.Fatalf("(c) gated-with-hint should stay listed (not hidden):\n%s", summary)
	}
	wantC := `- gated-with-hint — Gated skill with on_missing. (currently unavailable: required env var "MISSING_ENV_B" not set) → fallback: use powershell`
	if !strings.Contains(summary, wantC) {
		t.Fatalf("(c) gated-with-hint line mismatch, want line to contain %q\nsummary:\n%s", wantC, summary)
	}

	// Bodies must never inline (gated or not, progressive disclosure).
	if strings.Contains(summary, "BODY_AVAIL") || strings.Contains(summary, "BODY_GATED_NO_HINT") || strings.Contains(summary, "BODY_GATED_WITH_HINT") {
		t.Fatalf("summary should not inline any SKILL.md body:\n%s", summary)
	}
}
