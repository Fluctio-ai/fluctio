package kb

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
	"github.com/fluctio-ai/fluctio/internal/provider"
)

// TestRegisterKBToolsIncludesFlashTodo verifies the content-type tools land in
// the registry alongside the existing KB tools. Guards against the dead-wiring
// trap. sourceRatioFn/thresholdFn/insightInvoker can be nil — the new tools
// never read them, and a nil invoker simply leaves generate_insights off.
func TestRegisterKBToolsIncludesFlashTodo(t *testing.T) {
	db := setupKBVectorTestDB(t)
	store := NewKBStore(db, "sqlite")
	r := tools.NewRegistry("", "")
	RegisterKBTools(r, store, "agt_test", nil, nil, nil, "", 0)

	for _, name := range []string{
		"knowledgebase_save_flash",
		"knowledgebase_save_todo",
		"knowledgebase_update_todo",
		"knowledgebase_list_todos",
		"knowledgebase_list_notes",
		"knowledgebase_read_note",
		"knowledgebase_save_note",
		"knowledgebase_search", // existing — sanity check
	} {
		if !r.HasBuiltin(name) {
			t.Errorf("tool %q not registered", name)
		}
	}
	if r.HasBuiltin("knowledgebase_generate_insights") {
		t.Errorf("generate_insights must NOT register when invoker is nil")
	}
}

// TestRegisterKBToolsInsightGenerator verifies the deep-reading tool registers
// when an invoker is supplied.
func TestRegisterKBToolsInsightGenerator(t *testing.T) {
	db := setupKBVectorTestDB(t)
	store := NewKBStore(db, "sqlite")
	r := tools.NewRegistry("", "")
	invoker := InsightInvoker(func(ctx context.Context, messages []provider.Message) (string, error) {
		return "{}", nil
	})
	RegisterKBTools(r, store, "agt_test", nil, nil, invoker, "test-model", 1024)
	if !r.HasBuiltin("knowledgebase_generate_insights") {
		t.Errorf("generate_insights tool not registered when invoker provided")
	}
}

// TestNoteToolsAppendAndWhiteboardGuard exercises the note tools end to end
// through Execute: create → list → read → append (existing body and whiteboard
// fences untouched) → rewrite that drops a ```whiteboard fence is rejected,
// rewrite carrying the fence over succeeds.
func TestNoteToolsAppendAndWhiteboardGuard(t *testing.T) {
	db := setupKBVectorTestDB(t)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS kb_notes (
		id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, title TEXT NOT NULL DEFAULT '',
		content_md TEXT NOT NULL DEFAULT '', whiteboard TEXT NOT NULL DEFAULT '',
		sort_order REAL NOT NULL DEFAULT 0,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("create kb_notes: %v", err)
	}
	store := NewKBStore(db, "sqlite")
	r := tools.NewRegistry("", "")
	RegisterKBTools(r, store, "agt_test", nil, nil, nil, "", 0)
	ctx := context.Background()

	if _, err := r.Execute(ctx, "knowledgebase_save_note",
		`{"title":"会议记录","content":"# 会议记录\n要点一"}`); err != nil {
		t.Fatalf("create note: %v", err)
	}
	list, err := r.Execute(ctx, "knowledgebase_list_notes", `{}`)
	if err != nil {
		t.Fatalf("list notes: %v", err)
	}
	if !strings.Contains(list, "会议记录") {
		t.Fatalf("list output missing note title: %s", list)
	}
	notes, err := store.ListNotes(ctx, "agt_test")
	if err != nil || len(notes) != 1 {
		t.Fatalf("ListNotes after create: %d notes, err=%v", len(notes), err)
	}
	id := notes[0].ID

	read, err := r.Execute(ctx, "knowledgebase_read_note", fmt.Sprintf(`{"note_id":%q}`, id))
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	if !strings.Contains(read, "要点一") {
		t.Fatalf("read output missing body: %s", read)
	}

	// Append keeps the existing body and whiteboard fence untouched.
	board := "text\n\n```whiteboard\n{\"elements\":[]}\n```\n"
	if _, err := store.SaveNote(ctx, "agt_test", id, "会议记录", board, ""); err != nil {
		t.Fatalf("seed whiteboard: %v", err)
	}
	if _, err := r.Execute(ctx, "knowledgebase_save_note",
		fmt.Sprintf(`{"note_id":%q,"append":true,"content":"## 补充\n新要点"}`, id)); err != nil {
		t.Fatalf("append note: %v", err)
	}
	note, found, err := findNote(ctx, store, "agt_test", id)
	if err != nil || !found {
		t.Fatalf("findNote after append: found=%v err=%v", found, err)
	}
	if !strings.Contains(note.ContentMD, "```whiteboard") || !strings.Contains(note.ContentMD, "新要点") {
		t.Fatalf("append lost body content: %q", note.ContentMD)
	}
	if !strings.HasPrefix(note.ContentMD, "text\n\n```whiteboard") {
		t.Fatalf("append disturbed the original body: %q", note.ContentMD)
	}

	// Rewrite dropping the fence is rejected; rewrite keeping it succeeds.
	if out, err := r.Execute(ctx, "knowledgebase_save_note",
		fmt.Sprintf(`{"note_id":%q,"content":"全新正文，白板没了"}`, id)); err != nil {
		t.Fatalf("guard rewrite errored instead of refusing: %v", err)
	} else if !strings.Contains(out, "拒绝") {
		t.Fatalf("guard rewrite not rejected: %s", out)
	}
	newBody := "全新正文\n\n```whiteboard\n{\"elements\":[]}\n```"
	payload, err := json.Marshal(map[string]string{"note_id": id, "content": newBody})
	if err != nil {
		t.Fatalf("marshal rewrite args: %v", err)
	}
	if _, err := r.Execute(ctx, "knowledgebase_save_note", string(payload)); err != nil {
		t.Fatalf("fence-carrying rewrite: %v", err)
	}
	if note, _, _ := findNote(ctx, store, "agt_test", id); !strings.Contains(note.ContentMD, "```whiteboard") {
		t.Fatalf("fence-carrying rewrite lost the fence: %q", note.ContentMD)
	}
}
