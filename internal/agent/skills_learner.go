package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/llmjson"
	"github.com/fluctio-ai/fluctio/internal/privacy"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/skills"
)

// errSkillAlreadyPending signals that a skill with the same slug is already
// staged in <agentHome>/skills-pending/. MaybeExtract treats this as a silent
// skip — a chatty conversation that produces the same extraction repeatedly
// must not overwrite an already-staged pending entry.
var errSkillAlreadyPending = errors.New("skill already pending")

// SkillsLearner observes complex tasks and extracts reusable skill patterns.
//
// workspace is the live skills tree root used only for the "already exists?"
// pre-check (extracted skills whose slug already lives in <workspace>/skills
// are not re-staged). agentHome is where the pending approval queue lives
// (<agentHome>/skills-pending/); the two are usually the same directory in
// practice (rc.Home) but kept separate so the existence check and the write
// target are conceptually distinct.
type SkillsLearner struct {
	workspace    string
	agentHome    string
	provider     provider.Provider
	model        string
	minToolCalls int      // minimum tool calls to consider extracting (default: 3)
	skillDirs    []string // directories to search for the skill-learner skill
	piiScrub     bool     // PII-scrub the extraction transcript before it leaves
	piiEntropy   bool
}

// NewSkillsLearner creates a new SkillsLearner. agentHome is the directory
// under which skills-pending/ lives (the approval gate target). workspace is
// the live skills tree root for the "already exists?" pre-check; pass the
// same value when the caller doesn't distinguish.
func NewSkillsLearner(workspace, agentHome string, p provider.Provider, model string, skillDirs ...string) *SkillsLearner {
	return &SkillsLearner{
		workspace:    workspace,
		agentHome:    agentHome,
		provider:     p,
		model:        model,
		minToolCalls: 3,
		skillDirs:    skillDirs,
	}
}

// SetPIIScrub arms the PII scrubbing for the extraction LLM call. The
// transcript embeds chatter conversation content, so without this the
// privacy.piiScrubbing switch was bypassed by skill extraction.
func (sl *SkillsLearner) SetPIIScrub(enabled, entropy bool) {
	sl.piiScrub = enabled
	sl.piiEntropy = entropy
}

type extractedSkill struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

type extractionResponse struct {
	Extract bool           `json:"extract"`
	Skill   extractedSkill `json:"skill"`
}

// MaybeExtract checks if the conversation warrants skill extraction.
// Called after agent turns complete. The extracted skill is staged to
// <agentHome>/skills-pending/<slug>/ — NOT the live skills/ tree — so the
// background extractor honours the same user-approval gate as skill_manage.
// Activate via `fluctio skill approve <slug>`.
func (sl *SkillsLearner) MaybeExtract(ctx context.Context, messages []provider.Message, toolCallCount int) error {
	if toolCallCount < sl.minToolCalls {
		return nil
	}

	skill, err := sl.extractSkill(ctx, messages)
	if err != nil {
		return fmt.Errorf("extract skill: %w", err)
	}
	if skill == nil {
		return nil
	}

	// Skip if the skill already lives in the live tree. The pending write
	// itself also guards against re-staging (stageExtractedSkill returns
	// errSkillAlreadyPending), so this check is a fast-path stat before
	// hitting the LLM-staged body.
	skillDir := filepath.Join(sl.workspace, "skills", skill.Slug)
	if _, err := os.Stat(filepath.Join(skillDir, "SKILL.md")); err == nil {
		slog.Debug("skill already exists, skipping", "slug", skill.Slug)
		return nil
	}

	if err := sl.stageExtractedSkill(skill); err != nil {
		if errors.Is(err, errSkillAlreadyPending) {
			slog.Debug("skill already pending, skipping", "slug", skill.Slug)
			return nil
		}
		return fmt.Errorf("stage skill: %w", err)
	}

	slog.Info("staged skill to PENDING",
		"name", skill.Name, "slug", skill.Slug,
		"hint", fmt.Sprintf("run `fluctio skill approve %s` to activate", skill.Slug))
	return nil
}

// stageExtractedSkill writes the LLM-produced SKILL.md body to the pending
// approval queue at <agentHome>/skills-pending/<slug>/SKILL.md (plus
// .pending.json metadata with Source="skills_learner" and Action="create").
// It does NOT touch the live skills/ directory — approval is a separate
// user-driven step (`fluctio skill approve <slug>`).
//
// Returns errSkillAlreadyPending without writing if a pending entry with the
// same slug already exists, so repeated extraction attempts on similar
// conversations don't clobber an earlier stage.
func (sl *SkillsLearner) stageExtractedSkill(skill *extractedSkill) error {
	if sl.agentHome == "" {
		return fmt.Errorf("stageExtractedSkill: agentHome is required")
	}
	// Validate the LLM-extracted slug BEFORE touching the filesystem so a
	// malformed extraction surfaces as a clear "invalid slug" rejection
	// (and MaybeExtract logs "skipped extracted skill, invalid slug") rather
	// than a wrapped WritePending error from inside the skills package.
	if err := skills.IsValidSkillName(skill.Slug); err != nil {
		return fmt.Errorf("stageExtractedSkill: invalid slug %q: %w", skill.Slug, err)
	}
	pendingSkillPath := filepath.Join(sl.agentHome, "skills-pending", skill.Slug, "SKILL.md")
	if _, err := os.Stat(pendingSkillPath); err == nil {
		return errSkillAlreadyPending
	}
	meta := skills.PendingMeta{
		Source:      "skills_learner",
		Description: skill.Description,
		Action:      skills.ActionCreate,
	}
	if err := skills.WritePending(sl.agentHome, skill.Slug, []byte(skill.Content), meta); err != nil {
		return err
	}
	return nil
}

// loadSkillLearnerPrompt loads the skill-learner SKILL.md from disk.
// Falls back to a minimal built-in prompt if not found.
func (sl *SkillsLearner) loadSkillLearnerPrompt() string {
	// Search skill directories for skill-learner SKILL.md
	for _, dir := range sl.skillDirs {
		path := filepath.Join(dir, "fluctio-skill-learner", "SKILL.md")
		if data, err := os.ReadFile(path); err == nil {
			slog.Debug("loaded skill-learner prompt from file", "path", path)
			return string(data)
		}
	}

	// Fallback: minimal built-in prompt
	return fallbackExtractionPrompt
}

const fallbackExtractionPrompt = `Analyze the following conversation and determine if it demonstrates a reusable multi-step skill.

Criteria for extraction:
- The task involved 3+ tool calls in a clear, repeatable sequence
- The task is general enough to be useful in other contexts
- The steps can be described as a clear procedure

If this conversation demonstrates a reusable skill, output JSON:
{"extract": true, "skill": {"name": "Human readable name", "slug": "kebab-case-slug", "description": "One line description", "content": "Full SKILL.md content with YAML frontmatter"}}

If not reusable, output: {"extract": false}

The SKILL.md format uses YAML frontmatter:
---
name: Skill Name
description: What it does
---
Step-by-step instructions in markdown...

Output ONLY the JSON, no markdown fences.`

// extractSkill uses LLM to generate a SKILL.md from the conversation.
func (sl *SkillsLearner) extractSkill(ctx context.Context, messages []provider.Message) (*extractedSkill, error) {
	// Build a summary of the conversation for the extraction prompt
	var sb strings.Builder
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		content := m.Content
		if len(content) > 500 {
			content = content[:500] + "..."
		}
		sb.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, content))
		for _, tc := range m.ToolCalls {
			sb.WriteString(fmt.Sprintf("  -> tool: %s(%s)\n", tc.Function.Name, truncate(tc.Function.Arguments, 200)))
		}
	}

	prompt := sl.loadSkillLearnerPrompt()

	extractMsgs := []provider.Message{
		{Role: "system", Content: prompt + "\n\nOutput ONLY the JSON, no markdown fences."},
		{Role: "user", Content: sb.String()},
	}

	// The transcript embeds chatter conversation content — scrub it the
	// same way the interactive loop does when the switch is on.
	if sl.piiScrub {
		extractMsgs = privacy.ScrubMessages(extractMsgs, privacy.Options{Entropy: sl.piiEntropy})
	}

	resp, err := sl.provider.Chat(provider.WithJSONMode(provider.WithNoThinking(ctx)), extractMsgs, nil, sl.model, 1024, 0.3)
	if err != nil {
		return nil, err
	}

	var result extractionResponse
	// Try to parse response as JSON
	content := strings.TrimSpace(resp.Content)
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		// Bare quotes inside values (models ignoring the JSON-mode hint) —
		// repair once and retry. Failure here stays a silent skip: the
		// learner is opportunistic by design.
		if json.Unmarshal([]byte(llmjson.RepairUnescapedQuotes(content)), &result) != nil {
			slog.Debug("skill extraction: LLM response not valid JSON", "error", err)
			return nil, nil
		}
	}

	if !result.Extract || result.Skill.Slug == "" || result.Skill.Content == "" {
		return nil, nil
	}

	return &result.Skill, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
