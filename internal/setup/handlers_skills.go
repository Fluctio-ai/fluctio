package setup

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/agent"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/skills"
)

// --- Skills ---

func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	homeDir, err := config.HomeDir()
	if err != nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}

	skillsDir := filepath.Join(homeDir, "skills")
	// Hydrate from object store first so pods that didn't handle the
	// original install still see the skill bundle. Pass the bundled skill
	// names as the keep-local list so an empty OSS response never causes
	// us to prune builtin skills. No-op when no object store is configured
	// (local mode) or nothing is mirrored yet.
	if s.workspaceStore != nil {
		if err := skills.HydrateSkillsDown(
			r.Context(), s.workspaceStore, skills.GlobalSkillOwner, skillsDir,
			agent.BundledSkillNames()...,
		); err != nil {
			slog.Warn("failed to hydrate global skills from object store", "error", err)
		}
	}
	out := scanSkillsDir(skillsDir)
	if out == nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s *Server) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	homeDir, err := config.HomeDir()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	skillPath := filepath.Join(homeDir, "skills", name)
	if err := os.RemoveAll(skillPath); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// Also remove from the object store so other pods drop it on their
	// next hydrate. Failure here shouldn't fail the delete — the local
	// copy is gone already and a stale remote copy will just re-appear
	// next reload (annoying but not dangerous).
	if s.workspaceStore != nil {
		if derr := skills.DeleteSkillUp(r.Context(), s.workspaceStore, skills.GlobalSkillOwner, name); derr != nil {
			slog.Warn("failed to remove global skill from object store", "skill", name, "error", derr)
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleGetSkillManifest returns the raw SKILL.md content of a global
// skill so the admin UI can preview what a skill instructs the agent to
// do before trusting it. Login-gated like the list endpoint — env
// values aren't included, only the manifest the loader parses anyway.
func (s *Server) handleGetSkillManifest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// The route pattern already constrains name to a single path segment;
	// this guards against %-encoding tricks smuggling a traversal.
	if name == "" || filepath.Base(filepath.Clean(name)) != name {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid skill name"})
		return
	}
	homeDir, err := config.HomeDir()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	data, err := os.ReadFile(filepath.Join(homeDir, "skills", name, "SKILL.md"))
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "skill manifest not found"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"name": name, "content": string(data)})
}

// handleGetAgentSkillManifest mirrors handleGetSkillManifest for a
// skill installed in the agent's own home dir (~/.fluctio/agents/<id>/
// skills/<name>/SKILL.md). Owner-gated like the other agent-skill
// endpoints.
func (s *Server) handleGetAgentSkillManifest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	if name == "" || filepath.Base(filepath.Clean(name)) != name {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid skill name"})
		return
	}
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	homePath, err := config.AgentHomeDir(id)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	data, err := os.ReadFile(filepath.Join(homePath, "skills", name, "SKILL.md"))
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "skill manifest not found"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"name": name, "content": string(data)})
}

// handleListAgentSkills lists skills installed into an agent's own home
// directory (~/.fluctio/agents/<id>/skills/). Loader "Layer 1" picks
// these up at the highest precedence — they're exclusive to the agent.
func (s *Server) handleListAgentSkills(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Skill listing exposes per-skill env spec (which env keys the
	// owner has set). Owner-only — Identity.CanAccessAgent is a
	// deferred-true for session callers and would let any signed-in
	// user enumerate any agent's skills.
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	homePath, err := config.AgentHomeDir(id)
	if err != nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}
	skillsDir := filepath.Join(homePath, "skills")
	// Hydrate this agent's skills from object store on demand so replica
	// pods that haven't yet cached the bundle still list it in the UI.
	if s.workspaceStore != nil {
		if err := skills.HydrateSkillsDown(r.Context(), s.workspaceStore, id, skillsDir); err != nil {
			slog.Warn("failed to hydrate agent skills from object store",
				"agent", id, "error", err)
		}
	}
	out := scanSkillsDir(skillsDir)
	if out == nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

// scanSkillsDir reads every SKILL.md under dir and returns the list of
// {name, description, location, type, envSpec?} entries the admin UI
// renders. Shared between the global /api/skills and the per-agent
// /api/agents/{id}/skills paths so frontmatter parsing (description,
// envSpec) stays consistent — earlier the two handlers drifted, with
// the agent-scoped one falling back to "first non-# line" which then
// surfaced the literal `---` frontmatter delimiter as the description.
func scanSkillsDir(dir string) []map[string]any {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		skillPath := filepath.Join(dir, name, "SKILL.md")
		desc := ""
		var envSpec []agent.SkillEnvSpec
		if data, readErr := os.ReadFile(skillPath); readErr == nil {
			fm, body := agent.SplitSkillFrontmatter(data)
			if fm != nil {
				if fm.Description != "" {
					desc = fm.Description
				}
				// Top-level `env:` shortcut wins; fall back to the
				// namespaced metadata.fluctio|openclaw.env form.
				if len(fm.Env) > 0 {
					envSpec = fm.Env
				} else if meta := agent.ParseSkillMetadata(&fm.Metadata); meta != nil && meta.Meta() != nil {
					envSpec = meta.Meta().Env
				}
			}
			if desc == "" {
				for _, line := range strings.SplitN(body, "\n", 5) {
					line = strings.TrimSpace(line)
					if line != "" && !strings.HasPrefix(line, "#") {
						desc = line
						break
					}
				}
			}
		}
		entryOut := map[string]any{
			"name":        name,
			"description": desc,
			"location":    filepath.Join(dir, name),
			"type":        "skill",
		}
		if len(envSpec) > 0 {
			entryOut["envSpec"] = envSpec
		}
		out = append(out, entryOut)
	}
	return out
}

// handleDeleteAgentSkill removes a skill from an agent's own home dir
// only. Global/shared skills are untouched.
func (s *Server) handleDeleteAgentSkill(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	name := r.PathValue("name")
	// Mutation — owner-only. Identity.CanAccessAgent is a
	// deferred-true for session callers and would let anyone delete
	// skills off any agent.
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	homePath, err := config.AgentHomeDir(id)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	skillPath := filepath.Join(homePath, "skills", name)
	if err := os.RemoveAll(skillPath); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// Delete from object store so other pods drop it on their next hydrate.
	if s.workspaceStore != nil {
		if derr := skills.DeleteSkillUp(r.Context(), s.workspaceStore, id, name); derr != nil {
			slog.Warn("failed to remove agent skill from object store",
				"agent", id, "skill", name, "error", derr)
		}
	}
	// Hot-reload the agent so the removed skill drops out of its context.
	if ag := s.resolveAgent(r, id); ag != nil {
		ag.ReloadWorkspaceFiles()
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleReloadAgentSkills triggers a hot-reload of the agent's workspace
// files (skills + identity .md) so edits made outside the gateway — e.g.
// `fluctio skill approve` moving a skill from skills-pending/ to skills/
// on disk — take effect on the next turn without restarting the process.
// Mirrors the embedded ReloadWorkspaceFiles call already inside
// handleDeleteAgentSkill / handleInstallSkill, exposed as its own endpoint
// so external scripts (and the CLI) can request a reload after a file-level
// mutation. Owner-only: Identity.CanAccessAgent is a deferred-true for
// session callers, and triggering a reload on someone else's agent is a
// noisy primitive even though it's not strictly a privilege escalation.
func (s *Server) handleReloadAgentSkills(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	if ag := s.resolveAgent(r, id); ag != nil {
		ag.ReloadWorkspaceFiles()
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not running"})
}
