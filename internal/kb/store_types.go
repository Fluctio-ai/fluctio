package kb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
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
	var wikiGeneratedAt, createdAt, updatedAt, wikiDirtyAt sql.NullString
	var typeNS, statusNS, startAtNS, endAtNS, remindedAtNS sql.NullString
	if err := rows.Scan(&s.ID, &s.AgentID, &s.Title, &s.SourceType, &s.SourceRef,
		&s.EntryCount, &s.TotalChars, &wikiGeneratedAt, &createdAt, &updatedAt,
		&typeNS, &statusNS, &startAtNS, &endAtNS, &remindedAtNS, &wikiDirtyAt); err != nil {
		return KBSource{}, false
	}
	s.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
	s.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt.String)
	if wikiGeneratedAt.Valid && wikiGeneratedAt.String != "" {
		if t, err := time.Parse(time.RFC3339, wikiGeneratedAt.String); err == nil {
			s.WikiGeneratedAt = &t
		}
	}
	if wikiDirtyAt.Valid && wikiDirtyAt.String != "" {
		if t, err := time.Parse(time.RFC3339, wikiDirtyAt.String); err == nil {
			s.WikiDirtyAt = &t
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
	origin := SourceOriginFromCtx(ctx)
	_, err = tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO kb_sources (id, agent_id, title, source_type, source_ref, entry_count, total_chars, type, status, start_at, end_at, source_session_id, source_seq_ranges, created_at, updated_at)
			VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5), s.ph(6), s.ph(7), s.ph(8), s.ph(9), s.ph(10), s.ph(11), s.ph(12), s.ph(13), s.ph(14), s.ph(15)),
		sourceID, agentID, title, sourceType, sourceRef, 1, len(content), kbType, status, startAt, endAt, origin.SessionID, EncodeSeqRanges(origin.Seq), now, now)
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

// ErrFlashNotFound is returned by UpdateFlash when no row matches the given
// id+agent+type='flash' (wrong id, foreign agent, or a non-flash source).
var ErrFlashNotFound = errors.New("flash not found")

// UpdateFlash overwrites an existing flash's content (its single chunk) with
// the full evolved text, re-derives its title, bumps updated_at, and re-embeds.
// Use when the user iterates / refines / adds to an idea they already recorded
// — so one idea stays as one complete iterated flash instead of fragmenting
// into many partial duplicates.
func (s *KBStore) UpdateFlash(ctx context.Context, agentID, sourceID, content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("no content to save")
	}
	var srcType string
	err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT type FROM kb_sources WHERE id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		sourceID, agentID).Scan(&srcType)
	if err != nil {
		return ErrFlashNotFound
	}
	if srcType != "flash" {
		return fmt.Errorf("source %s is a %s, not a flash", sourceID, srcType)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE kb_entries SET content = %s WHERE source_id = %s AND agent_id = %s AND chunk_index = 0`,
			s.ph(1), s.ph(2), s.ph(3)),
		content, sourceID, agentID); err != nil {
		return fmt.Errorf("update flash entry: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE kb_sources SET title = %s, total_chars = %s, updated_at = %s WHERE id = %s AND agent_id = %s`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5)),
		deriveTitle(content), len(content), now, sourceID, agentID); err != nil {
		return fmt.Errorf("update flash source: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if s.embedder != nil && s.embedder.Available() {
		go s.embedSourceEntries(context.Background(), agentID, sourceID)
	}
	return nil
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

// UpdateTodo mutates a todo's content/status/start_at/end_at. Only non-empty
// arguments are applied; an empty argument leaves that field untouched. A
// content edit passes the full new text and mirrors UpdateFlash: the single
// chunk 0 is rewritten, the title re-derived, and the source re-embedded.
// reminded_at is reset on every change so a rescheduled or reopened todo can
// be pushed again by the due-reminder sweep. Returns ErrTodoNotFound when no
// todo row matched.
func (s *KBStore) UpdateTodo(ctx context.Context, agentID, sourceID, content, status, startAt, endAt string) error {
	content = strings.TrimSpace(content)
	if status != "" && !validTodoStatus(status) {
		return fmt.Errorf("invalid status %q", status)
	}
	var sets []string
	var args []interface{}
	n := 0
	if content != "" {
		n++
		sets = append(sets, fmt.Sprintf("title = %s", s.ph(n)))
		args = append(args, deriveTitle(content))
		n++
		sets = append(sets, fmt.Sprintf("total_chars = %s", s.ph(n)))
		args = append(args, len(content))
	}
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	q := fmt.Sprintf(`UPDATE kb_sources SET %s WHERE id = %s AND agent_id = %s AND type = 'todo'`,
		strings.Join(sets, ", "), s.ph(n), s.ph(n+1))
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("update todo: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrTodoNotFound
	}
	if content != "" {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE kb_entries SET content = %s WHERE source_id = %s AND agent_id = %s AND chunk_index = 0`,
				s.ph(1), s.ph(2), s.ph(3)),
			content, sourceID, agentID); err != nil {
			return fmt.Errorf("update todo entry: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if content != "" && s.embedder != nil && s.embedder.Available() {
		go s.embedSourceEntries(context.Background(), agentID, sourceID)
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
		type, status, start_at, end_at, reminded_at, wiki_dirty_at
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

// ListFlashes returns the agent's inspiration flashes (灵感闪记) — single-
// chunk sources of type 'flash', newest first. Flashes have no status or
// timing (unlike todos), so the only filter is a cap on count. Backs the
// knowledgebase_list_flashes tool so the LLM can discover recorded ideas
// proactively instead of waiting for a knowledgebase_search to hit them.
func (s *KBStore) ListFlashes(ctx context.Context, agentID string, limit int) ([]KBSource, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, agent_id, title, source_type, source_ref, entry_count, total_chars, wiki_generated_at, created_at, updated_at,
			type, status, start_at, end_at, reminded_at, wiki_dirty_at
			FROM kb_sources WHERE agent_id = %s AND type = 'flash'
			ORDER BY updated_at DESC LIMIT %s`, s.ph(1), s.ph(2)),
		agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("list flashes: %w", err)
	}
	defer rows.Close()
	var flashes []KBSource
	for rows.Next() {
		f, ok := scanSource(rows)
		if !ok {
			continue
		}
		flashes = append(flashes, f)
	}
	return flashes, nil
}

// searchFlashTodoByVector recalls flash/todo sources by semantic similarity.
// Flashes and todos skip wiki generation (they're short, single-chunk sources),
// so searchWikiByVector can't reach them — this queries their chunk vectors
// directly (kb_entry_embeddings JOIN kb_sources WHERE type IN flash/todo),
// cosine-scores, optionally cross-encoder re-ranks, then thresholds. Returns
// nil on any failure so Search proceeds wiki-only. KBResult.ContentType is
// stamped ("flash"/"todo") and a todo's status is folded into SourceTitle so
// callers can tell these hits apart from article recall.
func (s *KBStore) searchFlashTodoByVector(ctx context.Context, agentID, query string, limit int, threshold float64) []KBResult {
	if limit <= 0 {
		limit = 5
	}
	if s.embedder == nil || !s.embedder.Available() {
		return nil
	}
	qvecs, err := s.embedder.Embed(ctx, []string{query})
	if err != nil || len(qvecs) != 1 {
		return nil
	}
	q := qvecs[0]
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.embedding, ke.content, ke.source_id, COALESCE(s.title,''), s.type, s.status, s.end_at
		 FROM kb_entry_embeddings e
		 JOIN kb_entries ke ON ke.id = e.entry_id
		 JOIN kb_sources s ON s.id = ke.source_id
		 WHERE e.agent_id = `+s.ph(1)+` AND s.type IN ('flash','todo')`, agentID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	type cand struct {
		content, sourceID, title, kbType, status, endAt string
		score                                           float64
	}
	var cands []cand
	for rows.Next() {
		var blob []byte
		var c cand
		if err := rows.Scan(&blob, &c.content, &c.sourceID, &c.title, &c.kbType, &c.status, &c.endAt); err != nil {
			return nil
		}
		vec := kbFloat32FromBlob(blob)
		if len(vec) != len(q) {
			continue
		}
		c.score = kbCosine(q, vec)
		cands = append(cands, c)
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	pool := cands
	if len(pool) > limit*3 {
		pool = pool[:limit*3]
	}
	if s.reranker != nil && s.reranker.Available() && len(pool) > 1 {
		docs := make([]string, len(pool))
		for i, c := range pool {
			docs[i] = c.content
		}
		if scored, err := s.reranker.Rerank(ctx, query, docs, limit); err == nil && len(scored) > 0 {
			reranked := make([]cand, 0, len(scored))
			for _, sd := range scored {
				if sd.Index < 0 || sd.Index >= len(pool) {
					continue
				}
				c := pool[sd.Index]
				c.score = sd.Score
				reranked = append(reranked, c)
			}
			pool = reranked
		}
	}
	results := make([]KBResult, 0, limit)
	for _, c := range pool {
		if c.score < threshold {
			continue
		}
		snippet := c.content
		if len(snippet) > 300 {
			snippet = softClipUTF8(snippet, 300)
		}
		title := c.title
		if c.kbType == "todo" && c.status != "" {
			title = fmt.Sprintf("%s [%s]", c.title, c.status)
		}
		results = append(results, KBResult{
			SourceID:    c.sourceID,
			SourceTitle: title,
			SourceKind:  "kb",
			ContentType: c.kbType,
			Content:     c.content,
			Snippet:     snippet,
			Rank:        c.score,
		})
		if len(results) >= limit {
			break
		}
	}
	return results
}

// searchFlashTodoByKeyword is the embedder-free counterpart to
// searchFlashTodoByVector: it recalls flash/todo sources by query-token
// overlap instead of vector cosine. Flashes and todos skip wiki generation,
// so when no embedder is configured the Search fallback (searchWikiByType,
// wiki-only) would miss them entirely — this fills that gap so a relevant
// flash or todo still surfaces without vectorization. Score is the fraction
// of query tokens found in the entry (0..1), matching the scale
// mergeKBResults expects; entries with zero overlap are dropped.
func (s *KBStore) searchFlashTodoByKeyword(ctx context.Context, agentID, query string, limit int, threshold float64) []KBResult {
	if limit <= 0 {
		limit = 5
	}
	qTokens := tokenizeSet(query)
	if len(qTokens) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT ke.content, ke.source_id, COALESCE(s.title,''), s.type, s.status
		 FROM kb_entries ke
		 JOIN kb_sources s ON s.id = ke.source_id
		 WHERE ke.agent_id = `+s.ph(1)+` AND s.type IN ('flash','todo')
		 ORDER BY s.updated_at DESC`, agentID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	type cand struct {
		content, sourceID, title, kbType, status string
		score                                    float64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.content, &c.sourceID, &c.title, &c.kbType, &c.status); err != nil {
			return nil
		}
		eTokens := tokenizeSet(c.title + " " + c.content)
		if len(eTokens) == 0 {
			continue
		}
		hit := 0
		for t := range qTokens {
			if eTokens[t] {
				hit++
			}
		}
		if hit == 0 {
			continue
		}
		c.score = float64(hit) / float64(len(qTokens))
		cands = append(cands, c)
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	if len(cands) > limit*3 {
		cands = cands[:limit*3]
	}
	results := make([]KBResult, 0, limit)
	for _, c := range cands {
		if c.score < threshold {
			continue
		}
		snippet := c.content
		if len(snippet) > 300 {
			snippet = softClipUTF8(snippet, 300)
		}
		title := c.title
		if c.kbType == "todo" && c.status != "" {
			title = fmt.Sprintf("%s [%s]", c.title, c.status)
		}
		results = append(results, KBResult{
			SourceID:    c.sourceID,
			SourceTitle: title,
			SourceKind:  "kb",
			ContentType: c.kbType,
			Content:     c.content,
			Snippet:     snippet,
			Rank:        c.score,
		})
		if len(results) >= limit {
			break
		}
	}
	return results
}

// mergeKBResults combines wiki (article) hits with flash/todo hits, capping the
// flash/todo contribution to ceil(limit/3) so short-form recall supplements —
// not swamps — article recall, then re-ranks the merged set by score and caps
// to limit. Both inputs are already score-sorted from their recall functions.
func mergeKBResults(wiki, ft []KBResult, limit int) []KBResult {
	if limit <= 0 {
		limit = 5
	}
	flashQuota := (limit + 2) / 3 // ceil(limit/3)
	if flashQuota < 1 {
		flashQuota = 1
	}
	if len(ft) > flashQuota {
		ft = ft[:flashQuota]
	}
	merged := make([]KBResult, 0, len(wiki)+len(ft))
	merged = append(merged, wiki...)
	merged = append(merged, ft...)
	sort.Slice(merged, func(i, j int) bool { return merged[i].Rank > merged[j].Rank })
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

// ListDueTodos returns active todos whose end_at is set, at or before now+
// withinHours, and whose reminded_at is empty (never pushed). This is the
// reminders sweep's working set; MarkTodoReminded excludes a todo until
// UpdateTodo resets reminded_at on the next status/time change.
func (s *KBStore) ListDueTodos(ctx context.Context, agentID string, withinHours int) ([]KBSource, error) {
	if withinHours <= 0 {
		withinHours = 24
	}
	horizon := time.Now().UTC().Add(time.Duration(withinHours) * time.Hour).Format(time.RFC3339)
	q := fmt.Sprintf(`SELECT id, agent_id, title, source_type, source_ref, entry_count, total_chars, wiki_generated_at, created_at, updated_at,
		type, status, start_at, end_at, reminded_at, wiki_dirty_at
		FROM kb_sources
		WHERE agent_id = %s AND type = 'todo' AND status IN ('pending', 'in_progress')
		AND end_at != '' AND end_at <= %s AND reminded_at = ''
		ORDER BY end_at ASC`, s.ph(1), s.ph(2))
	rows, err := s.db.QueryContext(ctx, q, agentID, horizon)
	if err != nil {
		return nil, fmt.Errorf("list due todos: %w", err)
	}
	defer rows.Close()
	var due []KBSource
	for rows.Next() {
		t, ok := scanSource(rows)
		if !ok {
			continue
		}
		due = append(due, t)
	}
	return due, nil
}

// MarkTodoReminded stamps reminded_at=now on a todo so the sweep won't push it
// again until UpdateTodo clears it (reschedule/reopen). A miss (wrong id / not
// a todo) is silent — the sweep is best-effort.
func (s *KBStore) MarkTodoReminded(ctx context.Context, agentID, sourceID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE kb_sources SET reminded_at = %s WHERE id = %s AND agent_id = %s AND type = 'todo'`,
			s.ph(1), s.ph(2), s.ph(3)),
		now, sourceID, agentID)
	return err
}
