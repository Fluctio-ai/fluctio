package kb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// store_notes.go backs the 笔记 (personal notes) view: kb_notes holds the
// markdown body + whiteboard JSON, kb_note_attachments tracks uploaded
// files whose bytes live in the agent workspace. Deliberately outside the
// kb_sources/kb_entries chunking-and-embedding pipeline — notes are editor
// documents first; recall integration can layer on later without schema
// changes.

const noteColumns = `id, agent_id, title, content_md, whiteboard, created_at, updated_at`

type noteScanner interface {
	Scan(dest ...interface{}) error
}

func scanNote(row noteScanner) (KBNote, bool) {
	var n KBNote
	var createdAt, updatedAt sql.NullString
	if err := row.Scan(&n.ID, &n.AgentID, &n.Title, &n.ContentMD, &n.Whiteboard, &createdAt, &updatedAt); err != nil {
		return KBNote{}, false
	}
	if createdAt.Valid {
		n.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
	}
	if updatedAt.Valid {
		n.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt.String)
	}
	return n, true
}

// ListNotes returns the agent's notes in display order: manual drag order
// (sort_order ASC, 1-based, written by ReorderNotes) first, then un-ordered
// notes newest-first (sort_order 0). A fresh note therefore sits on top
// until the user drags anything; after the first drag every note carries
// an explicit position and manual order wins. Content and whiteboard ride
// along: notes are few and the editor loads the selected note in full,
// so a second fetch-by-id round trip buys nothing (same call as bookmarks).
func (s *KBStore) ListNotes(ctx context.Context, agentID string) ([]KBNote, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM kb_notes WHERE agent_id = %s ORDER BY sort_order ASC, updated_at DESC`,
			noteColumns, s.ph(1)),
		agentID)
	if err != nil {
		return nil, fmt.Errorf("list notes: %w", err)
	}
	defer rows.Close()
	var out []KBNote
	for rows.Next() {
		n, ok := scanNote(rows)
		if !ok {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// ReorderNotes writes the manual list order: ids[0] gets sort_order 1,
// ids[1] gets 2, … . Notes absent from the list keep sort_order 0 (they
// sort after the manual block, newest-first). Idempotent — the client
// sends the full visible order after each drag.
func (s *KBStore) ReorderNotes(ctx context.Context, agentID string, ids []string) error {
	for i, id := range ids {
		if _, err := s.db.ExecContext(ctx,
			fmt.Sprintf(`UPDATE kb_notes SET sort_order = %s WHERE id = %s AND agent_id = %s`,
				s.ph(1), s.ph(2), s.ph(3)),
			float64(i+1), id, agentID); err != nil {
			return fmt.Errorf("reorder notes: %w", err)
		}
	}
	return nil
}

// SaveNote upserts one note. Empty id creates; otherwise the caller's id
// must already exist for the note (a fresh uuid on an unknown id would
// silently create an orphan row the client can't reach again).
func (s *KBStore) SaveNote(ctx context.Context, agentID, id, title, contentMD, whiteboard string) (string, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if id == "" {
		id = uuid.NewString()
		_, err := s.db.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO kb_notes (%s) VALUES (%s,%s,%s,%s,%s,%s,%s)`,
				noteColumns, s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5), s.ph(6), s.ph(7)),
			id, agentID, title, contentMD, whiteboard, now, now)
		if err != nil {
			return "", fmt.Errorf("insert note: %w", err)
		}
		return id, nil
	}
	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE kb_notes SET title = %s, content_md = %s, whiteboard = %s, updated_at = %s
			WHERE id = %s AND agent_id = %s`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5), s.ph(6)),
		title, contentMD, whiteboard, now, id, agentID)
	if err != nil {
		return "", fmt.Errorf("update note: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", fmt.Errorf("note not found")
	}
	return id, nil
}

// DeleteNote removes a note and its attachment rows. Returns the deleted
// attachment file paths (workspace-relative) so the caller can drop the
// bytes from the workspace store — file I/O stays out of the store layer.
func (s *KBStore) DeleteNote(ctx context.Context, agentID, id string) ([]string, error) {
	paths, err := s.noteAttachmentPaths(ctx, agentID, id)
	if err != nil {
		return nil, err
	}
	_, _ = s.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM kb_note_attachments WHERE note_id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		id, agentID)
	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM kb_notes WHERE id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		id, agentID)
	if err != nil {
		return nil, fmt.Errorf("delete note: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("note not found")
	}
	return paths, nil
}

// NoteExists reports whether the (agent, note) pair matches a row. Guards
// attachment uploads so files can't be written under an arbitrary note id.
func (s *KBStore) NoteExists(ctx context.Context, agentID, id string) bool {
	var one int
	err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT 1 FROM kb_notes WHERE id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		id, agentID).Scan(&one)
	return err == nil
}

const noteAttachmentColumns = `id, note_id, agent_id, file_name, file_path, mime, size, created_at`

// ListNoteAttachments returns a note's attachments oldest-first (the UI
// appends new uploads to the end of the grid).
func (s *KBStore) ListNoteAttachments(ctx context.Context, agentID, noteID string) ([]KBNoteAttachment, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM kb_note_attachments WHERE note_id = %s AND agent_id = %s ORDER BY created_at`,
			noteAttachmentColumns, s.ph(1), s.ph(2)),
		noteID, agentID)
	if err != nil {
		return nil, fmt.Errorf("list note attachments: %w", err)
	}
	defer rows.Close()
	var out []KBNoteAttachment
	for rows.Next() {
		var a KBNoteAttachment
		var createdAt sql.NullString
		if err := rows.Scan(&a.ID, &a.NoteID, &a.AgentID, &a.FileName, &a.FilePath, &a.Mime, &a.Size, &createdAt); err != nil {
			continue
		}
		if createdAt.Valid {
			a.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
		}
		out = append(out, a)
	}
	return out, nil
}

// AddAttachment records one uploaded file. filePath is the workspace-
// relative path (notes/<noteID>/<name>) chosen by the caller after the
// bytes have been written.
func (s *KBStore) AddAttachment(ctx context.Context, agentID, noteID, fileName, filePath, mime string, size int64) (string, error) {
	id := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO kb_note_attachments (%s) VALUES (%s,%s,%s,%s,%s,%s,%s,%s)`,
			noteAttachmentColumns,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5), s.ph(6), s.ph(7), s.ph(8)),
		id, noteID, agentID, fileName, filePath, mime, size, now)
	if err != nil {
		return "", fmt.Errorf("insert note attachment: %w", err)
	}
	return id, nil
}

// DeleteAttachment drops one attachment row and returns its file path so
// the caller can delete the bytes from the workspace store.
func (s *KBStore) DeleteAttachment(ctx context.Context, agentID, noteID, attID string) (string, error) {
	var path string
	err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT file_path FROM kb_note_attachments WHERE id = %s AND note_id = %s AND agent_id = %s`,
			s.ph(1), s.ph(2), s.ph(3)),
		attID, noteID, agentID).Scan(&path)
	if err != nil {
		return "", fmt.Errorf("note attachment not found")
	}
	_, err = s.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM kb_note_attachments WHERE id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		attID, agentID)
	if err != nil {
		return "", fmt.Errorf("delete note attachment: %w", err)
	}
	return path, nil
}

func (s *KBStore) noteAttachmentPaths(ctx context.Context, agentID, noteID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT file_path FROM kb_note_attachments WHERE note_id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		noteID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil && strings.TrimSpace(p) != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}
