package wiki

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

type WikiStore struct {
	db      *sql.DB
	dialect string
}

func NewWikiStore(db *sql.DB, dialect string) *WikiStore {
	return &WikiStore{db: db, dialect: dialect}
}

func (s *WikiStore) ph(n int) string {
	if s.dialect == "postgres" {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// --- Upsert ---

func (s *WikiStore) UpsertPage(ctx context.Context, p *WikiPage) error {
	srcJSON, _ := json.Marshal(p.SourceIDs)
	tagsJSON, _ := json.Marshal(p.Tags)
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now

	upsert := `INSERT INTO wiki_pages (id, agent_id, page_type, slug, title, body, summary, source_ids, tags, created_at, updated_at, revision)
		VALUES (` + s.ph(1) + `,` + s.ph(2) + `,` + s.ph(3) + `,` + s.ph(4) + `,` + s.ph(5) + `,` + s.ph(6) + `,` + s.ph(7) + `,` + s.ph(8) + `,` + s.ph(9) + `,` + s.ph(10) + `,` + s.ph(11) + `,1)
		ON CONFLICT(id) DO UPDATE SET title=excluded.title, body=excluded.body, summary=excluded.summary,
			source_ids=excluded.source_ids, tags=excluded.tags, updated_at=excluded.updated_at,
			revision=wiki_pages.revision+1`

	_, err := s.db.ExecContext(ctx, upsert,
		p.ID, p.AgentID, p.PageType, p.Slug, p.Title, p.Body, p.Summary,
		string(srcJSON), string(tagsJSON), p.CreatedAt, p.UpdatedAt)
	return err
}

func (s *WikiStore) UpsertLink(ctx context.Context, l *WikiLink) error {
	q := `INSERT INTO wiki_links (src_page_id, dst_page_id, relation, weight)
		VALUES (` + s.ph(1) + `,` + s.ph(2) + `,` + s.ph(3) + `,` + s.ph(4) + `)
		ON CONFLICT(src_page_id, dst_page_id) DO UPDATE SET relation=excluded.relation, weight=excluded.weight`

	_, err := s.db.ExecContext(ctx, q, l.SrcPageID, l.DstPageID, l.Relation, l.Weight)
	return err
}

// --- Read ---

func (s *WikiStore) GetPage(ctx context.Context, id string) (*WikiPage, error) {
	q := `SELECT id, agent_id, page_type, slug, title, body, summary, source_ids, tags, created_at, updated_at, revision
		FROM wiki_pages WHERE id = ` + s.ph(1)

	row := s.db.QueryRowContext(ctx, q, id)
	p := &WikiPage{}
	var srcJSON, tagsJSON string
	err := row.Scan(&p.ID, &p.AgentID, &p.PageType, &p.Slug, &p.Title, &p.Body, &p.Summary,
		&srcJSON, &tagsJSON, &p.CreatedAt, &p.UpdatedAt, &p.Revision)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(srcJSON), &p.SourceIDs)
	json.Unmarshal([]byte(tagsJSON), &p.Tags)
	return p, nil
}

// GetPageBySlug resolves a wiki [[type:slug]] link to a page. Wiki links
// carry human-readable "page_type:slug" pairs (not UUIDs); the renderer
// splits them and the handler calls this to fetch the target page. Returns
// (nil, nil) on no match so the handler can 404 cleanly.
func (s *WikiStore) GetPageBySlug(ctx context.Context, agentID, pageType, slug string) (*WikiPage, error) {
	q := `SELECT id, agent_id, page_type, slug, title, body, summary, source_ids, tags, created_at, updated_at, revision
		FROM wiki_pages WHERE agent_id = ` + s.ph(1) + ` AND page_type = ` + s.ph(2) + ` AND slug = ` + s.ph(3) + ` LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, agentID, pageType, slug)
	p := &WikiPage{}
	var srcJSON, tagsJSON string
	err := row.Scan(&p.ID, &p.AgentID, &p.PageType, &p.Slug, &p.Title, &p.Body, &p.Summary,
		&srcJSON, &tagsJSON, &p.CreatedAt, &p.UpdatedAt, &p.Revision)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(srcJSON), &p.SourceIDs)
	json.Unmarshal([]byte(tagsJSON), &p.Tags)
	return p, nil
}

// FindPageByTitle returns an existing page with the given title for the agent.
func (s *WikiStore) FindPageByTitle(ctx context.Context, agentID, title string) (*WikiPage, error) {
	q := `SELECT id, agent_id, page_type, slug, title, body, summary, source_ids, tags, created_at, updated_at, revision
		FROM wiki_pages WHERE agent_id = ` + s.ph(1) + ` AND title = ` + s.ph(2) + ` LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, agentID, title)
	var p WikiPage
	var srcJSON, tagsJSON string
	var createdAt, updatedAt string
	err := row.Scan(&p.ID, &p.AgentID, &p.PageType, &p.Slug, &p.Title, &p.Body, &p.Summary, &srcJSON, &tagsJSON, &createdAt, &updatedAt, &p.Revision)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(srcJSON), &p.SourceIDs)
	json.Unmarshal([]byte(tagsJSON), &p.Tags)
	p.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	p.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &p, nil
}

func (s *WikiStore) ListPages(ctx context.Context, agentID, pageType string, limit, offset int) ([]WikiPage, int, error) {
	var where []string
	var args []any
	argN := 1

	where = append(where, "agent_id = "+s.ph(argN))
	args = append(args, agentID)
	argN++

	if pageType != "" {
		where = append(where, "page_type = "+s.ph(argN))
		args = append(args, pageType)
		argN++
	}

	whereClause := strings.Join(where, " AND ")

	// Count
	var total int
	countQ := `SELECT COUNT(*) FROM wiki_pages WHERE ` + whereClause
	if err := s.db.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// List (body omitted for performance)
	q := `SELECT id, agent_id, page_type, slug, title, summary, source_ids, tags, created_at, updated_at, revision
		FROM wiki_pages WHERE ` + whereClause + ` ORDER BY updated_at DESC LIMIT ` + s.ph(argN) + ` OFFSET ` + s.ph(argN+1)
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	pages := make([]WikiPage, 0)
	for rows.Next() {
		var p WikiPage
		var srcJSON, tagsJSON string
		if err := rows.Scan(&p.ID, &p.AgentID, &p.PageType, &p.Slug, &p.Title, &p.Summary,
			&srcJSON, &tagsJSON, &p.CreatedAt, &p.UpdatedAt, &p.Revision); err != nil {
			return nil, 0, err
		}
		json.Unmarshal([]byte(srcJSON), &p.SourceIDs)
		json.Unmarshal([]byte(tagsJSON), &p.Tags)
		pages = append(pages, p)
	}
	return pages, total, nil
}

// ListUncardedPages returns wiki pages never yet fed to a cardsgen pass
// (carded_at IS NULL), newest-updated first. The cardsgen material picker:
// the backlog drains newest-first across successive runs instead of only
// catching pages touched on one specific day.
func (s *WikiStore) ListUncardedPages(ctx context.Context, agentID string, limit int) ([]WikiPage, error) {
	q := `SELECT id, agent_id, page_type, slug, title, summary, source_ids, tags, created_at, updated_at, revision
		FROM wiki_pages WHERE agent_id = ` + s.ph(1) + ` AND carded_at IS NULL
		ORDER BY updated_at DESC LIMIT ` + s.ph(2)

	rows, err := s.db.QueryContext(ctx, q, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	pages := make([]WikiPage, 0)
	for rows.Next() {
		var p WikiPage
		var srcJSON, tagsJSON string
		if err := rows.Scan(&p.ID, &p.AgentID, &p.PageType, &p.Slug, &p.Title, &p.Summary,
			&srcJSON, &tagsJSON, &p.CreatedAt, &p.UpdatedAt, &p.Revision); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(srcJSON), &p.SourceIDs)
		json.Unmarshal([]byte(tagsJSON), &p.Tags)
		pages = append(pages, p)
	}
	return pages, rows.Err()
}

// MarkPagesCarded stamps carded_at on the given pages so later cardsgen
// passes skip them. No-op on an empty id list.
func (s *WikiStore) MarkPagesCarded(ctx context.Context, agentID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	phs := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, agentID)
	for i, id := range ids {
		phs[i] = s.ph(i + 2)
		args = append(args, id)
	}
	q := `UPDATE wiki_pages SET carded_at = CURRENT_TIMESTAMP
		WHERE agent_id = ` + s.ph(1) + ` AND id IN (` + strings.Join(phs, ",") + `)`
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// --- Graph ---

func (s *WikiStore) GetGraph(ctx context.Context, agentID string) (*WikiGraph, error) {
	pages, _, err := s.ListPages(ctx, agentID, "", 500, 0)
	if err != nil {
		return nil, err
	}

	q := `SELECT wl.src_page_id, wl.dst_page_id, wl.relation, wl.weight
		FROM wiki_links wl JOIN wiki_pages wp ON wl.src_page_id = wp.id
		WHERE wp.agent_id = ` + s.ph(1) + ` LIMIT 2000`

	rows, err := s.db.QueryContext(ctx, q, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []WikiLink
	for rows.Next() {
		var l WikiLink
		if err := rows.Scan(&l.SrcPageID, &l.DstPageID, &l.Relation, &l.Weight); err != nil {
			return nil, err
		}
		edges = append(edges, l)
	}
	return &WikiGraph{Nodes: pages, Edges: edges}, nil
}

func (s *WikiStore) GetStats(ctx context.Context, agentID string) (*WikiStats, error) {
	q := `SELECT page_type, COUNT(*) FROM wiki_pages WHERE agent_id = ` + s.ph(1) + ` GROUP BY page_type`
	rows, err := s.db.QueryContext(ctx, q, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)
	total := 0
	for rows.Next() {
		var pt string
		var c int
		if err := rows.Scan(&pt, &c); err != nil {
			return nil, err
		}
		counts[pt] = c
		total += c
	}

	var edgeCount int
	edgeQ := `SELECT COUNT(*) FROM wiki_links wl JOIN wiki_pages wp ON wl.src_page_id = wp.id WHERE wp.agent_id = ` + s.ph(1)
	if err := s.db.QueryRowContext(ctx, edgeQ, agentID).Scan(&edgeCount); err != nil {
		return nil, err
	}

	return &WikiStats{PageCounts: counts, TotalPages: total, TotalEdges: edgeCount}, nil
}

// DeletePagesBySource removes all wiki pages that include the given KB source ID.
// Call before regenerating to avoid duplicate pages.
func (s *WikiStore) DeletePagesBySource(ctx context.Context, agentID, sourceID string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Find pages containing this source_id
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM wiki_pages WHERE agent_id = `+s.ph(1)+` AND source_ids LIKE `+s.ph(2),
		agentID, "%\""+sourceID+"\"%")
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()

	var deleted int64
	for _, id := range ids {
		tx.Exec(`DELETE FROM wiki_links WHERE src_page_id = `+s.ph(1)+` OR dst_page_id = `+s.ph(2), id, id)
		if res, err := tx.Exec(`DELETE FROM wiki_pages WHERE id = `+s.ph(1), id); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				deleted += n
			}
		}
	}

	tx.Commit()
	return deleted, nil
}

// --- Delete ---

func (s *WikiStore) DeletePage(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	tx.Exec(`DELETE FROM wiki_links WHERE src_page_id = `+s.ph(1)+` OR dst_page_id = `+s.ph(2), id, id)
	tx.Exec(`DELETE FROM wiki_page_embeddings WHERE page_id = `+s.ph(1), id)
	_, err = tx.Exec(`DELETE FROM wiki_pages WHERE id = `+s.ph(1), id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// --- Page embeddings (vector-processing stage 2) ---

// float32ToBlob encodes a float32 vector as a little-endian byte slice for
// BLOB storage. Mirrors internal/store.float32ToBlob.
func float32ToBlob(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

// float32FromBlob is the inverse of float32ToBlob. Returns nil for empty or
// mis-sized input.
func float32FromBlob(buf []byte) []float32 {
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

// PageEmbedding pairs a wiki page ID with its decoded embedding vector.
type PageEmbedding struct {
	PageID string
	Vec    []float32
}

// SavePageEmbedding upserts the embedding vector for a wiki page.
func (s *WikiStore) SavePageEmbedding(ctx context.Context, agentID, pageID string, vec []float32, model string) error {
	q := `INSERT INTO wiki_page_embeddings (page_id, agent_id, embedding, dim, model, updated_at)
		VALUES (` + s.ph(1) + `,` + s.ph(2) + `,` + s.ph(3) + `,` + s.ph(4) + `,` + s.ph(5) + `,CURRENT_TIMESTAMP)
		ON CONFLICT(page_id) DO UPDATE SET agent_id=excluded.agent_id, embedding=excluded.embedding,
			dim=excluded.dim, model=excluded.model, updated_at=CURRENT_TIMESTAMP`
	_, err := s.db.ExecContext(ctx, q, pageID, agentID, float32ToBlob(vec), len(vec), model)
	return err
}

// ClearPageEmbeddingsForAgent deletes every embedding row for an agent
// (the "force re-embed" path clears before re-vectorizing all pages).
func (s *WikiStore) ClearPageEmbeddingsForAgent(ctx context.Context, agentID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM wiki_page_embeddings WHERE agent_id = `+s.ph(1), agentID)
	return err
}

// ListPageEmbeddingsByAgent returns all stored (page_id, vector) pairs for
// an agent. Vectors decode in-memory; cosine scoring happens in the caller,
// keeping SQL simple and dialect-agnostic (no vec0/pgvector dependency).
func (s *WikiStore) ListPageEmbeddingsByAgent(ctx context.Context, agentID string) ([]PageEmbedding, error) {
	q := `SELECT page_id, embedding FROM wiki_page_embeddings WHERE agent_id = ` + s.ph(1)
	rows, err := s.db.QueryContext(ctx, q, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PageEmbedding
	for rows.Next() {
		var pid string
		var blob []byte
		if err := rows.Scan(&pid, &blob); err != nil {
			return nil, err
		}
		vec := float32FromBlob(blob)
		if len(vec) == 0 {
			continue
		}
		out = append(out, PageEmbedding{PageID: pid, Vec: vec})
	}
	return out, rows.Err()
}
