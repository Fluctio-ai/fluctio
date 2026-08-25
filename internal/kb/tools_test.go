package kb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/workspace"
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
		"knowledgebase_save_note_attachment",
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

// TestNoteAttachmentTool exercises knowledgebase_save_note_attachment end to
// end: sandbox /workspace path mapping, bytes landing in the note's
// attachment dir + kb_note_attachments row, and the outside-workspace
// rejection guard.
func TestNoteAttachmentTool(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLUCTIO_HOME", home)
	db := setupKBVectorTestDB(t)
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS kb_notes (
			id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, title TEXT NOT NULL DEFAULT '',
			content_md TEXT NOT NULL DEFAULT '', whiteboard TEXT NOT NULL DEFAULT '',
			sort_order REAL NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS kb_note_attachments (
			id TEXT PRIMARY KEY, note_id TEXT NOT NULL, agent_id TEXT NOT NULL,
			file_name TEXT NOT NULL, file_path TEXT NOT NULL, mime TEXT NOT NULL DEFAULT '',
			size INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	store := NewKBStore(db, "sqlite")
	ws := workspace.NewLocalFS(filepath.Join(home, "workspaces"))
	r := tools.NewRegistry("", "")
	r.SetWorkspaceStore(ws, "agt_test")
	RegisterKBTools(r, store, "agt_test", nil, nil, nil, "", 0)
	ctx := context.Background()

	if _, err := r.Execute(ctx, "knowledgebase_save_note",
		`{"title":"带图笔记","content":"看这张图"}`); err != nil {
		t.Fatalf("create note: %v", err)
	}
	notes, err := store.ListNotes(ctx, "agt_test")
	if err != nil || len(notes) != 1 {
		t.Fatalf("ListNotes: %d notes, err=%v", len(notes), err)
	}
	id := notes[0].ID

	// A "user-uploaded" file inside the agent workspace (session scope).
	const srcRel = "uploads/photo.png"
	if err := ws.Put(ctx, "agt_test", "", "s1", srcRel, strings.NewReader("PNGDATA"), 7, "image/png"); err != nil {
		t.Fatalf("seed source file: %v", err)
	}

	// Sandbox view: the session dir is mounted at /workspace — the model
	// reports /workspace/uploads/photo.png; the tool must map it back.
	r.SetUserRoot(filepath.Join(home, "workspaces", "agt_test", "sessions", "s1"))
	out, err := r.Execute(ctx, "knowledgebase_save_note_attachment",
		fmt.Sprintf(`{"note_id":%q,"file_paths":["/workspace/uploads/photo.png"]}`, id))
	if err != nil {
		t.Fatalf("attach via /workspace path: %v (out=%s)", err, out)
	}
	if !strings.Contains(out, "已附加") {
		t.Fatalf("attach output unexpected: %s", out)
	}
	atts, err := store.ListNoteAttachments(ctx, "agt_test", id)
	if err != nil || len(atts) != 1 {
		t.Fatalf("ListNoteAttachments: %d, err=%v", len(atts), err)
	}
	if atts[0].FileName != "photo.png" || atts[0].Mime != "image/png" ||
		!strings.HasPrefix(atts[0].FilePath, "notes/"+id+"/") {
		t.Fatalf("attachment row wrong: %+v", atts[0])
	}
	rc, err := ws.Get(ctx, "agt_test", "", "", atts[0].FilePath)
	if err != nil {
		t.Fatalf("read back attachment bytes: %v", err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "PNGDATA" {
		t.Fatalf("attachment bytes wrong: %q", data)
	}

	// Host absolute path form works too.
	hostPath := filepath.Join(home, "workspaces", "agt_test", "sessions", "s1", "uploads", "photo.png")
	if _, err := r.Execute(ctx, "knowledgebase_save_note_attachment",
		fmt.Sprintf(`{"note_id":%q,"file_paths":[%q]}`, id, hostPath)); err != nil {
		t.Fatalf("attach via host path: %v", err)
	}
	if atts, _ := store.ListNoteAttachments(ctx, "agt_test", id); len(atts) != 2 {
		t.Fatalf("expected 2 attachments after second attach, got %d", len(atts))
	}

	// Outside the agent workspace → refused.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if out, err := r.Execute(ctx, "knowledgebase_save_note_attachment",
		fmt.Sprintf(`{"note_id":%q,"file_paths":[%q]}`, id, outside)); err != nil {
		t.Fatalf("outside guard should refuse, not error: %v", err)
	} else if !strings.Contains(out, "拒绝") {
		t.Fatalf("outside file not refused: %s", out)
	}

	// Unknown note → friendly refusal, no orphan attachment rows.
	if out, err := r.Execute(ctx, "knowledgebase_save_note_attachment",
		fmt.Sprintf(`{"note_id":"nope","file_paths":[%q]}`, hostPath)); err != nil {
		t.Fatalf("bad note should refuse, not error: %v", err)
	} else if !strings.Contains(out, "找不到") {
		t.Fatalf("bad note not refused: %s", out)
	}
}

// TestSearchRawAfterCitedSource guards the s-1787620042895 regression: a
// source already cited by an earlier knowledgebase_search hit (accumulator
// holds its title) must still be fetchable via knowledgebase_search_raw —
// that tool exists exactly for pulling verbatim chunks of an already-surfaced
// source. The old title-level dedup dropped every raw chunk ("All matching
// sources were already cited earlier"), and formatResults' 500-char clip
// hid the verbatim tail even when chunks came through.
func TestSearchRawAfterCitedSource(t *testing.T) {
	db := setupKBVectorTestDB(t)
	store := NewKBStore(db, "sqlite")
	r := tools.NewRegistry("", "")
	RegisterKBTools(r, store, "agt_test", nil, nil, nil, "", 0)
	ctx := context.Background()

	// Two paragraphs so the second chunk starts ~800 and runs ~700 chars,
	// placing the marker past the 500-char clip point of its chunk.
	p1 := strings.Repeat("备", 600)
	p2 := strings.Repeat("考", 800) + "简历投递专用邮箱RESUME-MAIL-MARKER"
	id, err := store.IngestText(ctx, "agt_test", "重庆小升初速查手册", p1+"\n\n"+p2, "text", "manual")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// Simulate the earlier search hit: the accumulator already cites this
	// source (wiki-channel shape, chunk 0).
	var acc []KnowledgeSource
	acc = append(acc, KnowledgeSource{ID: "K1", File: "重庆小升初速查手册", Kind: "wiki", Chunk: 0})
	actx := WithSourcesAccumulator(ctx, &acc)

	args := fmt.Sprintf(`{"source_ids":[%q],"limit":10}`, id)
	out, err := r.Execute(actx, "knowledgebase_search_raw", args)
	if err != nil {
		t.Fatalf("search_raw: %v", err)
	}
	if strings.Contains(out, "already cited") {
		t.Fatalf("raw blocked by already-cited dedup: %s", clipTestOut(out))
	}
	if !strings.Contains(out, "RESUME-MAIL-MARKER") {
		t.Fatalf("raw missing verbatim tail marker (500-char clip?): %s", clipTestOut(out))
	}

	// Re-fetching the exact same chunks still dedups — the anti-bloat
	// intent is preserved, per chunk instead of per source.
	out2, err := r.Execute(actx, "knowledgebase_search_raw", args)
	if err != nil {
		t.Fatalf("search_raw second call: %v", err)
	}
	if !strings.Contains(out2, "already cited") {
		t.Fatalf("identical raw chunks should be deduped on re-fetch: %s", clipTestOut(out2))
	}
}

// TestKnowledgebaseListFullID: knowledgebase_list's id must be the complete
// UUID — other tools (search_raw/delete/generate_insights) take it verbatim
// as source_id, so a 12-char display truncation makes the id unusable.
func TestKnowledgebaseListFullID(t *testing.T) {
	db := setupKBVectorTestDB(t)
	store := NewKBStore(db, "sqlite")
	r := tools.NewRegistry("", "")
	RegisterKBTools(r, store, "agt_test", nil, nil, nil, "", 0)

	id, err := store.IngestText(context.Background(), "agt_test", "完整ID测试", "内容", "text", "manual")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	out, err := r.Execute(context.Background(), "knowledgebase_list", `{}`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "id: "+id) {
		t.Fatalf("list must expose the full source id %q: %s", id, clipTestOut(out))
	}
}

func clipTestOut(s string) string {
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}
