// Package skills: pending.go provides a write-approval gate for agent-initiated
// skill edits. The agent's skill_manage tool writes here; nothing in this
// package touches the live skills/ directory unless ApprovePending is called.
//
// Layout under <agentHome>/skills-pending/<name>/:
//
//	SKILL.md        full skill body (frontmatter + content)
//	.pending.json   PendingMeta — origin tracking, shown by CLI list/approve
package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// validSkillName matches the same shape the install path already accepts:
// letters, digits, dots, underscores, hyphens. Path separators and the
// "..." traversal tokens are rejected by the character class alone; the
// explicit ".."/"." guard exists because the class admits '.'.
var validSkillName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// PendingMeta tracks origin of a staged skill edit. Serialised to
// .pending.json alongside the staged SKILL.md.
type PendingMeta struct {
	Source      string    `json:"source,omitempty"`      // "skill_manage" / "agent-self-edit" / "test"
	CreatedAt   time.Time `json:"created_at"`            // zero → filled by WritePending
	Description string    `json:"description,omitempty"` // human-facing note
}

// PendingEntry is one row returned by ListPending.
type PendingEntry struct {
	Name string     `json:"name"`
	Meta PendingMeta `json:"meta"`
}

// isValidSkillName rejects empty names, the reserved traversal tokens, and
// any byte outside the allowed set. The check is deliberately local (not
// exported) because callers always come through WritePending/Approve/Reject.
func isValidSkillName(name string) error {
	if name == "" {
		return fmt.Errorf("skill name is required")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("skill name %q is reserved", name)
	}
	if !validSkillName.MatchString(name) {
		return fmt.Errorf("skill name %q contains invalid characters (allowed: A-Z a-z 0-9 . _ -)", name)
	}
	return nil
}

// pendingDir returns <agentHome>/skills-pending. Use filepath.Join so the
// path works on Windows hosts (the dev environment) and Linux pods (deploy).
func pendingDir(agentHome string) string {
	return filepath.Join(agentHome, "skills-pending")
}

// WritePending stages a skill body at <agentHome>/skills-pending/<name>/SKILL.md
// (+ .pending.json meta). It does NOT touch the live skills/ directory — that's
// the write-approval gate. meta.CreatedAt is auto-filled when zero so callers
// don't have to remember to stamp it.
func WritePending(agentHome, name string, content []byte, meta PendingMeta) error {
	if err := isValidSkillName(name); err != nil {
		return err
	}
	if agentHome == "" {
		return fmt.Errorf("skills.WritePending: agentHome is required")
	}
	dir := filepath.Join(pendingDir(agentHome), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("skills.WritePending: mkdir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), content, 0o644); err != nil {
		return fmt.Errorf("skills.WritePending: write SKILL.md: %w", err)
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now().UTC()
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("skills.WritePending: marshal meta: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".pending.json"), metaBytes, 0o644); err != nil {
		return fmt.Errorf("skills.WritePending: write meta: %w", err)
	}
	return nil
}

// ApprovePending atomically moves <agentHome>/skills-pending/<name> to
// <agentHome>/skills/<name>. Returns the live path.
//
// If a live skill already exists at the destination it is replaced (the user
// has explicitly approved this edit). The .pending.json metadata file is
// dropped during the move — it has no meaning in the live tree and we don't
// want it leaking into tarballs or skill listings.
//
// "Atomic" here means "single os.Rename call after removing any existing
// destination"; on Windows, Rename to an existing dir fails, so we must
// Remove first. The tiny window between Remove and Rename is acceptable
// because approval is a user-driven CLI action, not a concurrent write path.
func ApprovePending(agentHome, name string) (livePath string, err error) {
	if err := isValidSkillName(name); err != nil {
		return "", err
	}
	if agentHome == "" {
		return "", fmt.Errorf("skills.ApprovePending: agentHome is required")
	}
	src := filepath.Join(pendingDir(agentHome), name)
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("skills.ApprovePending: no pending skill named %q", name)
		}
		return "", fmt.Errorf("skills.ApprovePending: stat: %w", err)
	}
	liveRoot := filepath.Join(agentHome, "skills")
	dst := filepath.Join(liveRoot, name)
	if err := os.MkdirAll(liveRoot, 0o755); err != nil {
		return "", fmt.Errorf("skills.ApprovePending: mkdir live: %w", err)
	}
	if _, err := os.Stat(dst); err == nil {
		// Destination exists (a live skill with the same name). Remove
		// the old tree so os.Rename succeeds. The pending entry already
		// represents the user-approved replacement, so dropping the old
		// copy here is the intended semantics, not a hazard.
		if err := os.RemoveAll(dst); err != nil {
			return "", fmt.Errorf("skills.ApprovePending: clear existing live: %w", err)
		}
	}
	if err := os.Rename(src, dst); err != nil {
		return "", fmt.Errorf("skills.ApprovePending: rename: %w", err)
	}
	// Pending-only metadata: remove from the now-live path. Best-effort
	// because a missing file is not an error condition worth failing the
	// approval for.
	_ = os.Remove(filepath.Join(dst, ".pending.json"))
	return dst, nil
}

// RejectPending removes <agentHome>/skills-pending/<name>. Returns nil if the
// entry does not exist (idempotent reject — the user-facing outcome "the
// pending edit is gone" is the same either way).
func RejectPending(agentHome, name string) error {
	if err := isValidSkillName(name); err != nil {
		return err
	}
	if agentHome == "" {
		return fmt.Errorf("skills.RejectPending: agentHome is required")
	}
	dir := filepath.Join(pendingDir(agentHome), name)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("skills.RejectPending: %w", err)
	}
	return nil
}

// ListPending returns names + meta of staged skills, sorted by name for
// stable CLI output. Returns an empty (nil) slice when the pending dir does
// not exist yet — fresh agents have no pending edits and that's not an error.
func ListPending(agentHome string) ([]PendingEntry, error) {
	if agentHome == "" {
		return nil, fmt.Errorf("skills.ListPending: agentHome is required")
	}
	dir := pendingDir(agentHome)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("skills.ListPending: %w", err)
	}
	var out []PendingEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if isValidSkillName(name) != nil {
			continue
		}
		var meta PendingMeta
		if b, err := os.ReadFile(filepath.Join(dir, name, ".pending.json")); err == nil {
			// Best-effort decode: a malformed meta file shouldn't hide
			// the skill from the listing — it just means Meta stays zero.
			_ = json.Unmarshal(b, &meta)
		}
		out = append(out, PendingEntry{Name: name, Meta: meta})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
