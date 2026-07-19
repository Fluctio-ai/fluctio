// Package skills: pending.go provides a write-approval gate for agent-initiated
// skill edits. The agent's skill_manage tool writes here; nothing in this
// package touches the live skills/ directory unless ApprovePending is called.
//
// Layout under <agentHome>/skills-pending/<name>/:
//
//	SKILL.md        full skill body (frontmatter + content) — present for
//	                create/patch/edit actions; absent for delete/write_file/
//	                remove_file (the latter two carry the sub-file instead).
//	<payload>       sub-file at the staged relpath for write_file actions.
//	.pending.json   PendingMeta — origin + action tracking, shown by CLI.
package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// validSkillName matches the same shape the install path already accepts:
// letters, digits, dots, underscores, hyphens. Path separators and the
// "..." traversal tokens are rejected by the character class alone; the
// explicit ".."/"." guard exists because the class admits '.'.
var validSkillName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// safeRelativePathChars is the allowed character set for write_file/
// remove_file relpaths: word chars, dot, underscore, hyphen, forward slash.
// Backslash is excluded so Windows-style separators can't bypass the ".."
// segment check after a ToSlash normalisation; drive letters (':') and
// spaces are rejected too.
var safeRelativePathChars = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// PendingAction constants document the values stored in PendingMeta.Action.
// They're string aliases — kept untyped so serialisation stays JSON-friendly
// and the agent tool can pass them as bare strings without import.
const (
	ActionCreate     = "create"      // write full SKILL.md body (new skill)
	ActionPatch      = "patch"       // overwrite existing SKILL.md body
	ActionEdit       = "edit"        // alias of patch for MVP (whole-body overwrite)
	ActionDelete     = "delete"      // remove the entire live skill dir
	ActionWriteFile  = "write_file"  // write a sub-file (templates/refs/...)
	ActionRemoveFile = "remove_file" // delete a sub-file from live skill dir
)

// PendingMeta tracks origin of a staged skill edit. Serialised to
// .pending.json alongside the staged payload.
type PendingMeta struct {
	Source      string    `json:"source,omitempty"`      // "skill_manage" / "agent-self-edit" / "test"
	CreatedAt   time.Time `json:"created_at"`            // zero → filled by WritePending
	Description string    `json:"description,omitempty"` // human-facing note
	// Action records what ApprovePending should do with this staged entry.
	// Empty defaults to "create" for backward compatibility with entries
	// written before multi-action support landed.
	Action string `json:"action,omitempty"`
	// File is the relative path of the sub-file targeted by write_file
	// and remove_file actions. Empty for create/patch/edit/delete.
	File string `json:"file,omitempty"`
}

// PendingEntry is one row returned by ListPending.
type PendingEntry struct {
	Name string     `json:"name"`
	Meta PendingMeta `json:"meta"`
}

// IsValidSkillName rejects empty names, the reserved traversal tokens, and
// any byte outside the allowed set. Exported so out-of-package callers
// (e.g. the skills_learner) can pre-validate an LLM-supplied slug BEFORE
// constructing a WritePending call, surfacing the failure with a clearer
// "invalid slug" message instead of a wrapped WritePending error.
func IsValidSkillName(name string) error {
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

// isValidSkillName is an internal alias kept so existing in-package callers
// read naturally. New internal code should call IsValidSkillName directly.
func isValidSkillName(name string) error { return IsValidSkillName(name) }

// isSafeRelativePath guards relpaths used by write_file/remove_file. It
// rejects absolute paths (Unix or Windows drive-letter form), backslashes,
// empty segments, and any traversal ("..") or lone "." segment. Allowed
// chars: A-Z a-z 0-9 . _ - /. The agent will typically pass paths like
// "templates/foo.md" or "refs/bar.json".
//
// The check is platform-aware via filepath.IsAbs and ToSlash so the same
// rule applies on Windows dev hosts and Linux deploy pods.
func isSafeRelativePath(p string) error {
	if p == "" {
		return fmt.Errorf("path is required")
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("path %q must be relative", p)
	}
	// Reject Windows drive prefixes ("C:..." or "c:\\...") — filepath.IsAbs
	// on Windows catches these, but on Linux it doesn't, so enforce here.
	if len(p) >= 2 && p[1] == ':' {
		return fmt.Errorf("path %q must not contain a drive letter", p)
	}
	if !safeRelativePathChars.MatchString(p) {
		return fmt.Errorf("path %q contains invalid characters (allowed: A-Z a-z 0-9 . _ / -)", p)
	}
	cleaned := filepath.ToSlash(p)
	segments := strings.Split(cleaned, "/")
	for _, seg := range segments {
		if seg == "" {
			return fmt.Errorf("path %q must not contain empty segments", p)
		}
		if seg == ".." {
			return fmt.Errorf("path %q must not contain '..' segment", p)
		}
		if seg == "." {
			return fmt.Errorf("path %q must not contain '.' segment", p)
		}
	}
	return nil
}

// pendingDir returns <agentHome>/skills-pending. Use filepath.Join so the
// path works on Windows hosts (the dev environment) and Linux pods (deploy).
func pendingDir(agentHome string) string {
	return filepath.Join(agentHome, "skills-pending")
}

// writePendingMeta writes .pending.json inside dir, auto-stamping CreatedAt
// when zero. Shared by every stager.
func writePendingMeta(dir string, meta PendingMeta) error {
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now().UTC()
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("skills: marshal meta: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".pending.json"), metaBytes, 0o644); err != nil {
		return fmt.Errorf("skills: write meta: %w", err)
	}
	return nil
}

// WritePending stages a skill body at <agentHome>/skills-pending/<name>/SKILL.md
// (+ .pending.json meta). It does NOT touch the live skills/ directory — that's
// the write-approval gate. meta.CreatedAt is auto-filled when zero so callers
// don't have to remember to stamp it. meta.Action is defaulted to "create"
// when empty so older callers (post-MVP create/patch path) get the right
// semantics from ApprovePending without knowing about the new field.
func WritePending(agentHome, name string, content []byte, meta PendingMeta) error {
	if err := isValidSkillName(name); err != nil {
		return err
	}
	if agentHome == "" {
		return fmt.Errorf("skills.WritePending: agentHome is required")
	}
	if meta.Action == "" {
		meta.Action = ActionCreate
	}
	dir := filepath.Join(pendingDir(agentHome), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("skills.WritePending: mkdir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), content, 0o644); err != nil {
		return fmt.Errorf("skills.WritePending: write SKILL.md: %w", err)
	}
	if err := writePendingMeta(dir, meta); err != nil {
		return err
	}
	return nil
}

// StageDeletePending records a deletion intent at
// <agentHome>/skills-pending/<name>/.pending.json. No SKILL.md is written —
// ApprovePending will os.RemoveAll the live skill dir.
func StageDeletePending(agentHome, name string, meta PendingMeta) error {
	if err := isValidSkillName(name); err != nil {
		return err
	}
	if agentHome == "" {
		return fmt.Errorf("skills.StageDeletePending: agentHome is required")
	}
	meta.Action = ActionDelete
	meta.File = ""
	dir := filepath.Join(pendingDir(agentHome), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("skills.StageDeletePending: mkdir: %w", err)
	}
	if err := writePendingMeta(dir, meta); err != nil {
		return err
	}
	return nil
}

// StageFilePending writes a sub-file (e.g. templates/foo.md) at
// <agentHome>/skills-pending/<name>/<relPath> plus .pending.json describing
// the write_file action. ApprovePending will ensure the live skill dir
// exists and copy the sub-file in.
func StageFilePending(agentHome, name, relPath string, content []byte, meta PendingMeta) error {
	if err := isValidSkillName(name); err != nil {
		return err
	}
	if err := isSafeRelativePath(relPath); err != nil {
		return fmt.Errorf("skills.StageFilePending: %w", err)
	}
	if agentHome == "" {
		return fmt.Errorf("skills.StageFilePending: agentHome is required")
	}
	meta.Action = ActionWriteFile
	meta.File = relPath
	dir := filepath.Join(pendingDir(agentHome), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("skills.StageFilePending: mkdir: %w", err)
	}
	// Use FromSlash so Windows hosts honor forward-slash relpaths the same
	// way Linux does — the agent speaks forward-slash.
	target := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("skills.StageFilePending: mkdir sub: %w", err)
	}
	if err := os.WriteFile(target, content, 0o644); err != nil {
		return fmt.Errorf("skills.StageFilePending: write sub-file: %w", err)
	}
	if err := writePendingMeta(dir, meta); err != nil {
		return err
	}
	return nil
}

// StageRemoveFilePending records an intent to delete a sub-file from a live
// skill dir. ApprovePending will os.Remove that file (no error if it's
// already gone — idempotent removal).
func StageRemoveFilePending(agentHome, name, relPath string, meta PendingMeta) error {
	if err := isValidSkillName(name); err != nil {
		return err
	}
	if err := isSafeRelativePath(relPath); err != nil {
		return fmt.Errorf("skills.StageRemoveFilePending: %w", err)
	}
	if agentHome == "" {
		return fmt.Errorf("skills.StageRemoveFilePending: agentHome is required")
	}
	meta.Action = ActionRemoveFile
	meta.File = relPath
	dir := filepath.Join(pendingDir(agentHome), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("skills.StageRemoveFilePending: mkdir: %w", err)
	}
	if err := writePendingMeta(dir, meta); err != nil {
		return err
	}
	return nil
}

// ApprovePending applies the staged action to the live skill tree and
// cleans up the pending entry. The dispatch reads .pending.json to choose
// between:
//
//   - create / patch / edit (default for old entries): atomically move the
//     pending dir to <agentHome>/skills/<name>, replacing any existing live
//     skill. The .pending.json file is dropped from the live tree.
//   - delete: os.RemoveAll the live skill dir.
//   - write_file: ensure the live skill dir exists, copy the staged sub-file
//     to <live>/<name>/<relPath>. Other files in the live skill survive.
//   - remove_file: os.Remove <live>/<name>/<relPath> (idempotent on missing).
//
// In every case the pending dir is removed at the end. Returns the canonical
// live path for the skill (<agentHome>/skills/<name>).
//
// "Atomic" here means "single os.Rename call after removing any existing
// destination" for the create/patch/edit path; the file-level actions aren't
// transactional but they're each a single filesystem op, and approval is a
// user-driven CLI action, not a concurrent write path.
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
	// Read action + relPath from meta. Old entries (no Action field) default
	// to create so the original rename semantics are preserved.
	action := ActionCreate
	file := ""
	if metaBytes, rerr := os.ReadFile(filepath.Join(src, ".pending.json")); rerr == nil {
		var m PendingMeta
		if jerr := json.Unmarshal(metaBytes, &m); jerr == nil {
			if m.Action != "" {
				action = m.Action
			}
			file = m.File
		}
	}

	liveRoot := filepath.Join(agentHome, "skills")
	dst := filepath.Join(liveRoot, name)

	switch action {
	case ActionCreate, ActionPatch, ActionEdit:
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

	case ActionDelete:
		// Remove the live skill tree if present. RemoveAll returns nil for
		// a missing path, so approving a delete on a skill that was never
		// live is still success (matches "the user wants it gone").
		if err := os.RemoveAll(dst); err != nil {
			return "", fmt.Errorf("skills.ApprovePending: delete live: %w", err)
		}
		// Clean up the pending entry.
		_ = os.RemoveAll(src)
		return dst, nil

	case ActionWriteFile:
		if file == "" {
			return "", fmt.Errorf("skills.ApprovePending: write_file pending entry %q missing File", name)
		}
		// Re-sanitize defensively in case the on-disk meta was hand-edited.
		if err := isSafeRelativePath(file); err != nil {
			return "", fmt.Errorf("skills.ApprovePending: %w", err)
		}
		srcFile := filepath.Join(src, filepath.FromSlash(file))
		data, rerr := os.ReadFile(srcFile)
		if rerr != nil {
			return "", fmt.Errorf("skills.ApprovePending: read staged file: %w", rerr)
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return "", fmt.Errorf("skills.ApprovePending: mkdir live skill: %w", err)
		}
		dstFile := filepath.Join(dst, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(dstFile), 0o755); err != nil {
			return "", fmt.Errorf("skills.ApprovePending: mkdir live sub: %w", err)
		}
		if err := os.WriteFile(dstFile, data, 0o644); err != nil {
			return "", fmt.Errorf("skills.ApprovePending: write live file: %w", err)
		}
		_ = os.RemoveAll(src)
		return dst, nil

	case ActionRemoveFile:
		if file == "" {
			return "", fmt.Errorf("skills.ApprovePending: remove_file pending entry %q missing File", name)
		}
		if err := isSafeRelativePath(file); err != nil {
			return "", fmt.Errorf("skills.ApprovePending: %w", err)
		}
		target := filepath.Join(dst, filepath.FromSlash(file))
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("skills.ApprovePending: remove live file: %w", err)
		}
		_ = os.RemoveAll(src)
		return dst, nil

	default:
		return "", fmt.Errorf("skills.ApprovePending: unknown action %q on pending entry %q", action, name)
	}
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
