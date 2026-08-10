package kb

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// NormalizeBookmarkURL canonicalizes a URL for storage and dedup: parse, drop
// the fragment, strip a handful of tracking query keys (utm_*, fbclid, gclid,
// ref, igshid), and lower-case the host. Returns the trimmed original when
// the URL won't parse so callers still get a usable value.
func NormalizeBookmarkURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Host == "" {
		return raw
	}
	u.Fragment = ""
	q := u.Query()
	for k := range q {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "utm_") || lk == "fbclid" || lk == "gclid" || lk == "ref" || lk == "igshid" {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	u.Host = strings.ToLower(u.Host)
	return u.String()
}

// SaveBookmark inserts one kb_bookmarks row. The URL is normalized first;
// title and summary are optional. content is the fetched page body (may be
// empty when the fetch was skipped or failed); when non-empty the row is
// stamped content_fetched_at=now. source records the entry point ("cli" /
// "slash" / "llm"). When an embedder is wired, title+summary is embedded
// asynchronously so the bookmark is discoverable by vector recall. Returns
// the new bookmark id.
func (s *KBStore) SaveBookmark(ctx context.Context, agentID, rawURL, title, summary, content, source string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("url is required")
	}
	rawURL = NormalizeBookmarkURL(rawURL)
	now := time.Now().UTC().Format(time.RFC3339)
	id := uuid.New().String()
	fetchedAt := ""
	if content != "" {
		fetchedAt = now
	}
	_, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO kb_bookmarks (id, agent_id, url, title, summary, content, content_fetched_at, source, created_at, updated_at)
			VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5), s.ph(6), s.ph(7), s.ph(8), s.ph(9), s.ph(10)),
		id, agentID, rawURL, title, summary, content, fetchedAt, source, now, now)
	if err != nil {
		return "", fmt.Errorf("insert bookmark: %w", err)
	}
	if s.embedder != nil && s.embedder.Available() {
		go s.embedBookmark(context.Background(), agentID, id)
	}
	return id, nil
}

// embedBookmark embeds title+summary (the short metadata, NOT the fetched
// body) for one bookmark and upserts it into kb_bookmark_embeddings. Best-
// effort and silent on failure — mirrors embedSourceEntries. Called async
// after SaveBookmark / UpdateBookmark.
func (s *KBStore) embedBookmark(ctx context.Context, agentID, bookmarkID string) {
	if s.embedder == nil || !s.embedder.Available() {
		return
	}
	var title, summary string
	err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT title, summary FROM kb_bookmarks WHERE id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		bookmarkID, agentID).Scan(&title, &summary)
	if err != nil {
		return
	}
	text := strings.TrimSpace(title + "\n" + summary)
	if text == "" {
		return // nothing meaningful to embed
	}
	vecs, err := s.embedder.Embed(ctx, []string{text})
	if err != nil || len(vecs) != 1 {
		return
	}
	_ = s.SaveBookmarkEmbedding(ctx, agentID, bookmarkID, vecs[0], s.embedder.Model())
}

// SaveBookmarkEmbedding upserts the title+summary embedding for one bookmark.
func (s *KBStore) SaveBookmarkEmbedding(ctx context.Context, agentID, bookmarkID string, vec []float32, model string) error {
	q := `INSERT INTO kb_bookmark_embeddings (bookmark_id, agent_id, embedding, dim, model, updated_at)
		VALUES (` + s.ph(1) + `,` + s.ph(2) + `,` + s.ph(3) + `,` + s.ph(4) + `,` + s.ph(5) + `,CURRENT_TIMESTAMP)
		ON CONFLICT(bookmark_id) DO UPDATE SET agent_id=excluded.agent_id, embedding=excluded.embedding,
			dim=excluded.dim, model=excluded.model, updated_at=CURRENT_TIMESTAMP`
	_, err := s.db.ExecContext(ctx, q, bookmarkID, agentID, kbFloat32ToBlob(vec), len(vec), model)
	return err
}

// bookmarkScanner is satisfied by both *sql.Row and *sql.Rows (each has a
// Scan(dest ...interface{}) error), so GetBookmark and ListBookmarks can
// share one decode path.
type bookmarkScanner interface {
	Scan(dest ...interface{}) error
}

const bookmarkColumns = `id, agent_id, url, title, summary, content, content_fetched_at, source, created_at, updated_at, promoted_to_article_id`

func scanBookmark(row bookmarkScanner) (KBBookmark, bool) {
	var b KBBookmark
	var fetchedAt, createdAt, updatedAt sql.NullString
	if err := row.Scan(&b.ID, &b.AgentID, &b.URL, &b.Title, &b.Summary, &b.Content, &fetchedAt, &b.Source, &createdAt, &updatedAt, &b.PromotedTo); err != nil {
		return KBBookmark{}, false
	}
	if createdAt.Valid {
		b.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
	}
	if updatedAt.Valid {
		b.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt.String)
	}
	if fetchedAt.Valid && fetchedAt.String != "" {
		b.FetchedAt, _ = time.Parse(time.RFC3339, fetchedAt.String)
	}
	return b, true
}

func (s *KBStore) ListBookmarks(ctx context.Context, agentID string, limit, offset int) ([]KBBookmark, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM kb_bookmarks WHERE agent_id = %s ORDER BY created_at DESC LIMIT %s OFFSET %s`,
			bookmarkColumns, s.ph(1), s.ph(2), s.ph(3)),
		agentID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list bookmarks: %w", err)
	}
	defer rows.Close()
	var out []KBBookmark
	for rows.Next() {
		b, ok := scanBookmark(rows)
		if !ok {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

func (s *KBStore) GetBookmark(ctx context.Context, agentID, id string) (*KBBookmark, error) {
	row := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s FROM kb_bookmarks WHERE id = %s AND agent_id = %s`,
			bookmarkColumns, s.ph(1), s.ph(2)),
		id, agentID)
	b, ok := scanBookmark(row)
	if !ok {
		return nil, fmt.Errorf("bookmark not found")
	}
	return &b, nil
}

func (s *KBStore) DeleteBookmark(ctx context.Context, agentID, id string) error {
	// Cascade: drop the bookmark's embedding first (best-effort).
	_, _ = s.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM kb_bookmark_embeddings WHERE bookmark_id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		id, agentID)
	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM kb_bookmarks WHERE id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		id, agentID)
	if err != nil {
		return fmt.Errorf("delete bookmark: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("bookmark not found")
	}
	return nil
}

// UpdateBookmark overwrites title and/or summary (the editable metadata) and
// re-embeds so the bookmark stays discoverable under its new text. URL and
// fetched content are immutable here — a re-fetch is a separate operation.
func (s *KBStore) UpdateBookmark(ctx context.Context, agentID, id, title, summary string) error {
	var sets []string
	var args []interface{}
	n := 0
	if title != "" {
		n++
		sets = append(sets, fmt.Sprintf("title = %s", s.ph(n)))
		args = append(args, title)
	}
	if summary != "" {
		n++
		sets = append(sets, fmt.Sprintf("summary = %s", s.ph(n)))
		args = append(args, summary)
	}
	if len(sets) == 0 {
		return nil
	}
	n++
	sets = append(sets, fmt.Sprintf("updated_at = %s", s.ph(n)))
	args = append(args, time.Now().UTC().Format(time.RFC3339))
	n++
	args = append(args, id, agentID)
	q := fmt.Sprintf(`UPDATE kb_bookmarks SET %s WHERE id = %s AND agent_id = %s`,
		strings.Join(sets, ", "), s.ph(n), s.ph(n+1))
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("update bookmark: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("bookmark not found")
	}
	if s.embedder != nil && s.embedder.Available() {
		go s.embedBookmark(context.Background(), agentID, id)
	}
	return nil
}

// searchBookmarksByVector recalls bookmarks by cosine similarity over their
// title+summary embedding, optionally cross-encoder re-ranks, then thresholds.
// Returns nil on any failure so Search can treat bookmarks as an optional
// supplement. KBResult.ContentType is stamped "bookmark" so callers can tell
// these hits apart from article/flash/todo recall.
func (s *KBStore) searchBookmarksByVector(ctx context.Context, agentID, query string, limit int, threshold float64) []KBResult {
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
		`SELECT e.embedding, b.id, b.url, b.title, b.summary
		 FROM kb_bookmark_embeddings e
		 JOIN kb_bookmarks b ON b.id = e.bookmark_id
		 WHERE e.agent_id = `+s.ph(1), agentID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	type cand struct {
		id, link, title, summary string
		score                    float64
	}
	var cands []cand
	for rows.Next() {
		var blob []byte
		var c cand
		if err := rows.Scan(&blob, &c.id, &c.link, &c.title, &c.summary); err != nil {
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
			docs[i] = c.title + "\n" + c.summary
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
		display := c.title
		if display == "" {
			display = c.link
		}
		results = append(results, KBResult{
			SourceID:    c.id,
			SourceTitle: display,
			SourceKind:  "kb",
			ContentType: "bookmark",
			Content:     c.summary,
			Snippet:     c.link,
			Rank:        c.score,
		})
		if len(results) >= limit {
			break
		}
	}
	return results
}

// BackfillBookmarkEmbeddings vectorizes kb_bookmarks whose title+summary has
// no embedding yet — the safety-net for the save-time path (embedBookmark),
// which is best-effort and silent on failure. CLI/slash-saved bookmarks never
// had an embedder wired, so this is how they catch up once vectorization is
// on. Mirrors BackfillEntryEmbeddings (force=false). Returns processed/failed
// counts; a failed embed is counted and skipped, never aborts the pass.
func (s *KBStore) BackfillBookmarkEmbeddings(ctx context.Context, agentID string, perCallDelay time.Duration) (processed, failed int, err error) {
	if s.embedder == nil || !s.embedder.Available() || agentID == "" {
		return 0, 0, fmt.Errorf("kb.BackfillBookmarkEmbeddings: embedder and agentID required")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT b.id, b.title, b.summary
		 FROM kb_bookmarks b
		 LEFT JOIN kb_bookmark_embeddings e ON e.bookmark_id = b.id
		 WHERE b.agent_id = `+s.ph(1)+` AND e.bookmark_id IS NULL
		 ORDER BY b.created_at`, agentID)
	if err != nil {
		return 0, 0, fmt.Errorf("kb bookmark backfill: list pending: %w", err)
	}
	type pending struct {
		id              string
		title, summary  string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.title, &p.summary); err != nil {
			rows.Close()
			return processed, failed, err
		}
		batch = append(batch, p)
	}
	rows.Close()
	if len(batch) == 0 {
		return 0, 0, nil
	}
	// Embed one at a time — bookmarks are few and each text is short, so
	// batching buys little and complicates the title+summary concat.
	for _, p := range batch {
		if ctx.Err() != nil {
			return processed, failed, ctx.Err()
		}
		text := strings.TrimSpace(p.title + "\n" + p.summary)
		if text == "" {
			continue // nothing to embed; leave pending rather than store an empty vector
		}
		vecs, embErr := s.embedder.Embed(ctx, []string{text})
		if embErr != nil || len(vecs) != 1 {
			slog.Warn("kb bookmark backfill: embed failed", "agent", agentID, "bookmark", p.id, "error", embErr)
			failed++
		} else if saveErr := s.SaveBookmarkEmbedding(ctx, agentID, p.id, vecs[0], s.embedder.Model()); saveErr != nil {
			failed++
		} else {
			processed++
		}
		if perCallDelay > 0 {
			select {
			case <-time.After(perCallDelay):
			case <-ctx.Done():
				return processed, failed, ctx.Err()
			}
		}
	}
	return processed, failed, nil
}
