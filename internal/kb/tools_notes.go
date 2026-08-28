package kb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/safename"
	"github.com/google/uuid"
)

// tools_notes.go exposes the 笔记 (personal notes) view to the harness.
// Notes are editable markdown documents (kb_notes) deliberately outside the
// kb_sources chunking/embedding pipeline (see store_notes.go), so
// knowledgebase_search cannot find them — the list/read tools below are the
// only discovery path. Harness visibility: the when-to-use lives in the tool
// descriptions (per tool-guidance-placement A), cross-referencing the
// article/flash tools so 一句话灵感 → flash, 检索语料 → article,
// 整理成笔记 → note.

func registerKBNotes(r *tools.Registry, store *KBStore, agentID string) {
	registerKBListNotes(r, store, agentID)
	registerKBReadNote(r, store, agentID)
	registerKBSaveNote(r, store, agentID)
	registerKBAttachFile(r, store, agentID)
}

// findNote loads one note by id via the single-row store fetch (the old
// ListNotes-and-filter dragged every note's full content per tool call).
func findNote(ctx context.Context, store *KBStore, agentID, id string) (KBNote, bool, error) {
	return store.GetNote(ctx, agentID, id)
}

// registerKBListNotes adds knowledgebase_list_notes — the agent's view of the
// user's notes (笔记): note_id + title, so the agent can find which note to
// write into before appending.
func registerKBListNotes(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_list_notes", "List the user's notes (笔记) — editable markdown documents shown in the knowledge-base Notes view, each with its note_id and title. Use it to find which note the user means before writing into one, or when they ask what notes they have. Notes are NOT indexed for search — knowledgebase_search cannot find them; this listing (plus knowledgebase_read_note) is the only discovery path.", map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		notes, err := store.ListNotes(ctx, agentID)
		if err != nil {
			return "", err
		}
		if len(notes) == 0 {
			return "No notes yet.", nil
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "%d note(s):\n", len(notes))
		for _, n := range notes {
			title := n.Title
			if title == "" {
				title = "(untitled)"
			}
			fmt.Fprintf(&sb, "- %s (note_id: %s, %d chars, updated %s)\n",
				title, n.ID, len([]rune(n.ContentMD)), n.UpdatedAt.Format("2006-01-02 15:04"))
		}
		sb.WriteString("\nTo add to a note: knowledgebase_save_note with note_id + append=true. To read one: knowledgebase_read_note.")
		return sb.String(), nil
	})
}

// registerKBReadNote adds knowledgebase_read_note — the full markdown body of
// one note. Required before a full rewrite (save_note without append), since
// the rewrite must carry over kept content and whiteboard fences verbatim.
func registerKBReadNote(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_read_note", "Read one note's (笔记) full markdown body by note_id (from knowledgebase_list_notes). Required before REWRITING a note (knowledgebase_save_note without append=true), or whenever the user asks what a note contains. Whiteboard drawings appear as ```whiteboard fences inside the body — treat them as opaque blocks and carry them over VERBATIM when rewriting.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"note_id": map[string]interface{}{
				"type":        "string",
				"description": "The note_id of the note to read (from knowledgebase_list_notes)",
			},
		},
		"required": []string{"note_id"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			NoteID string `json:"note_id"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.NoteID == "" {
			return "", fmt.Errorf("note_id is required")
		}
		note, found, err := findNote(ctx, store, agentID, args.NoteID)
		if err != nil {
			return "", err
		}
		if !found {
			return fmt.Sprintf("找不到 note_id=%s 的笔记（可能不属于本 agent）。请先 knowledgebase_list_notes 确认。", args.NoteID), nil
		}
		return fmt.Sprintf("title: %s\nupdated: %s\n\n%s", note.Title, note.UpdatedAt.Format("2006-01-02 15:04"), note.ContentMD), nil
	})
}

// registerKBSaveNote adds knowledgebase_save_note — create / append / rewrite
// one note. Append is the default for organizing conversation content into an
// existing note: no read-modify-write, existing text and whiteboards stay
// untouched. Full rewrites carry a whiteboard-preservation guard (dropping a
// ```whiteboard fence is rejected) — an LLM rewrite silently wiping a
// hand-drawn board is the expensive failure that guard prevents.
func registerKBSaveNote(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_save_note", "Save or organize content into the user's NOTES (笔记) — markdown documents in the knowledge-base Notes view that the user also edits by hand (may contain whiteboard drawings). THREE modes: (1) CREATE — omit note_id, pass content (title optional, defaults to its first line). (2) APPEND — pass note_id + append=true: content is appended as a new section after the existing body; existing text and whiteboards are untouched. This is the DEFAULT for adding to an existing note — do NOT read-and-rewrite just to add a section. (3) REWRITE — pass note_id + the FULL new body (read the note first via knowledgebase_read_note); ```whiteboard blocks must be carried over verbatim or the write is rejected. Use when the user asks to save / organize / write into a note (记到笔记 / 整理成笔记 / 写进XX笔记). Images/files from the conversation go in via knowledgebase_save_note_attachment after the note exists. Routing: retrievable knowledge → knowledgebase_add (article); one-line idea → knowledgebase_save_flash; editable document → this tool. Don't store the same content in two places.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"title": map[string]interface{}{
				"type":        "string",
				"description": "Note title. On CREATE defaults to the first line of content; optional on APPEND/REWRITE to rename the note.",
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "Markdown to write. CREATE: the whole note body. APPEND: ONLY the new section — it is appended after the existing body. REWRITE: the COMPLETE new body including the kept content and whiteboard fences.",
			},
			"note_id": map[string]interface{}{
				"type":        "string",
				"description": "Omit to CREATE. Pass an existing note_id (from knowledgebase_list_notes) to APPEND or REWRITE that note.",
			},
			"append": map[string]interface{}{
				"type":        "boolean",
				"description": "true = append content after the existing body (existing text/whiteboards untouched). Requires note_id. Omit when creating; omit or false with note_id = full rewrite (read the note first).",
			},
		},
		"required": []string{"content"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			NoteID  string `json:"note_id,omitempty"`
			Title   string `json:"title,omitempty"`
			Content string `json:"content"`
			Append  bool   `json:"append,omitempty"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.Content == "" {
			return "", fmt.Errorf("content is required")
		}
		if args.NoteID == "" {
			if args.Append {
				return "", fmt.Errorf("append=true requires note_id — to create a new note, omit both")
			}
			title := args.Title
			if title == "" {
				title = deriveTitle(args.Content)
			}
			id, err := store.SaveNote(ctx, agentID, "", title, args.Content, "")
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Saved new note (note_id=%s, title=%q, %d chars).", id, title, len([]rune(args.Content))), nil
		}
		note, found, err := findNote(ctx, store, agentID, args.NoteID)
		if err != nil {
			return "", err
		}
		if !found {
			return fmt.Sprintf("找不到 note_id=%s 的笔记（可能不属于本 agent）。请先 knowledgebase_list_notes 确认。", args.NoteID), nil
		}
		title, content := note.Title, note.ContentMD
		if args.Title != "" {
			title = args.Title
		}
		if args.Append {
			content = strings.TrimRight(content, "\n") + "\n\n" + strings.TrimSpace(args.Content)
		} else {
			// Whiteboard-preservation guard: boards live inline in the body as
			// ```whiteboard fences; a rewrite that drops one would silently
			// destroy a hand-drawn board. Counting fence openings suffices.
			if strings.Count(note.ContentMD, "```whiteboard") > strings.Count(args.Content, "```whiteboard") {
				return "重写被拒绝：原笔记含有 ```whiteboard 白板块，但新内容中缺失。请先用 knowledgebase_read_note 读取原文，把白板块逐字原样保留；或改用 append=true 追加，不动原文。", nil
			}
			content = args.Content
		}
		if _, err := store.SaveNote(ctx, agentID, note.ID, title, content, note.Whiteboard); err != nil {
			return "", err
		}
		if args.Append {
			return fmt.Sprintf("Appended %d chars to note %q (note_id=%s).", len([]rune(args.Content)), note.Title, note.ID), nil
		}
		return fmt.Sprintf("Rewrote note %q (note_id=%s, now %d chars).", note.Title, note.ID, len([]rune(content))), nil
	})
}

// registerKBAttachFile adds knowledgebase_save_note_attachment — copy files
// the agent can already reach (inbound IM/web image paths, file-tool outputs,
// sandbox-reported /workspace/... paths) into a note's attachment area,
// mirroring the web upload handler exactly: bytes land in the workspace at
// notes/<noteID>/<uuid8>-<name> plus a kb_note_attachments row, so the Notes
// UI shows them like manual uploads.
func registerKBAttachFile(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_save_note_attachment", "Attach files (images, PDFs, …) to an existing note (笔记). Use when the user wants a photo/file from the conversation kept in a note (把这张图存到笔记 / 连同图片一起整理到笔记). file_paths accept THREE forms: the local path listed under a user-uploaded image in the chat, a path a file tool reported, or a sandbox-reported /workspace/... path (mapped to the host workspace automatically). Files outside the agent workspace (e.g. sandbox /tmp) are rejected — copy them into /workspace first. This tool only handles FILES; the note's text goes through knowledgebase_save_note.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"note_id": map[string]interface{}{
				"type":        "string",
				"description": "The note to attach to (note_id from knowledgebase_list_notes or knowledgebase_save_note output)",
			},
			"file_paths": map[string]interface{}{
				"type":  "array",
				"items": map[string]interface{}{"type": "string"},
				"description": "Local file paths to attach (1..N). Accepts the conversation's image paths, file-tool output paths, or sandbox /workspace/... paths.",
			},
		},
		"required": []string{"note_id", "file_paths"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			NoteID    string   `json:"note_id"`
			FilePaths []string `json:"file_paths"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.NoteID == "" {
			return "", fmt.Errorf("note_id is required")
		}
		if len(args.FilePaths) == 0 {
			return "", fmt.Errorf("file_paths is required")
		}
		ws := r.WorkspaceStore()
		if ws == nil {
			return "", fmt.Errorf("workspace store unavailable on this deployment")
		}
		if !store.NoteExists(ctx, agentID, args.NoteID) {
			return fmt.Sprintf("找不到 note_id=%s 的笔记。请先 knowledgebase_save_note 新建或 knowledgebase_list_notes 确认。", args.NoteID), nil
		}
		home, err := config.HomeDir()
		if err != nil {
			return "", err
		}
		root := filepath.Join(home, "workspaces", agentID)
		var saved []string
		for _, p := range args.FilePaths {
			p = strings.TrimSpace(p)
			// The model reports sandbox paths as logical /workspace/<name>;
			// this process runs on the host where they live at UserRoot/<name>
			// — same mapping as the vision tool / deliver_file.
			if p == "/workspace" || strings.HasPrefix(p, "/workspace/") {
				if ur := r.UserRoot(); ur != "" {
					p = filepath.Join(ur, strings.TrimPrefix(p, "/workspace"))
				}
			}
			abs, err := filepath.Abs(p)
			if err != nil {
				return "", fmt.Errorf("resolve %q: %w", p, err)
			}
			if !pathWithinWorkspace(abs, root) {
				return fmt.Sprintf("拒绝 %s：只支持本 agent 工作区内的文件（%s 子树）。sandbox 里 /workspace 之外的文件（如 /tmp）请先用文件工具复制到 /workspace 下再附加。", p, root), nil
			}
			// Reject oversized files on metadata before reading them into
			// memory — the read-time check below stays as the race backstop.
			if fi, err := os.Stat(abs); err == nil && fi.Size() > 64<<20 {
				return fmt.Sprintf("文件 %s 超过 64MB 上限（%d MB），未附加。", filepath.Base(abs), fi.Size()>>20), nil
			}
			data, err := os.ReadFile(abs)
			if err != nil {
				return "", fmt.Errorf("read %q: %w", p, err)
			}
			if int64(len(data)) > 64<<20 {
				return fmt.Sprintf("文件 %s 超过 64MB 上限（%d MB），未附加。", filepath.Base(abs), len(data)>>20), nil
			}
			name := safename.SanitizeFileName(filepath.Base(abs), 120)
			if name == "" {
				return fmt.Sprintf("无法从路径 %q 推导出安全的文件名，未附加。", p), nil
			}
			mimeType := mime.TypeByExtension(filepath.Ext(name))
			if mimeType == "" || mimeType == "application/octet-stream" {
				mimeType = http.DetectContentType(data)
			}
			wsPath := fmt.Sprintf("notes/%s/%s-%s", args.NoteID, uuid.NewString()[:8], name)
			if err := ws.Put(ctx, agentID, "", "", wsPath, bytes.NewReader(data), int64(len(data)), mimeType); err != nil {
				return "", err
			}
			if _, err := store.AddAttachment(ctx, agentID, args.NoteID, name, wsPath, mimeType, int64(len(data))); err != nil {
				_ = ws.Delete(ctx, agentID, "", "", wsPath)
				return "", err
			}
			saved = append(saved, fmt.Sprintf("%s (%d KB)", name, len(data)/1024))
		}
		return fmt.Sprintf("已附加 %d 个文件到笔记（note_id=%s）：%s。附件在笔记页与手动上传一样展示。", len(saved), args.NoteID, strings.Join(saved, "、")), nil
	})
}

// pathWithinWorkspace reports whether abs sits inside root (or equals it),
// tolerating case differences on Windows (drive-letter casing varies between
// paths the LLM echoes back and what UserHomeDir produced). Symlinks are
// resolved on both sides first so a link planted inside the workspace can't
// smuggle a read from outside it.
func pathWithinWorkspace(abs, root string) bool {
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	if r, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = r
	}
	if abs == absRoot {
		return true
	}
	return len(abs) > len(absRoot) && abs[len(absRoot)] == filepath.Separator &&
		strings.EqualFold(abs[:len(absRoot)], absRoot)
}
