package kb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// scanSource decodes one kb_sources row — the original 10 columns plus the five
// content-type columns (type/status/start_at/end_at/reminded_at) — into a
// KBSource. Shared by ListSources and ListTodos so the 15-column scan lives in
// exactly one place. Returns ok=false on scan error so callers skip the row.
func scanSource(rows *sql.Rows) (KBSource, bool) {
	var s KBSource
	var wikiGeneratedAt, createdAt, updatedAt sql.NullString
	var typeNS, statusNS, startAtNS, endAtNS, remindedAtNS sql.NullString
	if err := rows.Scan(&s.ID, &s.AgentID, &s.Title, &s.SourceType, &s.SourceRef,
		&s.EntryCount, &s.TotalChars, &wikiGeneratedAt, &createdAt, &updatedAt,
		&typeNS, &statusNS, &startAtNS, &endAtNS, &remindedAtNS); err != nil {
		return KBSource{}, false
	}
	s.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
	s.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt.String)
	if wikiGeneratedAt.Valid && wikiGeneratedAt.String != "" {
		if t, err := time.Parse(time.RFC3339, wikiGeneratedAt.String); err == nil {
			s.WikiGeneratedAt = &t
		}
	}
	s.Type = typeNS.String
	s.Status = statusNS.String
	if t, ok := parseRFC3339Ptr(startAtNS); ok {
		s.StartAt = t
	}
	if t, ok := parseRFC3339Ptr(endAtNS); ok {
		s.EndAt = t
	}
	if t, ok := parseRFC3339Ptr(remindedAtNS); ok {
		s.RemindedAt = t
	}
	return s, true
}

// parseRFC3339Ptr parses a sql.NullString RFC3339 timestamp into a *time.Time;
// returns ok=false for empty/invalid so KBSource time fields stay nil when the
// todo column was never set.
func parseRFC3339Ptr(ns sql.NullString) (*time.Time, bool) {
	if !ns.Valid || ns.String == "" {
		return nil, false
	}
	t, err := time.Parse(time.RFC3339, ns.String)
	if err != nil {
		return nil, false
	}
	return &t, true
}

// deriveTitle makes a short title for a flash/todo from content: the first
// non-empty line, clipped to ~50 bytes on a rune boundary. Falls back to a
// prefix of the whole content when no line is usable.
func deriveTitle(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 50 {
			return clipUTF8(line, 50) + "…"
		}
		return line
	}
	if len(content) > 50 {
		return clipUTF8(content, 50) + "…"
	}
	return content
}

func validTodoStatus(s string) bool {
	switch s {
	case "pending", "in_progress", "done", "cancelled":
		return true
	}
	return false
}

// ErrTodoNotFound is returned by UpdateTodo when no row matches the given
// id+agent+type='todo' (wrong id, foreign agent, or the id points at a non-todo).
var ErrTodoNotFound = errors.New("todo not found")

// saveSingleChunk inserts one kb_sources row of the given content type plus a
// single kb_entries chunk holding content verbatim (flashes/todos are short —
// no chunking), then best-effort async-embeds it. Mirrors IngestText's shape
// but skips ChunkText and writes the type/status/time columns. status/startAt/
// endAt are empty for flashes; todos pass status (validated upstream) and
// optional RFC3339 timestamps. Returns the new source id.
func (s *KBStore) saveSingleChunk(ctx context.Context, agentID, kbType, title, content, sourceType, sourceRef, status, startAt, endAt string) (string, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	sourceID := uuid.New().String()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO kb_sources (id, agent_id, title, source_type, source_ref, entry_count, total_chars, type, status, start_at, end_at, created_at, updated_at)
			VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5), s.ph(6), s.ph(7), s.ph(8), s.ph(9), s.ph(10), s.ph(11), s.ph(12), s.ph(13)),
		sourceID, agentID, title, sourceType, sourceRef, 1, len(content), kbType, status, startAt, endAt, now, now)
	if err != nil {
		return "", fmt.Errorf("insert source: %w", err)
	}
	entryUUID := uuid.New().String()
	_, err = tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO kb_entries (uuid, agent_id, source_id, chunk_index, content) VALUES (%s,%s,%s,%s,%s)`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5)),
		entryUUID, agentID, sourceID, 0, content)
	if err != nil {
		return "", fmt.Errorf("insert entry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	if s.embedder != nil && s.embedder.Available() {
		go s.embedSourceEntries(context.Background(), agentID, sourceID)
	}
	return sourceID, nil
}

// SaveFlash stores an inspiration flash (灵感闪记) as a single un-chunked KB
// source of type 'flash'. Title is derived from the first line of content.
func (s *KBStore) SaveFlash(ctx context.Context, agentID, content string) (string, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", fmt.Errorf("no content to save")
	}
	return s.saveSingleChunk(ctx, agentID, "flash", deriveTitle(content), content, "text", "manual", "", "", "")
}

// SaveTodo stores a todo item as a single KB source of type 'todo'. status
// defaults to pending and is validated against the four allowed values;
// startAt/endAt are RFC3339 strings or empty.
func (s *KBStore) SaveTodo(ctx context.Context, agentID, content, status, startAt, endAt string) (string, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", fmt.Errorf("no content to save")
	}
	if status == "" {
		status = "pending"
	}
	if !validTodoStatus(status) {
		return "", fmt.Errorf("invalid status %q (pending/in_progress/done/cancelled)", status)
	}
	return s.saveSingleChunk(ctx, agentID, "todo", deriveTitle(content), content, "text", "manual", status, startAt, endAt)
}

// UpdateTodo mutates a todo's status/start_at/end_at. Only non-empty arguments
// are applied; an empty argument leaves that field untouched. reminded_at is
// reset on every change so a rescheduled or reopened todo can be pushed again
// by the due-reminder sweep. Returns ErrTodoNotFound when no todo row matched.
func (s *KBStore) UpdateTodo(ctx context.Context, agentID, sourceID string, status, startAt, endAt string) error {
	if status != "" && !validTodoStatus(status) {
		return fmt.Errorf("invalid status %q", status)
	}
	var sets []string
	var args []interface{}
	n := 0
	if status != "" {
		n++
		sets = append(sets, fmt.Sprintf("status = %s", s.ph(n)))
		args = append(args, status)
	}
	if startAt != "" {
		n++
		sets = append(sets, fmt.Sprintf("start_at = %s", s.ph(n)))
		args = append(args, startAt)
	}
	if endAt != "" {
		n++
		sets = append(sets, fmt.Sprintf("end_at = %s", s.ph(n)))
		args = append(args, endAt)
	}
	if len(sets) == 0 {
		return nil // nothing to update
	}
	n++
	sets = append(sets, fmt.Sprintf("reminded_at = %s", s.ph(n)))
	args = append(args, "")
	n++
	sets = append(sets, fmt.Sprintf("updated_at = %s", s.ph(n)))
	args = append(args, time.Now().UTC().Format(time.RFC3339))
	n++
	args = append(args, sourceID, agentID)
	q := fmt.Sprintf(`UPDATE kb_sources SET %s WHERE id = %s AND agent_id = %s AND type = 'todo'`,
		strings.Join(sets, ", "), s.ph(n), s.ph(n+1))
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("update todo: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrTodoNotFound
	}
	return nil
}

// ListTodos returns a todo list scoped to an agent. status="" returns every
// status; status="active" returns pending+in_progress (the reminders working
// set); any other value filters to that exact status. dueWithinHours>0 narrows
// further to todos whose end_at is set and at or before now+window (due soon
// or overdue), ordered by end_at ascending; otherwise ordered by updated_at
// descending.
func (s *KBStore) ListTodos(ctx context.Context, agentID, status string, dueWithinHours int) ([]KBSource, error) {
	var conditions []string
	var args []interface{}
	n := 0
	n++
	conditions = append(conditions, fmt.Sprintf("agent_id = %s", s.ph(n)))
	args = append(args, agentID)
	conditions = append(conditions, "type = 'todo'")
	switch {
	case status == "active":
		conditions = append(conditions, fmt.Sprintf("status IN (%s, %s)", s.ph(n+1), s.ph(n+2)))
		args = append(args, "pending", "in_progress")
		n += 2
	case status != "":
		n++
		conditions = append(conditions, fmt.Sprintf("status = %s", s.ph(n)))
		args = append(args, status)
	}
	order := "updated_at DESC"
	if dueWithinHours > 0 {
		n++
		horizon := time.Now().UTC().Add(time.Duration(dueWithinHours) * time.Hour).Format(time.RFC3339)
		conditions = append(conditions, fmt.Sprintf("end_at != '' AND end_at <= %s", s.ph(n)))
		args = append(args, horizon)
		order = "end_at ASC"
	}
	q := fmt.Sprintf(`SELECT id, agent_id, title, source_type, source_ref, entry_count, total_chars, wiki_generated_at, created_at, updated_at,
		type, status, start_at, end_at, reminded_at
		FROM kb_sources WHERE %s ORDER BY %s`,
		strings.Join(conditions, " AND "), order)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list todos: %w", err)
	}
	defer rows.Close()
	var todos []KBSource
	for rows.Next() {
		t, ok := scanSource(rows)
		if !ok {
			continue
		}
		todos = append(todos, t)
	}
	return todos, nil
}
