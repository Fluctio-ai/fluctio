package sandbox

import (
	"context"
	"strings"
	"testing"
)

// TestHydrateWorkspaceProjectScopeKeepsSidPrefix pins the project-chat
// scope folding: hydrate must List the whole project (session="") so
// paths keep their <sid>/ prefix and land where the bind mount already
// exposes them. The pre-fix behavior listed the per-chat subdir and
// FLATTENED its files onto the project root — every file twice.
func TestHydrateWorkspaceProjectScopeKeepsSidPrefix(t *testing.T) {
	ws := newFakeWorkspace()
	// Objects as the store returns them under List(agent, "p1", "") —
	// relative to the project root, sid dirs included.
	ws.put(wsScopeKey("dave", scopeForKey("p1", "")), "s-1/notes.md", []byte("chat-1 notes"))
	ws.put(wsScopeKey("dave", scopeForKey("p1", "")), "math-course/worksheet.html", []byte("shared"))

	ex := &fakeExecutor{}
	hydrateWorkspace(context.Background(), ws, ex, "dave", "p1", "s-1", "/workspace")

	ex.mu.Lock()
	defer ex.mu.Unlock()
	if len(ex.writes) != 2 {
		t.Fatalf("expected 2 hydrate writes; got %d: %v", len(ex.writes), ex.writes)
	}
	for want := range map[string]bool{
		"/workspace/s-1/notes.md":               true,
		"/workspace/math-course/worksheet.html": true,
	} {
		if _, ok := ex.writes[want]; !ok {
			t.Errorf("missing hydrate write %s; got %v", want, mapKeys(ex.writes))
		}
	}
	for p := range ex.writes {
		if p == "/workspace/notes.md" || p == "/workspace/worksheet.html" {
			t.Errorf("flattened write %s leaked onto the project root (pre-fix bug)", p)
		}
	}
}

// TestHydrateWorkspaceLooseScopeUnchanged: loose chats keep the plain
// session scope — paths have no prefix and land at /workspace/<rel>.
func TestHydrateWorkspaceLooseScopeUnchanged(t *testing.T) {
	ws := newFakeWorkspace()
	ws.put(wsScopeKey("dave", scopeForKey("", "s-2")), "todo.md", []byte("hi"))

	ex := &fakeExecutor{}
	hydrateWorkspace(context.Background(), ws, ex, "dave", "", "s-2", "/workspace")

	ex.mu.Lock()
	defer ex.mu.Unlock()
	if len(ex.writes) != 1 || !strings.Contains(strings.Join(mapKeys(ex.writes), ","), "/workspace/todo.md") {
		t.Fatalf("loose hydrate wrote unexpected paths: %v", ex.writes)
	}
}
