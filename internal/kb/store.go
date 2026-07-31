package kb

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fluctio-ai/fluctio/internal/embedding"
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
	db       *sql.DB
	dialect  string
	embedder embedding.Embedder // optional; nil → keyword/bigram search
	reranker embedding.Reranker // optional; nil → embedding scores only
}

func NewKBStore(db *sql.DB, dialect string) *KBStore {
	return &KBStore{db: db, dialect: dialect}
}

// SetRetriever equips the store with an embedder (and optional reranker) so
// Search uses vector recall + cross-encoder re-rank instead of keyword
// bigram scoring. Either may be nil; a nil embedder keeps the legacy path.
// Called once when the agent wires up its KB tools — only when KBEmbedding
// is on and an embedder is configured.
func (s *KBStore) SetRetriever(emb embedding.Embedder, rr embedding.Reranker) {
	s.embedder = emb
	s.reranker = rr
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
	// Best-effort async embed of the just-ingested chunks so SearchRawKB can
	// reach them by vector. Never blocks ingest; silent on failure.
	if s.embedder != nil && s.embedder.Available() {
		go s.embedSourceEntries(context.Background(), agentID, sourceID)
	}
	return sourceID, nil
}

// Search searches both kb_entries (FTS5/LIKE) and wiki_pages (bigram scorer).
// preFilterLimit: number of SQL candidates for wiki search (default 30).
// sourceRatio in [0,1]: fraction of wiki slots for source pages (raw-content-
// derived) vs concept/entity/query pages (conceptual). Strategy: search wiki
// first (source + other, each bigram-scored within its bucket so the ratio
// holds); if wiki doesn't fill the limit, fall back to kb_entries (raw chunks,
// FTS) so exact-phrase queries still hit. kb_entries is a fallback only.
func (s *KBStore) Search(ctx context.Context, agentID, query string, limit int, preFilterLimit int, sourceRatio float64, threshold float64) ([]KBResult, error) {
	if limit <= 0 {
		limit = 5
	}
	if sourceRatio < 0 || sourceRatio > 1 {
		sourceRatio = 0.5
	}
	if threshold < 0 || threshold > 1 {
		threshold = 0.45
	}
	sourceLimit := int(math.Round(float64(limit) * sourceRatio))
	otherLimit := limit - sourceLimit

	// Vector path: when an embedder is wired up, recall by semantic
	// similarity and (optionally) cross-encoder re-rank. Falls through to
	// the keyword path on any failure (off / unconfigured / empty store).
	if s.embedder != nil && s.embedder.Available() {
		if rr := s.searchWikiByVector(ctx, agentID, query, limit, preFilterLimit, threshold); len(rr) > 0 {
			return rr, nil
		}
	}

	// Wiki-only: source pages (raw-content-derived) + concept/entity/query
	// pages, each bigram-scored within its bucket. kb_entries is NOT searched
	// here — it's exposed as a separate on-demand tool (SearchRawKB) the LLM
	// calls when wiki's summary isn't detailed enough.
	return s.searchWikiByType(ctx, agentID, query, sourceLimit, otherLimit, preFilterLimit, threshold), nil
}

// SearchRawKB returns the raw kb_entries chunks for the given sourceIDs,
// optionally narrowed by a LIKE query. It backs the knowledgebase_search_raw
// tool: the LLM calls it with source_ids obtained from a prior
// knowledgebase_search to pull the verbatim original text of a source the
// wiki layer already deemed relevant. sourceIDs must be non-empty — this is
// an auxiliary second-pass lookup, not an independent search. Not used by
// auto-query; that path stays wiki-only.
func (s *KBStore) SearchRawKB(ctx context.Context, agentID, query string, sourceIDs []string, limit int) ([]KBResult, error) {
	if limit <= 0 {
		limit = 5
	}
	sourceIDs = s.resolveSourceIDs(ctx, agentID, sourceIDs)
	// Vector path: when an embedder is wired, cosine-search chunk vectors
	// within the requested sources (and optionally re-rank). Falls back to
	// keyword LIKE on any failure (off / unconfigured / empty store).
	if s.embedder != nil && s.embedder.Available() && strings.TrimSpace(query) != "" {
		if rr := s.searchRawByVector(ctx, agentID, query, sourceIDs, limit); len(rr) > 0 {
			return rr, nil
		}
	}
	return s.searchLike(ctx, agentID, query, sourceIDs, limit)
}

// resolveSourceIDs expands any wiki page ids ("<type>:<slug>", as returned by
// knowledgebase_search) into their underlying KB source UUIDs listed in
// wiki_pages.source_ids. Plain UUIDs pass through unchanged. This bridges the
// two id namespaces so the LLM can hand the raw tool either form — search
// returns wiki page ids, but kb_entries.source_id is a UUID. On lookup error
// it falls back to the original ids (no worse than before).
func (s *KBStore) resolveSourceIDs(ctx context.Context, agentID string, ids []string) []string {
	var wikiIDs, resolved []string
	for _, id := range ids {
		if strings.Contains(id, ":") {
			wikiIDs = append(wikiIDs, id)
		} else {
			resolved = append(resolved, id)
		}
	}
	if len(wikiIDs) == 0 {
		return ids
	}
	phs := make([]string, len(wikiIDs))
	args := make([]interface{}, 0, len(wikiIDs)+1)
	for i, wid := range wikiIDs {
		phs[i] = s.ph(i + 1)
		args = append(args, wid)
	}
	args = append(args, agentID)
	q := fmt.Sprintf(`SELECT source_ids FROM wiki_pages WHERE id IN (%s) AND agent_id = %s`,
		strings.Join(phs, ","), s.ph(len(wikiIDs)+1))
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		slog.Debug("resolveSourceIDs wiki lookup failed", "err", err)
		return ids
	}
	defer rows.Close()
	for rows.Next() {
		var srcJSON string
		if err := rows.Scan(&srcJSON); err != nil {
			continue
		}
		var uuids []string
		if err := json.Unmarshal([]byte(srcJSON), &uuids); err == nil {
			resolved = append(resolved, uuids...)
		}
	}
	if len(resolved) == 0 {
		return ids
	}
	return resolved
}

// searchWikiByType scores wiki pages in two buckets — source pages
// (raw-content-derived) and everything else (concept/entity/query) — and
// returns up to sourceLimit + otherLimit so the caller's ratio holds even
// when one bucket has few matches.
func (s *KBStore) searchWikiByType(ctx context.Context, agentID, query string, sourceLimit, otherLimit, preFilterLimit int, threshold float64) []KBResult {
	if sourceLimit <= 0 && otherLimit <= 0 {
		return nil
	}
	if preFilterLimit <= 0 {
		preFilterLimit = 30
	}
	candidates := s.searchWikiPrefilter(ctx, agentID, query, preFilterLimit)
	if len(candidates) == 0 {
		return nil
	}
	var sourcePages, otherPages []wikiPageRow
	for _, p := range candidates {
		if p.PageType == "source" {
			sourcePages = append(sourcePages, p)
		} else {
			otherPages = append(otherPages, p)
		}
	}
	var results []KBResult
	if sourceLimit > 0 {
		results = append(results, scoredToResults(scoreCandidates(sourcePages, query, sourceLimit, threshold))...)
	}
	if otherLimit > 0 {
		results = append(results, scoredToResults(scoreCandidates(otherPages, query, otherLimit, threshold))...)
	}
	return results
}

// searchWikiByVector is the embedding + reranker path. It embeds the query,
// cosine-scores every stored wiki page vector, takes the top candidates,
// then optionally re-ranks with the cross-encoder. Returns nil on any
// failure so Search falls back to the keyword path. Unlike searchWikiByType
// it does not split source/other buckets — reranker relevance surfaces the
// right mix on its own.
func (s *KBStore) searchWikiByVector(ctx context.Context, agentID, query string, limit, preFilterLimit int, threshold float64) []KBResult {
	qvecs, err := s.embedder.Embed(ctx, []string{query})
	if err != nil || len(qvecs) != 1 {
		return nil
	}
	q := qvecs[0]
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.page_id, e.embedding, wp.title, wp.summary, wp.body, wp.page_type, wp.slug
		 FROM wiki_page_embeddings e
		 JOIN wiki_pages wp ON wp.id = e.page_id
		 WHERE e.agent_id = `+s.ph(1), agentID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	type cand struct {
		id, title, summary, body, pageType string
		score                              float64
	}
	var cands []cand
	for rows.Next() {
		var pid string
		var blob []byte
		var title, summary, body, pageType, slug string
		if err := rows.Scan(&pid, &blob, &title, &summary, &body, &pageType, &slug); err != nil {
			return nil
		}
		vec := kbFloat32FromBlob(blob)
		if len(vec) != len(q) {
			continue // dim mismatch (stale vector from a different model)
		}
		cands = append(cands, cand{pid, title, summary, body, pageType, kbCosine(q, vec)})
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })

	// Re-rank the top candidates with the cross-encoder when available; its
	// normalised 0..1 score replaces cosine so the threshold gate and
	// downstream ranking consume the same scale.
	pool := cands
	if preFilterLimit > 0 && len(pool) > preFilterLimit*2 {
		pool = pool[:preFilterLimit*2]
	}
	if s.reranker != nil && s.reranker.Available() && len(pool) > 1 {
		docs := make([]string, len(pool))
		for i, c := range pool {
			docs[i] = c.summary
			if docs[i] == "" {
				docs[i] = c.title
			}
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
		content := c.summary
		if content == "" {
			content = c.body
		}
		snippet := content
		if len(snippet) > 300 {
			snippet = softClipUTF8(snippet, 300)
		}
		results = append(results, KBResult{
			SourceID:    c.id,
			SourceTitle: c.title,
			SourceKind:  "wiki",
			PageType:    c.pageType,
			Content:     content,
			Snippet:     snippet,
			Rank:        c.score,
		})
		if len(results) >= limit {
			break
		}
	}
	return results
}

// kbFloat32ToBlob encodes a float32 vector as a little-endian byte slice.
func kbFloat32ToBlob(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

// kbFloat32FromBlob decodes a little-endian float32 BLOB into a vector.
// Mirrors wiki.float32FromBlob / store.float32FromBlob.
func kbFloat32FromBlob(buf []byte) []float32 {
	if len(buf) == 0 || len(buf)%4 != 0 {
		return nil
	}
	vec := make([]float32, len(buf)/4)
	for i := range vec {
		bits := binary.LittleEndian.Uint32(buf[i*4:])
		vec[i] = math.Float32frombits(bits)
	}
	return vec
}

// kbCosine is cosine similarity over float32 vectors.
func kbCosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
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

func (s *KBStore) searchLike(ctx context.Context, agentID, query string, sourceIDs []string, limit int) ([]KBResult, error) {
	// Token-based LIKE: match any query token (word or CJK bigram) in content
	// or title. A whole-query LIKE barely matched because user/model queries
	// are rarely verbatim substrings of the stored chunks. When query is
	// empty, only the source_id filter applies — pull all chunks of the given
	// sources ordered by chunk_index.
	tokens := tokenize(query)
	if len(tokens) == 0 && query != "" {
		tokens = []string{query}
	}
	var clauses []string
	var args []interface{}
	phIdx := 0
	for _, t := range tokens {
		pat := "%" + t + "%"
		phIdx++
		cPH := s.ph(phIdx)
		phIdx++
		tPH := s.ph(phIdx)
		clauses = append(clauses, fmt.Sprintf("(e.content LIKE %s OR s.title LIKE %s)", cPH, tPH))
		args = append(args, pat, pat)
	}
	phIdx++
	agentPH := s.ph(phIdx)
	args = append(args, agentID)

	var sourceCond string
	if len(sourceIDs) > 0 {
		sourcePHs := make([]string, len(sourceIDs))
		for i, sid := range sourceIDs {
			phIdx++
			sourcePHs[i] = s.ph(phIdx)
			args = append(args, sid)
		}
		sourceCond = fmt.Sprintf(" AND e.source_id IN (%s)", strings.Join(sourcePHs, ","))
	}

	phIdx++
	limitPH := s.ph(phIdx)
	args = append(args, limit)

	where := fmt.Sprintf("e.agent_id = %s%s", agentPH, sourceCond)
	if len(clauses) > 0 {
		where = fmt.Sprintf("(%s) AND %s", strings.Join(clauses, " OR "), where)
	}
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT e.source_id, COALESCE(s.title, s.source_ref, ''), e.chunk_index, e.content,
			'' AS snippet, 0.0 AS rank
		FROM kb_entries e
		JOIN kb_sources s ON s.id = e.source_id
		WHERE %s
		ORDER BY e.chunk_index
		LIMIT %s`,
			where, limitPH),
		args...)
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

// SaveEntryEmbedding upserts the embedding for one raw kb_entries chunk.
func (s *KBStore) SaveEntryEmbedding(ctx context.Context, agentID string, entryID int, vec []float32, model string) error {
	q := `INSERT INTO kb_entry_embeddings (entry_id, agent_id, embedding, dim, model, updated_at)
		VALUES (` + s.ph(1) + `,` + s.ph(2) + `,` + s.ph(3) + `,` + s.ph(4) + `,` + s.ph(5) + `,CURRENT_TIMESTAMP)
		ON CONFLICT(entry_id) DO UPDATE SET agent_id=excluded.agent_id, embedding=excluded.embedding,
			dim=excluded.dim, model=excluded.model, updated_at=CURRENT_TIMESTAMP`
	_, err := s.db.ExecContext(ctx, q, entryID, agentID, kbFloat32ToBlob(vec), len(vec), model)
	return err
}

// ClearEntryEmbeddingsForAgent deletes every kb_entry_embeddings row for an agent.
func (s *KBStore) ClearEntryEmbeddingsForAgent(ctx context.Context, agentID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kb_entry_embeddings WHERE agent_id = `+s.ph(1), agentID)
	return err
}

// searchRawByVector is the embedding path for raw kb_entries chunks: embed the
// query, cosine-score every stored chunk vector within the requested sources,
// then optionally cross-encoder re-rank. Returns nil on any failure so
// SearchRawKB falls back to searchLike. Emits KBResult with SourceKind="kb".
func (s *KBStore) searchRawByVector(ctx context.Context, agentID, query string, sourceIDs []string, limit int) []KBResult {
	qvecs, err := s.embedder.Embed(ctx, []string{query})
	if err != nil || len(qvecs) != 1 {
		return nil
	}
	q := qvecs[0]
	var args []interface{}
	args = append(args, agentID)
	sourceCond := ""
	if len(sourceIDs) > 0 {
		phs := make([]string, len(sourceIDs))
		for i, sid := range sourceIDs {
			phs[i] = s.ph(i + 2)
			args = append(args, sid)
		}
		sourceCond = " AND ke.source_id IN (" + strings.Join(phs, ",") + ")"
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.embedding, ke.content, ke.source_id, ke.chunk_index, COALESCE(s.title, s.source_ref, '')
		 FROM kb_entry_embeddings e
		 JOIN kb_entries ke ON ke.id = e.entry_id
		 JOIN kb_sources s ON s.id = ke.source_id
		 WHERE e.agent_id = `+s.ph(1)+sourceCond, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	type cand struct {
		content, sourceID, title string
		chunk                    int
		score                    float64
	}
	var cands []cand
	for rows.Next() {
		var blob []byte
		var c cand
		if err := rows.Scan(&blob, &c.content, &c.sourceID, &c.chunk, &c.title); err != nil {
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
		snippet := c.content
		if len(snippet) > 300 {
			snippet = softClipUTF8(snippet, 300)
		}
		results = append(results, KBResult{
			SourceID:    c.sourceID,
			SourceTitle: c.title,
			SourceKind:  "kb",
			ChunkIndex:  c.chunk,
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

// embedSourceEntries embeds every chunk of one source (best-effort, called
// asynchronously after IngestText so the embed API never blocks content ingest).
func (s *KBStore) embedSourceEntries(ctx context.Context, agentID, sourceID string) {
	if s.embedder == nil || !s.embedder.Available() {
		return
	}
	entries, err := s.ListEntries(ctx, agentID, sourceID, 10000, 0)
	if err != nil || len(entries) == 0 {
		return
	}
	texts := make([]string, len(entries))
	for i, e := range entries {
		texts[i] = e.Content
	}
	vecs, err := s.embedder.Embed(ctx, texts)
	if err != nil || len(vecs) != len(entries) {
		return
	}
	for i, e := range entries {
		_ = s.SaveEntryEmbedding(ctx, agentID, e.ID, vecs[i], s.embedder.Model())
	}
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

func formatResults(results []KBResult, query string, ids []string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d results for %q:\n\n", len(results), query))
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("--- [%s] Result %d (source_id: %s, source: %s, chunk %d) ---\n", ids[i], i+1, r.SourceID, r.SourceTitle, r.ChunkIndex))
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
	switch {
	case len(ids) >= 2:
		sb.WriteString(fmt.Sprintf("When you use a fact from these results, cite it inline with the bracketed id, e.g. [%s]; multiple sources: [%s][%s].\n", ids[0], ids[0], ids[1]))
	case len(ids) == 1:
		sb.WriteString(fmt.Sprintf("When you use a fact from these results, cite it inline with the bracketed id, e.g. [%s].\n", ids[0]))
	default:
		sb.WriteString("When you use a fact from these results, cite it inline with the bracketed id.\n")
	}
	return sb.String()
}
