// Package tools: skill_manage.go registers the skill_manage tool — the
// agent-facing half of the Phase 4 write-approval gate.
//
// The agent calls skill_manage when it wants to create or patch a skill.
// The body lands in <agentHome>/skills-pending/<name>/ and ONLY becomes live
// after the user runs `fluctio skill approve <name>` (Phase 4 Task 2 wires
// that CLI). Until approval the live skills/ directory is untouched, so a
// misbehaving agent cannot alter its own capabilities mid-turn.
//
// The parser closure is constructed in package agent (which owns the real
// parseFrontmatterFromBytes + CheckGating) and injected here so skill_manage
// can surface gating info back to the model without creating an import cycle
// (tools → skills is fine; tools → agent is not). This is the same SkillGate
// pattern used by load_skill.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fluctio-ai/fluctio/internal/skills"
)

// SkillManifest is the tools-side view of a parsed SKILL.md frontmatter.
// Package agent constructs it from parseFrontmatterFromBytes + CheckGating
// and passes it to RegisterSkillManage so skill_manage can echo gating state
// to the model without creating an import cycle.
type SkillManifest struct {
	Name        string
	Description string
	Gated       bool
	GateReason  string
}

// FrontmatterParser parses raw SKILL.md bytes into a SkillManifest.
// Returns nil if the bytes are not valid frontmatter. Implementation lives
// in package agent; this type is just the callable signature.
type FrontmatterParser func([]byte) *SkillManifest

type skillManageArgs struct {
	Action  string `json:"action"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

// RegisterSkillManage registers the skill_manage tool.
//
// agentHome is the agent's home directory (the parent of skills/ and the
// new skills-pending/). The same value passed to RegisterLoadSkill's
// skillDirs is correct.
//
// pendingHint is the CLI command string echoed back in the tool result so
// the model can tell the user how to approve (e.g. "fluctio skill approve").
//
// parser (optional) is invoked on the saved content so the tool result can
// surface frontmatter gating info back to the model. Pass nil to skip.
//
// onChange is reserved for Phase 4 hot-reload wiring; pass nil for now
// (approval happens out-of-band via the CLI, not in-process).
func RegisterSkillManage(r *Registry, agentHome, pendingHint string, parser FrontmatterParser, onChange func()) {
	if pendingHint == "" {
		pendingHint = "fluctio skill approve"
	}
	r.RegisterWithEffect("skill_manage",
		"Create or patch a skill (writes to a PENDING dir — NOT live until the user runs `"+pendingHint+" <name>`). Use after completing a non-trivial workflow or finding a working path worth reusing. action: create|patch; content is the full SKILL.md body including --- frontmatter ---.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"action": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"create", "patch"},
					"description": "create = new skill; patch = overwrite existing skill body",
				},
				"name": map[string]interface{}{
					"type":        "string",
					"description": "skill directory name (no path separators)",
				},
				"content": map[string]interface{}{
					"type":        "string",
					"description": "full SKILL.md body including --- frontmatter ---",
				},
			},
			"required": []string{"action", "name", "content"},
		},
		makeSkillManage(agentHome, pendingHint, parser), SideWritesFile)
}

// makeSkillManage returns the ToolFunc for the skill_manage tool. Separated
// from Register so the closure captures only the three values it needs
// (agentHome, pendingHint, parser) and not the whole Registry.
func makeSkillManage(agentHome, pendingHint string, parser FrontmatterParser) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args skillManageArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.Name == "" {
			return "", fmt.Errorf("skill_manage: name is required")
		}
		if args.Content == "" {
			return "", fmt.Errorf("skill_manage: content is required")
		}
		switch args.Action {
		case "create", "patch":
		default:
			return "", fmt.Errorf("skill_manage: action must be create or patch, got %q", args.Action)
		}

		meta := skills.PendingMeta{
			Source:      "skill_manage",
			CreatedAt:   time.Now().UTC(),
			Description: args.Action + " via skill_manage tool",
		}
		if err := skills.WritePending(agentHome, args.Name, []byte(args.Content), meta); err != nil {
			return "", fmt.Errorf("skill_manage: %w", err)
		}

		note := fmt.Sprintf("Saved skill %q to PENDING (not live). The user must run `%s %s` to activate it.",
			args.Name, pendingHint, args.Name)
		// Parser is best-effort: a nil parser or malformed frontmatter
		// must not block the save, only suppress the extra note.
		if parser != nil {
			if m := parser([]byte(args.Content)); m != nil {
				if m.Description != "" {
					note += fmt.Sprintf(" Parsed frontmatter: name=%q description=%q.", m.Name, m.Description)
				}
				if m.Gated {
					note += fmt.Sprintf(" Heads-up: this skill's frontmatter requirements are not met on this host — %s. The skill will be listed but gated until the requirement is satisfied.", m.GateReason)
				}
			}
		}
		return note, nil
	}
}
