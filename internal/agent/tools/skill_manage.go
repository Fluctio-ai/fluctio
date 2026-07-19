// Package tools: skill_manage.go registers the skill_manage tool — the
// agent-facing half of the Phase 4 write-approval gate.
//
// The agent calls skill_manage when it wants to mutate a skill. Every action
// lands in <agentHome>/skills-pending/<name>/ and ONLY becomes live after the
// user runs `fluctio skill approve <name>` (Phase 4 Task 2 wires that CLI).
// Until approval the live skills/ directory is untouched, so a misbehaving
// agent cannot alter its own capabilities mid-turn.
//
// Supported actions (Hermes-compatible shape):
//
//   - create / patch / edit: write the full SKILL.md body. (edit is an alias
//     of patch for MVP — the agent passes the whole new body.)
//   - delete: stage removal of the entire live skill dir.
//   - write_file: stage a sub-file (references/templates/<path>).
//   - remove_file: stage deletion of a sub-file.
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
	Path    string `json:"path"`
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
// parser (optional) is invoked on the saved content (create/patch/edit only)
// so the tool result can surface frontmatter gating info back to the model.
// Pass nil to skip.
//
// onChange is reserved for Phase 4 hot-reload wiring; pass nil for now
// (approval happens out-of-band via the CLI, not in-process).
func RegisterSkillManage(r *Registry, agentHome, pendingHint string, parser FrontmatterParser, onChange func()) {
	if pendingHint == "" {
		pendingHint = "fluctio skill approve"
	}
	r.RegisterWithEffect("skill_manage",
		"Mutate a skill via PENDING staging (NOT live until the user runs `"+pendingHint+" <name>`). Actions: create|patch|edit (full SKILL.md body in content); delete (no body); write_file (sub-file at path, content is the file body); remove_file (sub-file path, no body). Call this proactively when ANY of these four triggers fire: (1) After a complex successful task (5+ tool calls) that's worth reusing; (2) When you hit a wall and then found the working path — capture it so future turns skip the dead ends; (3) When the user corrected your approach — encode the correction; (4) When you discovered a non-trivial workflow/technique not already in a skill.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"action": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"create", "patch", "edit", "delete", "write_file", "remove_file"},
					"description": "create = new skill; patch/edit = overwrite existing skill body; delete = remove entire skill; write_file = add/update a sub-file; remove_file = delete a sub-file",
				},
				"name": map[string]interface{}{
					"type":        "string",
					"description": "skill directory name (no path separators)",
				},
				"content": map[string]interface{}{
					"type":        "string",
					"description": "for create/patch/edit: full SKILL.md body; for write_file: the sub-file body; ignored for delete/remove_file",
				},
				"path": map[string]interface{}{
					"type":        "string",
					"description": "relative path within skill dir (e.g. templates/foo.md); required for write_file and remove_file",
				},
			},
			"required": []string{"action", "name"},
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

		// Per-action validation + staging.
		var note string
		switch args.Action {
		case "create", "patch", "edit":
			if args.Content == "" {
				return "", fmt.Errorf("skill_manage: content is required for action %q", args.Action)
			}
			meta := skills.PendingMeta{
				Source:      "skill_manage",
				CreatedAt:   time.Now().UTC(),
				Description: args.Action + " via skill_manage tool",
				Action:      mapActionConst(args.Action),
			}
			if err := skills.WritePending(agentHome, args.Name, []byte(args.Content), meta); err != nil {
				return "", fmt.Errorf("skill_manage: %w", err)
			}
			note = fmt.Sprintf("Saved skill %q body to PENDING (not live). Run `%s %s` to activate it.",
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

		case "delete":
			meta := skills.PendingMeta{
				Source:      "skill_manage",
				CreatedAt:   time.Now().UTC(),
				Description: "delete via skill_manage tool",
				Action:      skills.ActionDelete,
			}
			if err := skills.StageDeletePending(agentHome, args.Name, meta); err != nil {
				return "", fmt.Errorf("skill_manage: %w", err)
			}
			note = fmt.Sprintf("Staged DELETION of skill %q to PENDING. Run `%s %s` to execute it.",
				args.Name, pendingHint, args.Name)

		case "write_file":
			if args.Path == "" {
				return "", fmt.Errorf("skill_manage: path is required for action %q", args.Action)
			}
			if args.Content == "" {
				return "", fmt.Errorf("skill_manage: content is required for action %q", args.Action)
			}
			meta := skills.PendingMeta{
				Source:      "skill_manage",
				CreatedAt:   time.Now().UTC(),
				Description: fmt.Sprintf("write_file %s via skill_manage tool", args.Path),
				Action:      skills.ActionWriteFile,
				File:        args.Path,
			}
			if err := skills.StageFilePending(agentHome, args.Name, args.Path, []byte(args.Content), meta); err != nil {
				return "", fmt.Errorf("skill_manage: %w", err)
			}
			note = fmt.Sprintf("Staged write of %q in skill %q to PENDING. Run `%s %s` to activate it.",
				args.Path, args.Name, pendingHint, args.Name)

		case "remove_file":
			if args.Path == "" {
				return "", fmt.Errorf("skill_manage: path is required for action %q", args.Action)
			}
			meta := skills.PendingMeta{
				Source:      "skill_manage",
				CreatedAt:   time.Now().UTC(),
				Description: fmt.Sprintf("remove_file %s via skill_manage tool", args.Path),
				Action:      skills.ActionRemoveFile,
				File:        args.Path,
			}
			if err := skills.StageRemoveFilePending(agentHome, args.Name, args.Path, meta); err != nil {
				return "", fmt.Errorf("skill_manage: %w", err)
			}
			note = fmt.Sprintf("Staged removal of %q in skill %q to PENDING. Run `%s %s` to execute it.",
				args.Path, args.Name, pendingHint, args.Name)

		default:
			return "", fmt.Errorf("skill_manage: action must be create, patch, edit, delete, write_file, or remove_file, got %q", args.Action)
		}

		return note, nil
	}
}

// mapActionConst forwards the string action to the matching skills constant
// (they're identical today, but going through the constant keeps the wire
// format documented in one place).
func mapActionConst(action string) string {
	switch action {
	case "create":
		return skills.ActionCreate
	case "patch":
		return skills.ActionPatch
	case "edit":
		return skills.ActionEdit
	default:
		return action
	}
}
