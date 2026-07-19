package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/skills"
)

// TestStageExtractedSkill_WritesPendingNotLive drives the small helper that
// MaybeExtract calls after the LLM produces a skill body. It must stage the
// body under <agentHome>/skills-pending/<slug>/ (NOT the live skills/ tree)
// so the background extractor goes through the same user-approval gate as
// skill_manage. Repeated stages for the same slug are a no-op so a chatty
// conversation doesn't overwrite an already-pending extraction.
func TestStageExtractedSkill_WritesPendingNotLive(t *testing.T) {
	home := t.TempDir()
	sl := &SkillsLearner{
		workspace:    home,
		agentHome:    home,
		minToolCalls: 1,
	}
	skill := &extractedSkill{
		Name:        "demo",
		Slug:        "demo",
		Description: "test skill",
		Content:     "---\nname: demo\ndescription: test\n---\nbody\n",
	}

	if err := sl.stageExtractedSkill(skill); err != nil {
		t.Fatalf("stageExtractedSkill: %v", err)
	}

	// Live skills tree must NOT be touched.
	livePath := filepath.Join(home, "skills", "demo", "SKILL.md")
	if _, err := os.Stat(livePath); !os.IsNotExist(err) {
		t.Fatalf("live SKILL.md should not exist, got err=%v", err)
	}

	// Pending tree must hold the staged body.
	pendingPath := filepath.Join(home, "skills-pending", "demo", "SKILL.md")
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("pending SKILL.md not written: %v", err)
	}

	// Meta must point to skills_learner so the CLI can distinguish the source.
	entries, err := skills.ListPending(home)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "demo" {
		t.Fatalf("ListPending = %+v, want one entry 'demo'", entries)
	}
	if entries[0].Meta.Source != "skills_learner" {
		t.Fatalf("Source = %q, want skills_learner", entries[0].Meta.Source)
	}
	if entries[0].Meta.Action != skills.ActionCreate {
		t.Fatalf("Action = %q, want %q", entries[0].Meta.Action, skills.ActionCreate)
	}

	// Re-staging the same slug must be a no-op (already-pending sentinel).
	if err := sl.stageExtractedSkill(skill); !errors.Is(err, errSkillAlreadyPending) {
		t.Fatalf("second stage err = %v, want errSkillAlreadyPending", err)
	}

	// And the pending body must still match the first write (not overwritten
	// by a hypothetical second run).
	got, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatalf("read pending: %v", err)
	}
	if string(got) != skill.Content {
		t.Fatalf("pending body changed: got %q, want %q", string(got), skill.Content)
	}
}

// TestStageExtractedSkill_RejectsInvalidSlug covers the early-validation
// path: an LLM that returns a malformed slug (path separator, traversal
// token, space, etc.) must be rejected at stageExtractedSkill BEFORE any
// filesystem write is attempted under skills-pending/. Without this guard
// the failure surfaces as a wrapped skills.WritePending error from inside
// the skills package, which is harder to attribute to the LLM extraction.
func TestStageExtractedSkill_RejectsInvalidSlug(t *testing.T) {
	home := t.TempDir()
	sl := &SkillsLearner{
		workspace:    home,
		agentHome:    home,
		minToolCalls: 1,
	}
	for _, bad := range []string{"", "..", ".", "foo/bar", "foo\\bar", "../escape", "a b", "a:b"} {
		skill := &extractedSkill{
			Name:    "demo",
			Slug:    bad,
			Content: "---\nname: demo\n---\nbody\n",
		}
		err := sl.stageExtractedSkill(skill)
		if err == nil {
			t.Fatalf("stageExtractedSkill accepted invalid slug %q", bad)
		}
		if !strings.Contains(err.Error(), "invalid slug") {
			t.Fatalf("slug %q: error %v should mention 'invalid slug'", bad, err)
		}
	}
	// Nothing should have been written to pending for any of the bad slugs.
	entries, err := skills.ListPending(home)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected zero pending entries, got %d", len(entries))
	}
}
