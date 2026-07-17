package kb

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// clipUTF8 returns s trimmed to at most maxBytes without splitting a
// multi-byte UTF-8 rune — it backs end up to the last rune-start byte so a
// naive s[:maxBytes] (which can cleave a CJK character and render as �) is
// avoided. The caller decides whether to append an ellipsis.
func clipUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

// softClipUTF8 truncates s near maxBytes, preferring to break at a natural
// boundary (newline, sentence-end punctuation, or a comma) found while
// scanning backward over the latter half of the window, so a snippet ends
// cleanly at a clause/sentence edge instead of mid-sentence. Falls back to
// a plain rune boundary when no punctuation is nearby.
func softClipUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && s[end]&0xC0 == 0x80 {
		end--
	}
	floor := end / 2
	// LastIndexAny is rune-aware: it returns the byte offset of the last
	// punctuation/newline rune in the window. Cut just past it (inclusive).
	if idx := strings.LastIndexAny(s[floor:end], "\n。！？；，、.!?;,"); idx >= 0 {
		absIdx := floor + idx
		rl := utf8.RuneLen([]rune(s[absIdx:])[0])
		return s[:absIdx+rl]
	}
	return s[:end]
}

type KBStore struct {
	db      *sql.DB
	dialect string
}

func NewKBStore(db *sql.DB, dialect string) *KBStore {
	return &KBStore{db: db, dialect: dialect}
}

func (s *KBStore) ph(n int) string {
	if s.dialect == "postgres" {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

func (s *KBStore) IngestText(ctx context.Context, agentID, title, content, sourceType, sourceRef string) (string, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	sourceID := uuid.New().String()

	chunks := ChunkText(content, 0, 0)
	if len(chunks) == 0 {
		return "", fmt.Errorf("no content to ingest")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO kb_sources (id, agent_id, title, source_type, source_ref, entry_count, total_chars, created_at, updated_at)
			VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s)`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5), s.ph(6), s.ph(7), s.ph(8), s.ph(9)),
		sourceID, agentID, title, sourceType, sourceRef, len(chunks), len(content), now, now)
	if err != nil {
		return "", fmt.Errorf("insert source: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx,
		fmt.Sprintf(`INSERT INTO kb_entries (uuid, agent_id, source_id, chunk_index, content)
			VALUES (%s, %s, %s, %s, %s)`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5)))
	if err != nil {
		return "", fmt.Errorf("prepare entry: %w", err)
	}
	defer stmt.Close()

	for _, c := range chunks {
		entryUUID := uuid.New().String()
		if _, err := stmt.ExecContext(ctx, entryUUID, agentID, sourceID, c.Index, c.Content); err != nil {
			return "", fmt.Errorf("insert entry: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return sourceID, nil
}

// Search searches both kb_entries (FTS5/LIKE) and wiki_pages (bigram scorer).
// preFilterLimit: number of SQL candidates for wiki search (default 30).
func (s *KBStore) Search(ctx context.Context, agentID, query string, limit int, preFilterLimit int, wikiRatio float64) ([]KBResult, error) {
	if limit <= 0 {
		limit = 5
	}
	// wikiRatio in [0,1]: fraction of slots reserved for wiki pages. The
	// rest go to kb_entries. Out-of-range falls back to 0.5.
	if wikiRatio < 0 || wikiRatio > 1 {
		wikiRatio = 0.5
	}
	wikiLimit := int(math.Round(float64(limit) * wikiRatio))
	kbLimit := limit - wikiLimit

	var entries []KBResult
	// L1: kb_entries — FTS5 + LIKE fallback for raw chunk matching.
	if kbLimit > 0 {
		var err error
		entries, err = s.searchFTS(ctx, agentID, query, kbLimit)
		if err != nil || len(entries) == 0 {
			entries, err = s.searchLike(ctx, agentID, query, kbLimit)
			if err != nil {
				return nil, err
			}
		}
	}

	// L2: wiki_pages — bigram scorer with Redis cache or SQL pre-filter.
	// kb may return fewer than kbLimit (FTS is strict); we do NOT top up
	// from wiki so the user-chosen ratio is respected rather than defeated
	// by kb's strictness filling the whole limit with wiki.
	if wikiLimit > 0 {
		wikiResults := s.searchWiki(ctx, agentID, query, wikiLimit, preFilterLimit)
		entries = append(entries, wikiResults...)
	}
	return entries, nil
}

func (s *KBStore) searchWiki(ctx context.Context, agentID, query string, limit int, preFilterLimit int) []KBResult {
	if preFilterLimit <= 0 {
		preFilterLimit = 30
	}

	// SQL pre-filter: LIKE on title/body to get candidates, then bigram re-rank.
	candidates := s.searchWikiPrefilter(ctx, agentID, query, preFilterLimit)
	if len(candidates) == 0 {
		return nil
	}

	scored := scoreCandidates(candidates, query, limit)
	return scoredToResults(scored)
}

// searchWikiPrefilter uses LIKE with individual query tokens to get candidate pages.
func (s *KBStore) searchWikiPrefilter(ctx context.Context, agentID, query string, limit int) []wikiPageRow {
	tokens := tokenize(query)
	if len(tokens) == 0 {
		return nil
	}

	// Build OR clause from tokens: (title LIKE '%tok1%' OR body LIKE '%tok1%' OR ...)
	var conditions []string
	var args []interface{}
	for _, tok := range tokens {
		pattern := "%" + tok + "%"
		conditions = append(conditions,
			fmt.Sprintf("(wp.title LIKE %s OR wp.body LIKE %s OR wp.summary LIKE %s)", s.ph(len(args)+1), s.ph(len(args)+2), s.ph(len(args)+3)))
		args = append(args, pattern, pattern, pattern)
	}
	args = append(args, agentID, limit)

	q := fmt.Sprintf(`SELECT wp.id, wp.title, wp.summary, wp.body, wp.page_type, wp.slug, wp.tags
		FROM wiki_pages wp
		WHERE (%s) AND wp.agent_id = %s
		ORDER BY wp.updated_at DESC
		LIMIT %s`,
		strings.Join(conditions, " OR "),
		s.ph(len(args)-1), s.ph(len(args)))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		slog.Debug("wiki prefilter query failed", "err", err)
		return nil
	}
	defer rows.Close()

	var pages []wikiPageRow
	for rows.Next() {
		var p wikiPageRow
		if err := rows.Scan(&p.ID, &p.Title, &p.Summary, &p.Body, &p.PageType, &p.Slug, &p.Tags); err != nil {
			continue
		}
		pages = append(pages, p)
	}
	return pages
}

func scoredToResults(scored []scoredPage) []KBResult {
	results := make([]KBResult, len(scored))
	for i, s := range scored {
		content := s.Summary
		if content == "" {
			content = s.Body
		}
		snippet := content
		if len(snippet) > 300 {
			snippet = softClipUTF8(snippet, 300)
		}
		results[i] = KBResult{
			SourceID:    s.ID,
			SourceTitle: s.Title,
			SourceKind:  "wiki",
			PageType:    s.PageType,
			Content:     content,
			Snippet:     snippet,
			Rank:        s.Score,
		}
	}
	return results
}

func (s *KBStore) searchFTS(ctx context.Context, agentID, query string, limit int) ([]KBResult, error) {
	ftsQuery := s.buildFTSQuery(query)
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT e.source_id, COALESCE(s.title, s.source_ref, ''), e.chunk_index, e.content,
			snippet(kb_entries_fts, 0, '<b>', '</b>', '...', 32) AS snippet,
			kb_entries_fts.rank
		FROM kb_entries_fts
		JOIN kb_entries e ON e.id = kb_entries_fts.rowid
		JOIN kb_sources s ON s.id = e.source_id
		WHERE kb_entries_fts MATCH %s AND e.agent_id = %s
		ORDER BY kb_entries_fts.rank
		LIMIT %s`,
			s.ph(1), s.ph(2), s.ph(3)),
		ftsQuery, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []KBResult
	for rows.Next() {
		var r KBResult
		if err := rows.Scan(&r.SourceID, &r.SourceTitle, &r.ChunkIndex, &r.Content, &r.Snippet, &r.Rank); err != nil {
			continue
		}
		r.SourceKind = "kb"
		results = append(results, r)
	}
	return results, nil
}

func (s *KBStore) searchLike(ctx context.Context, agentID, query string, limit int) ([]KBResult, error) {
	pattern := "%" + query + "%"
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT e.source_id, COALESCE(s.title, s.source_ref, ''), e.chunk_index, e.content,
			'' AS snippet, 0.0 AS rank
		FROM kb_entries e
		JOIN kb_sources s ON s.id = e.source_id
		WHERE (e.content LIKE %s OR s.title LIKE %s) AND e.agent_id = %s
		ORDER BY e.chunk_index
		LIMIT %s`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4)),
		pattern, pattern, agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("kb search: %w", err)
	}
	defer rows.Close()

	var results []KBResult
	for rows.Next() {
		var r KBResult
		if err := rows.Scan(&r.SourceID, &r.SourceTitle, &r.ChunkIndex, &r.Content, &r.Snippet, &r.Rank); err != nil {
			continue
		}
		r.SourceKind = "kb"
		results = append(results, r)
	}
	return results, nil
}

// buildFTSQuery converts a user query into an FTS5 MATCH expression.
func (s *KBStore) buildFTSQuery(query string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return "*"
	}
	parts := strings.Fields(query)
	var tokens []string
	for _, p := range parts {
		runes := []rune(p)
		if len(runes) <= 2 {
			tokens = append(tokens, string(runes))
			continue
		}
		for i := 0; i < len(runes)-1; i++ {
			tokens = append(tokens, string(runes[i:i+2]))
		}
	}
	if len(tokens) == 0 {
		return "*"
	}
	var escaped []string
	for _, t := range tokens {
		t = strings.Trim(t, `"`)
		t = strings.ReplaceAll(t, `"`, `""`)
		escaped = append(escaped, `"`+t+`"`)
	}
	return strings.Join(escaped, " OR ")
}

func (s *KBStore) ListSources(ctx context.Context, agentID string, limit, offset int) ([]KBSource, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, agent_id, title, source_type, source_ref, entry_count, total_chars, wiki_generated_at, created_at, updated_at
			FROM kb_sources WHERE agent_id = %s ORDER BY created_at DESC LIMIT %s OFFSET %s`,
			s.ph(1), s.ph(2), s.ph(3)),
		agentID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()

	var sources []KBSource
	for rows.Next() {
		var src KBSource
		var wikiGeneratedAt, createdAt, updatedAt sql.NullString
		if err := rows.Scan(&src.ID, &src.AgentID, &src.Title, &src.SourceType, &src.SourceRef, &src.EntryCount, &src.TotalChars, &wikiGeneratedAt, &createdAt, &updatedAt); err != nil {
			continue
		}
		src.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
		src.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt.String)
		if wikiGeneratedAt.Valid && wikiGeneratedAt.String != "" {
			if t, err := time.Parse(time.RFC3339, wikiGeneratedAt.String); err == nil {
				src.WikiGeneratedAt = &t
			}
		}
		sources = append(sources, src)
	}
	return sources, nil
}

func (s *KBStore) DeleteSource(ctx context.Context, agentID, sourceID string) error {
	_, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM kb_entries WHERE agent_id = %s AND source_id = %s`, s.ph(1), s.ph(2)),
		agentID, sourceID)
	if err != nil {
		return fmt.Errorf("delete entries: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM kb_sources WHERE id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		sourceID, agentID)
	if err != nil {
		return fmt.Errorf("delete source: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("source not found")
	}
	return nil
}

// MarkSourceGenerated sets the wiki_generated_at timestamp for a source.
func (s *KBStore) MarkSourceGenerated(ctx context.Context, sourceID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE kb_sources SET wiki_generated_at = %s WHERE id = %s`, s.ph(1), s.ph(2)),
		now, sourceID)
	return err
}

func (s *KBStore) GetStats(ctx context.Context, agentID string) (*KBStats, error) {
	var stats KBStats
	err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COALESCE(COUNT(*),0), COALESCE(SUM(entry_count),0), COALESCE(SUM(total_chars),0)
			FROM kb_sources WHERE agent_id = %s`, s.ph(1)),
		agentID).Scan(&stats.SourceCount, &stats.EntryCount, &stats.TotalChars)
	if err != nil {
		return nil, fmt.Errorf("kb stats: %w", err)
	}
	return &stats, nil
}

func (s *KBStore) ListEntries(ctx context.Context, agentID, sourceID string, limit, offset int) ([]KBEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, source_id, chunk_index, content
			FROM kb_entries WHERE agent_id = %s AND source_id = %s
			ORDER BY chunk_index LIMIT %s OFFSET %s`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4)),
		agentID, sourceID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	defer rows.Close()
	var entries []KBEntry
	for rows.Next() {
		var e KBEntry
		if err := rows.Scan(&e.ID, &e.SourceID, &e.ChunkIndex, &e.Content); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (s *KBStore) ListAllEntries(ctx context.Context, agentID, query string, limit, offset int) ([]KBEntry, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	var countSQL, listSQL string
	var args []interface{}
	if query != "" {
		pattern := "%" + query + "%"
		countSQL = fmt.Sprintf(
			`SELECT COUNT(*) FROM kb_entries WHERE agent_id = %s AND content LIKE %s`, s.ph(1), s.ph(2))
		listSQL = fmt.Sprintf(
			`SELECT e.id, e.source_id, e.chunk_index, e.content
			FROM kb_entries e
			WHERE e.agent_id = %s AND e.content LIKE %s
			ORDER BY e.id DESC LIMIT %s OFFSET %s`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4))
		args = []interface{}{agentID, pattern, limit, offset}
	} else {
		countSQL = fmt.Sprintf(
			`SELECT COUNT(*) FROM kb_entries WHERE agent_id = %s`, s.ph(1))
		listSQL = fmt.Sprintf(
			`SELECT e.id, e.source_id, e.chunk_index, e.content
			FROM kb_entries e
			WHERE e.agent_id = %s
			ORDER BY e.id DESC LIMIT %s OFFSET %s`,
			s.ph(1), s.ph(2), s.ph(3))
		args = []interface{}{agentID, limit, offset}
	}

	var total int
	if err := s.db.QueryRowContext(ctx, countSQL, args[:len(args)-2]...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count entries: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, listSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list all entries: %w", err)
	}
	defer rows.Close()

	var entries []KBEntry
	for rows.Next() {
		var e KBEntry
		if err := rows.Scan(&e.ID, &e.SourceID, &e.ChunkIndex, &e.Content); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, total, nil
}

func formatResults(results []KBResult, query string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d results for %q:\n\n", len(results), query))
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("--- [K%d] Result %d (source: %s, chunk %d) ---\n", i+1, i+1, r.SourceTitle, r.ChunkIndex))
		if r.Snippet != "" {
			sb.WriteString(r.Snippet)
		} else {
			content := r.Content
			if len(content) > 500 {
				content = softClipUTF8(content, 500) + "..."
			}
			sb.WriteString(content)
		}
		sb.WriteString("\n\n")
	}
	sb.WriteString("When you use a fact from these results, cite it inline with the bracketed id, e.g. [K1]; multiple sources: [K1][K3].\n")
	return sb.String()
}
