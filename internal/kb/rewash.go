package kb

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Pipeline versioning, the MimirQ pointer-flip idea adapted to a system
// that doesn't retain originals: every IngestText source keeps its raw
// pre-governance text (kb_sources.orig_content) plus the pipeline hash
// that produced its chunks. When the governance rules or the chunker
// change, RewashSources re-derives chunks from the retained original in
// one transaction and marks the source wiki-dirty — rollback is "restore
// the old rows from backup", forward-only is the designed path. Legacy
// sources without orig_content report MissingOrigin and are skipped:
// nothing honest can be re-derived from post-hoc chunks.

// chunkerVersion is the chunker's BEHAVIOR version — bumped manually when
// ChunkText's splitting changes. Kept manual because behavior can't be
// hashed; the governance rules below are hashed directly instead of
// carrying a manual version, so a rule edit can never be forgotten.
const chunkerVersion = "v1"

// PipelineHash identifies the current ingest pipeline by hashing what it
// IS, not what a human remembered to bump: the compiled governance rules
// (name + pattern source + replacement, in order) plus the chunker
// version and parameters. Editing any rule changes the hash, so stale
// sources are detected by construction.
func PipelineHash() string {
	h := sha256.New()
	fmt.Fprintf(h, "chunker:%s:%d/%d", chunkerVersion, defaultChunkSize, defaultOverlap)
	for _, rules := range [][]govRule{govBaseRules, govWebRules} {
		for _, r := range rules {
			fmt.Fprintf(h, "|%s\x00%s\x00%s", r.name, r.re.String(), r.repl)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:8])
}

// chunkOffsets locates each chunk's span in text (see IngestText for the
// line-by-line rationale). Shared by ingest and preview so both report
// identical positions.
func chunkOffsets(content string, chunks []Chunk) [][2]int {
	offsets := make([][2]int, len(chunks))
	searchFrom := 0
	for i, c := range chunks {
		lines := strings.Split(c.Content, "\n")
		pos := strings.Index(content[searchFrom:], lines[0])
		if pos < 0 {
			offsets[i] = [2]int{-1, -1}
			continue
		}
		start := searchFrom + pos
		end := start + len(lines[0])
		for _, ln := range lines[1:] {
			p := strings.Index(content[end:], ln)
			if p < 0 {
				end = -1
				break
			}
			end = end + p + len(ln)
		}
		if end < 0 {
			offsets[i] = [2]int{-1, -1}
			continue
		}
		offsets[i] = [2]int{start, end}
		searchFrom = start
	}
	return offsets
}

// RewashReport summarizes one rewash scan.
type RewashReport struct {
	Scanned       int `json:"scanned"`
	WouldChange   int `json:"would_change"`
	Rewashed      int `json:"rewashed"`
	SkippedFresh  int `json:"skipped_fresh"`  // hash already current
	SkippedEmpty  int `json:"skipped_empty"`  // current rules would produce zero chunks
	MissingOrigin int `json:"missing_origin"` // legacy rows: no orig retained
}

// RewashSources scans the agent's article sources and, for each whose
// pipeline_hash differs from the current pipeline, re-derives chunks from
// the retained original. apply=false is the dry run (counts only); the
// default should ALWAYS be a dry run first — the owner eyeballs
// PreviewIngest output for a representative source before committing.
//
// Apply path: one transaction replaces the chunks, updates source
// metadata, and stamps wiki_dirty_at so the running daemon's wiki autogen
// regenerates pages from the new chunks. Embeddings re-run async
// best-effort after commit, mirroring IngestText.
func (s *KBStore) RewashSources(ctx context.Context, agentID string, apply bool) (*RewashReport, error) {
	rep := &RewashReport{}
	current := PipelineHash()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, source_type, source_ref, orig_content, pipeline_hash
		 FROM kb_sources WHERE agent_id = `+s.ph(1)+` AND (type = 'article' OR type = '')`,
		agentID)
	if err != nil {
		return nil, err
	}
	type src struct {
		id, sourceType, sourceRef, orig, hash string
	}
	var sources []src
	for rows.Next() {
		var v src
		if err := rows.Scan(&v.id, &v.sourceType, &v.sourceRef, &v.orig, &v.hash); err != nil {
			rows.Close()
			return nil, err
		}
		sources = append(sources, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, v := range sources {
		rep.Scanned++
		if v.orig == "" {
			rep.MissingOrigin++
			continue
		}
		if v.hash == current {
			rep.SkippedFresh++
			continue
		}
		governed, _ := GovernContent(v.orig, v.sourceType)
		chunks := ChunkText(governed, 0, 0)
		if len(chunks) == 0 {
			// The current rules would reject this source entirely (e.g.
			// all-boilerplate). Rewashing to zero chunks is destructive —
			// skip and let the owner decide (delete or exempt).
			rep.SkippedEmpty++
			continue
		}
		rep.WouldChange++
		if !apply {
			continue
		}
		if err := s.rewashOne(ctx, agentID, v.id, governed, chunks); err != nil {
			return rep, fmt.Errorf("rewash %s: %w", v.id, err)
		}
		rep.Rewashed++
	}
	return rep, nil
}

// rewashOne replaces one source's chunks in a single transaction.
func (s *KBStore) rewashOne(ctx context.Context, agentID, sourceID, governed string, chunks []Chunk) error {
	offsets := chunkOffsets(governed, chunks)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM kb_entries WHERE source_id = `+s.ph(1), sourceID); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx,
		fmt.Sprintf(`INSERT INTO kb_entries (uuid, agent_id, source_id, chunk_index, content, start_offset, end_offset)
			VALUES (%s, %s, %s, %s, %s, %s, %s)`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5), s.ph(6), s.ph(7)))
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, c := range chunks {
		if _, err := stmt.ExecContext(ctx, uuid.New().String(), agentID, sourceID, c.Index, c.Content, offsets[i][0], offsets[i][1]); err != nil {
			return err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx,
		`UPDATE kb_sources SET entry_count = `+s.ph(1)+`, total_chars = `+s.ph(2)+`,
			pipeline_hash = `+s.ph(3)+`, wiki_dirty_at = `+s.ph(4)+`, updated_at = `+s.ph(5)+`
		WHERE id = `+s.ph(6),
		len(chunks), len(governed), PipelineHash(), now, now, sourceID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.embedder != nil && s.embedder.Available() {
		go s.embedSourceEntries(context.Background(), agentID, sourceID)
	}
	return nil
}
