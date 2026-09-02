package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/workspace"
)

// newScopedRegistry builds a registry backed by a LocalFS workspace store
// in a temp dir — the shape the gateway wires in production.
func newScopedRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	root := t.TempDir()
	r := NewRegistry(t.TempDir(), t.TempDir())
	r.SetWorkspaceStore(workspace.NewLocalFS(root), "agt_test")
	return r, root
}

// TestWsScopeProjectChatSessionFirst pins the session-first layout for
// ordinary project chats: bare paths land in the chat's own session dir,
// exactly one "../" reaches the project-shared root, and anything that
// would widen further (a second "../", or another chat's session dir) is
// rejected. This is the layout handleChatTodo — the chat panel's todo
// list — reads, so a regression here resurfaces as "the todo list never
// syncs with what the agent writes".
func TestWsScopeProjectChatSessionFirst(t *testing.T) {
	r, _ := newScopedRegistry(t)
	r.SetProjectID("proj_1")
	r.SetSessionID("s-1787700473713-ucrayk")

	// Bare path → this chat's own session dir.
	sp, err := r.wsScope("todo.md")
	if err != nil {
		t.Fatalf("bare path: %v", err)
	}
	if sp.projectID != "proj_1" || sp.sessionID != "s-1787700473713-ucrayk" || sp.storePath != "todo.md" {
		t.Fatalf("bare path: got %+v", sp)
	}
	if sp.sandboxPath != "/workspace/s-1787700473713-ucrayk/todo.md" {
		t.Fatalf("bare path sandboxPath: %q", sp.sandboxPath)
	}

	// "../" → the project-shared root every chat in the project sees.
	sp, err = r.wsScope("../math-course/lesson.html")
	if err != nil {
		t.Fatalf("../ escape: %v", err)
	}
	if sp.projectID != "proj_1" || sp.sessionID != "" || sp.storePath != "math-course/lesson.html" {
		t.Fatalf("../ escape: got %+v", sp)
	}
	if sp.sandboxPath != "/workspace/math-course/lesson.html" {
		t.Fatalf("../ escape sandboxPath: %q", sp.sandboxPath)
	}

	// "../../" climbs above the project root: rejected.
	if _, err := r.wsScope("../../etc/passwd"); err == nil {
		t.Fatal("double escape: want error")
	}
	// "../<sibling session dir>": another chat's scratch, rejected.
	if _, err := r.wsScope("../s-1786792934517-vl5w38/notes.md"); err == nil {
		t.Fatal("sibling session dir: want error")
	}
	// "../<own session dir>" is this chat's own dir spelled from the
	// root: allowed (round-trips to the same file as the bare path).
	if _, err := r.wsScope("../s-1787700473713-ucrayk/todo.md"); err != nil {
		t.Fatalf("own session via ../: %v", err)
	}
	// A human-named shared dir that merely starts with "s" is NOT a
	// session dir: allowed.
	if _, err := r.wsScope("../shopping-list.md"); err != nil {
		t.Fatalf("s-prefixed shared file: %v", err)
	}
}

// TestWsScopeLooseChatNoParentLayer: loose chats have nothing above their
// session dir, so "../" is a scope escape and must be rejected rather
// than silently re-rooted.
func TestWsScopeLooseChatNoParentLayer(t *testing.T) {
	r, _ := newScopedRegistry(t)
	r.SetSessionID("s-1788305403517-5rdy73")

	sp, err := r.wsScope("todo.md")
	if err != nil {
		t.Fatalf("bare path: %v", err)
	}
	if sp.projectID != "" || sp.sessionID != "s-1788305403517-5rdy73" || sp.storePath != "todo.md" {
		t.Fatalf("bare path: got %+v", sp)
	}
	if _, err := r.wsScope("../shared.md"); err == nil {
		t.Fatal("loose chat ../: want error")
	}
}

// TestWsScopeCodingRootNoEscape: coding-root scope already addresses the
// project root — "../" from there would leave the workspace entirely.
func TestWsScopeCodingRootNoEscape(t *testing.T) {
	r, _ := newScopedRegistry(t)
	r.SetProjectID("proj_1")
	r.SetSessionID("s-1-a")
	r.SetCodingRootScope(true)

	sp, err := r.wsScope("src/index.tsx")
	if err != nil {
		t.Fatalf("bare path: %v", err)
	}
	if sp.sessionID != "" {
		t.Fatalf("coding root: want empty session segment, got %+v", sp)
	}
	if _, err := r.wsScope("../shared.md"); err == nil {
		t.Fatal("coding-root ../: want error")
	}
}

// TestWriteFileLandsPerScope drives the real write_file / read_file
// tools through a LocalFS-backed registry and checks the on-disk layout
// end to end: bare writes land in the chat's session dir (where the todo
// panel reads), "../" writes land in the project-shared root.
func TestWriteFileLandsPerScope(t *testing.T) {
	r, root := newScopedRegistry(t)
	r.SetProjectID("proj_1")
	r.SetSessionID("s-100-x")

	ctx := context.Background()
	mustWrite := func(raw string) {
		t.Helper()
		if _, err := r.Execute(ctx, "write_file", raw); err != nil {
			t.Fatalf("write_file: %v", err)
		}
	}

	mustWrite(`{"path":"todo.md","content":"plan"}`)
	if b, err := os.ReadFile(filepath.Join(root, "agt_test", "projects", "proj_1", "s-100-x", "todo.md")); err != nil || string(b) != "plan" {
		t.Fatalf("session-scoped write: %v %q", err, string(b))
	}

	mustWrite(`{"path":"../math-course/lesson.html","content":"<h1/>"}`)
	if b, err := os.ReadFile(filepath.Join(root, "agt_test", "projects", "proj_1", "math-course", "lesson.html")); err != nil || string(b) != "<h1/>" {
		t.Fatalf("shared-layer write: %v %q", err, string(b))
	}

	// read_file round-trips both scopes.
	out, err := r.Execute(ctx, "read_file", `{"path":"../math-course/lesson.html"}`)
	if err != nil || !strings.Contains(out, "<h1/>") {
		t.Fatalf("read shared layer: %v %q", err, out)
	}
	out, err = r.Execute(ctx, "read_file", `{"path":"todo.md"}`)
	if err != nil || !strings.Contains(out, "plan") {
		t.Fatalf("read session file: %v %q", err, out)
	}

	// edit_file stays in the session scope for bare paths.
	if _, err := r.Execute(ctx, "edit_file", `{"path":"todo.md","old_string":"plan","new_string":"plan v2"}`); err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "agt_test", "projects", "proj_1", "s-100-x", "todo.md")); err != nil || string(b) != "plan v2" {
		t.Fatalf("session-scoped edit: %v %q", err, string(b))
	}
}
