package kb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// Default dedup thresholds applied when KBCfg leaves a field at zero. Picked
// conservative (high) so only near-duplicates trip; calibrate from real
// duplicate-pair scores once there's data.
const (
	DefaultArticleDupHigh    = 0.90
	DefaultArticleDupMid     = 0.72
	DefaultFlashDupThreshold = 0.85
	DefaultTodoDupThreshold  = 0.78
)

// PendingDefaultTTL is how long a mid-tier article pend stays rescuable
// before the expiry sweep reclaims it.
const PendingDefaultTTL = 24 * time.Hour

// ResolveArticleDupHigh/Mid resolve a cfg value against the default.
func ResolveArticleDupHigh(v float64) float64 {
	if v > 0 {
		return v
	}
	return DefaultArticleDupHigh
}

func ResolveArticleDupMid(v float64) float64 {
	if v > 0 {
		return v
	}
	return DefaultArticleDupMid
}

func ResolveFlashDupThreshold(v float64) float64 {
	if v > 0 {
		return v
	}
	return DefaultFlashDupThreshold
}

func ResolveTodoDupThreshold(v float64) float64 {
	if v > 0 {
		return v
	}
	return DefaultTodoDupThreshold
}

// SetDedupCfgFn wires an optional per-agent dedup config supplier. When set,
// the Dup* helpers below read live thresholds from it; otherwise built-in
// defaults apply. Injected by the caller (manager / setup) from config.
func (s *KBStore) SetDedupCfgFn(fn func() KBCfg) { s.dedupCfgFn = fn }

func (s *KBStore) dedupCfg() KBCfg {
	if s.dedupCfgFn != nil {
		return s.dedupCfgFn()
	}
	return KBCfg{}
}

// DupArticleHigh/Mid/Flash/Todo resolve the active dedup threshold for the
// store, honoring a wired config override, else the built-in default.
func (s *KBStore) DupArticleHigh() float64 { return ResolveArticleDupHigh(s.dedupCfg().ArticleDupHigh) }
func (s *KBStore) DupArticleMid() float64  { return ResolveArticleDupMid(s.dedupCfg().ArticleDupMid) }
func (s *KBStore) DupFlash() float64       { return ResolveFlashDupThreshold(s.dedupCfg().FlashDupThreshold) }
func (s *KBStore) DupTodo() float64        { return ResolveTodoDupThreshold(s.dedupCfg().TodoDupThreshold) }

// EncodeSeqRanges encodes a single seq as a JSON-style range array
// "[[seq,seq]]"; empty when seq<=0 so legacy rows / non-chat writes stay "".
func EncodeSeqRanges(seq int) string {
	if seq <= 0 {
		return ""
	}
	return fmt.Sprintf("[[%d,%d]]", seq, seq)
}

// SeqRange is a [lo,hi] inclusive message-seq span a KB entry was captured from.
type SeqRange [2]int

// ParseSeqRanges decodes source_seq_ranges JSON ("[[1,10],[14,14]]"); nil on
// empty/unparseable — callers treat nil as "no seq provenance".
func ParseSeqRanges(s string) []SeqRange {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var rs []SeqRange
	if err := json.Unmarshal([]byte(s), &rs); err != nil {
		return nil
	}
	return rs
}

// SeqRangesOverlap reports whether any range in a intersects any in b. Used
// by L1: an incoming write whose seq falls inside an existing entry's capture
// span is a rewrite of already-captured content.
func SeqRangesOverlap(a, b []SeqRange) bool {
	for _, x := range a {
		for _, y := range b {
			lo := x[0]
			if y[0] > lo {
				lo = y[0]
			}
			hi := x[1]
			if y[1] < hi {
				hi = y[1]
			}
			if lo <= hi {
				return true
			}
		}
	}
	return false
}

// SimilarHit is a same-type KB source close to a candidate write.
type SimilarHit struct {
	SourceID string
	Title    string
	KbType   string
	Score    float64
}

// FindSimilar returns same-type KB sources whose chunks are cosine-similar to
// content at or above threshold. Mirrors searchFlashTodoByVector's join +
// cosine + threshold shape, but takes kbType as a parameter so all three
// content types share one recall path, and rolls chunk scores up to one hit
// per source (top chunk wins). Returns nil when the embedder is unavailable
// or no candidate clears threshold.
func (s *KBStore) FindSimilar(ctx context.Context, agentID, content, kbType string, threshold float64, limit int) []SimilarHit {
	if limit <= 0 {
		limit = 5
	}
	if s.embedder == nil || !s.embedder.Available() || threshold <= 0 {
		return nil
	}
	qvecs, err := s.embedder.Embed(ctx, []string{content})
	if err != nil || len(qvecs) != 1 {
		return nil
	}
	q := qvecs[0]
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.embedding, ke.source_id, COALESCE(s.title,''), s.type
		 FROM kb_entry_embeddings e
		 JOIN kb_entries ke ON ke.id = e.entry_id
		 JOIN kb_sources s ON s.id = ke.source_id
		 WHERE e.agent_id = `+s.ph(1)+` AND s.type = `+s.ph(2),
		agentID, kbType)
	if err != nil {
		return nil
	}
	defer rows.Close()
	type cand struct {
		sourceID, title, kbType string
		score                   float64
	}
	best := map[string]*cand{}
	for rows.Next() {
		var blob []byte
		var sourceID, title, t string
		if err := rows.Scan(&blob, &sourceID, &title, &t); err != nil {
			return nil
		}
		vec := kbFloat32FromBlob(blob)
		if len(vec) != len(q) {
			continue
		}
		score := kbCosine(q, vec)
		if c, ok := best[sourceID]; !ok || score > c.score {
			best[sourceID] = &cand{sourceID, title, t, score}
		}
	}
	if len(best) == 0 {
		return nil
	}
	cands := make([]*cand, 0, len(best))
	for _, c := range best {
		cands = append(cands, c)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	hits := make([]SimilarHit, 0, limit)
	for _, c := range cands {
		if c.score < threshold {
			continue
		}
		hits = append(hits, SimilarHit{SourceID: c.sourceID, Title: c.title, KbType: c.kbType, Score: c.score})
		if len(hits) >= limit {
			break
		}
	}
	return hits
}

// PendingEntry is a KB write parked at the mid dedup tier, awaiting the
// user's merge / create-new / skip choice.
type PendingEntry struct {
	ID                string    `json:"id"`
	AgentID           string    `json:"agent_id"`
	KbType            string    `json:"kb_type"`
	Title             string    `json:"title"`
	Content           string    `json:"content"`
	SourceType        string    `json:"source_type"`
	SourceRef         string    `json:"source_ref"`
	SourceSessionID   string    `json:"source_session_id,omitempty"`
	SourceSeqRanges   string    `json:"source_seq_ranges,omitempty"`
	CandidateSourceID string    `json:"candidate_source_id"`
	CandidateTitle    string    `json:"candidate_title"`
	Similarity        float64   `json:"similarity"`
	CreatedAt         time.Time `json:"created_at"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// SavePending inserts a pending entry row.
func (s *KBStore) SavePending(ctx context.Context, p *PendingEntry) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kb_pending_entries (id, agent_id, kb_type, title, content, source_type, source_ref, source_session_id, source_seq_ranges, candidate_source_id, candidate_title, similarity, created_at, expires_at)
		 VALUES (`+s.ph(1)+`,`+s.ph(2)+`,`+s.ph(3)+`,`+s.ph(4)+`,`+s.ph(5)+`,`+s.ph(6)+`,`+s.ph(7)+`,`+s.ph(8)+`,`+s.ph(9)+`,`+s.ph(10)+`,`+s.ph(11)+`,`+s.ph(12)+`,`+s.ph(13)+`,`+s.ph(14)+`)`,
		p.ID, p.AgentID, p.KbType, p.Title, p.Content, p.SourceType, p.SourceRef, p.SourceSessionID, p.SourceSeqRanges, p.CandidateSourceID, p.CandidateTitle, p.Similarity, p.CreatedAt.UTC().Format(time.RFC3339), p.ExpiresAt.UTC().Format(time.RFC3339))
	return err
}

// GetPending loads a pending entry by id+agent. Returns (nil, nil) when the
// row doesn't exist so callers can treat absence as "not pending".
func (s *KBStore) GetPending(ctx context.Context, id, agentID string) (*PendingEntry, error) {
	var p PendingEntry
	var created, expires string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, agent_id, kb_type, title, content, source_type, source_ref, source_session_id, source_seq_ranges, candidate_source_id, candidate_title, similarity, created_at, expires_at
		 FROM kb_pending_entries WHERE id = `+s.ph(1)+` AND agent_id = `+s.ph(2),
		id, agentID).Scan(&p.ID, &p.AgentID, &p.KbType, &p.Title, &p.Content, &p.SourceType, &p.SourceRef, &p.SourceSessionID, &p.SourceSeqRanges, &p.CandidateSourceID, &p.CandidateTitle, &p.Similarity, &created, &expires)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt, _ = time.Parse(time.RFC3339, created)
	p.ExpiresAt, _ = time.Parse(time.RFC3339, expires)
	return &p, nil
}

// DeletePending removes a pending entry (after resolve or user skip).
func (s *KBStore) DeletePending(ctx context.Context, id, agentID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM kb_pending_entries WHERE id = `+s.ph(1)+` AND agent_id = `+s.ph(2),
		id, agentID)
	return err
}

// PruneExpiredPending deletes pending rows past their expires_at. Returns
// the count deleted. Called by the dedup sweep alongside other retention.
func (s *KBStore) PruneExpiredPending(ctx context.Context) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM kb_pending_entries WHERE expires_at < `+s.ph(1), now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeriveTitle exports the title derivation SaveFlash/SaveTodo use, so
// callers outside the kb package (setup HTTP handlers) can match it for
// dedup checks without reimplementing the first-line rule.
func DeriveTitle(content string) string {
	return deriveTitle(content)
}

// DupCheckResult reports whether an incoming KB write is blocked by an
// existing same-type source, and by which dedup layer fired.
type DupCheckResult struct {
	Duplicate bool
	Reason    string // "l1-origin" | "l2-vector" | "l3-title"
	SourceID  string
	Title     string
	Score     float64 // L2 cosine; 0 for L1/L3
}

// cjkStopRunes are common Chinese function/grammar runes stripped by the
// heavy L3 title normalize so "我的想法" and "这想法" both collapse toward
// "想法". ASCII stop words need tokenization (not handled — spaces are
// already stripped before this runs).
var cjkStopRunes = map[rune]bool{
	'的': true, '了': true, '是': true, '在': true, '和': true,
	'与': true, '及': true, '或': true, '我': true, '你': true,
	'他': true, '她': true, '它': true, '们': true, '上': true,
	'下': true, '中': true, '这': true, '那': true, '个': true,
	'一': true, '不': true, '也': true, '都': true, '就': true,
	'把': true, '被': true, '让': true, '给': true, '到': true,
}

// normalizeTitle applies the L3 heavy normalize: lowercase, keep only
// letters/digits (drops spaces/punct), and strip common CJK stop runes.
// Two titles that collapse to the same key are treated as identical.
func normalizeTitle(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if cjkStopRunes[r] {
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// CheckDuplicate runs the L1 → L2 → L3 chain for an incoming KB write of the
// given type. L1 (same session+seq) and L3 (normalized title) always run; L2
// (vector) runs only when a vector threshold > 0 is passed. Returns
// DupCheckResult{Duplicate:false} when nothing blocks the write.
func (s *KBStore) CheckDuplicate(ctx context.Context, agentID, kbType, title, content string, vectorThreshold float64) DupCheckResult {
	// L1: same session + overlapping seq range = rewrite of captured content.
	origin := SourceOriginFromCtx(ctx)
	if origin.SessionID != "" && origin.Seq > 0 {
		incoming := []SeqRange{{origin.Seq, origin.Seq}}
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, COALESCE(title,''), COALESCE(source_seq_ranges,'') FROM kb_sources
			 WHERE agent_id = `+s.ph(1)+` AND type = `+s.ph(2)+` AND source_session_id = `+s.ph(3),
			agentID, kbType, origin.SessionID)
		if err == nil {
			for rows.Next() {
				var id, t, seqJSON string
				if scanErr := rows.Scan(&id, &t, &seqJSON); scanErr != nil {
					break
				}
				if SeqRangesOverlap(incoming, ParseSeqRanges(seqJSON)) {
					rows.Close()
					return DupCheckResult{Duplicate: true, Reason: "l1-origin", SourceID: id, Title: t}
				}
			}
			rows.Close()
		}
	}

	// L2: vector similarity (no-op when threshold is 0 / embedder unavailable).
	if vectorThreshold > 0 {
		if hits := s.FindSimilar(ctx, agentID, content, kbType, vectorThreshold, 1); len(hits) > 0 {
			return DupCheckResult{Duplicate: true, Reason: "l2-vector", SourceID: hits[0].SourceID, Title: hits[0].Title, Score: hits[0].Score}
		}
	}

	// L3: normalized-title exact match against same-type sources.
	key := normalizeTitle(title)
	if key != "" {
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, COALESCE(title,'') FROM kb_sources
			 WHERE agent_id = `+s.ph(1)+` AND type = `+s.ph(2),
			agentID, kbType)
		if err == nil {
			for rows.Next() {
				var id, t string
				if scanErr := rows.Scan(&id, &t); scanErr != nil {
					break
				}
				if normalizeTitle(t) == key {
					rows.Close()
					return DupCheckResult{Duplicate: true, Reason: "l3-title", SourceID: id, Title: t}
				}
			}
			rows.Close()
		}
	}

	return DupCheckResult{}
}

// SourceIDByRef returns the id + title of the most recent source whose
// source_ref matches (e.g. a URL), or ("","") when none. Used by L1 URL
// dedup so re-ingesting a URL already in the KB is skipped rather than
// creating a duplicate source.
func (s *KBStore) SourceIDByRef(ctx context.Context, agentID, sourceRef string) (string, string) {
	var id, title string
	_ = s.db.QueryRowContext(ctx,
		`SELECT id, COALESCE(title,'') FROM kb_sources
		 WHERE agent_id = `+s.ph(1)+` AND source_ref = `+s.ph(2)+`
		 ORDER BY created_at DESC LIMIT 1`,
		agentID, sourceRef).Scan(&id, &title)
	return id, title
}

// CreatePending constructs and saves a new PendingEntry with sensible
// defaults (uuid id, now + PendingDefaultTTL expiry, origin pulled from ctx),
// returning the assigned id. The caller passes the matched DupCheckResult so
// the candidate source / title / similarity are stamped on the pending row.
func (s *KBStore) CreatePending(ctx context.Context, agentID, kbType, title, content, sourceType, sourceRef string, dup DupCheckResult) (string, error) {
	origin := SourceOriginFromCtx(ctx)
	now := time.Now()
	p := &PendingEntry{
		ID:                uuid.NewString(),
		AgentID:           agentID,
		KbType:            kbType,
		Title:             title,
		Content:           content,
		SourceType:        sourceType,
		SourceRef:         sourceRef,
		SourceSessionID:   origin.SessionID,
		SourceSeqRanges:   EncodeSeqRanges(origin.Seq),
		CandidateSourceID: dup.SourceID,
		CandidateTitle:    dup.Title,
		Similarity:        dup.Score,
		CreatedAt:         now,
		ExpiresAt:         now.Add(PendingDefaultTTL),
	}
	if err := s.SavePending(ctx, p); err != nil {
		return "", err
	}
	return p.ID, nil
}

// ListPending returns all non-expired pending entries for the agent, oldest
// first. The article list renders these as cards awaiting resolution.
func (s *KBStore) ListPending(ctx context.Context, agentID string) ([]PendingEntry, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, agent_id, kb_type, title, content, source_type, source_ref, source_session_id, source_seq_ranges, candidate_source_id, candidate_title, similarity, created_at, expires_at
		 FROM kb_pending_entries
		 WHERE agent_id = `+s.ph(1)+` AND expires_at >= `+s.ph(2)+`
		 ORDER BY created_at ASC`,
		agentID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingEntry
	for rows.Next() {
		var p PendingEntry
		var created, expires string
		if err := rows.Scan(&p.ID, &p.AgentID, &p.KbType, &p.Title, &p.Content, &p.SourceType, &p.SourceRef, &p.SourceSessionID, &p.SourceSeqRanges, &p.CandidateSourceID, &p.CandidateTitle, &p.Similarity, &created, &expires); err != nil {
			return nil, err
		}
		p.CreatedAt, _ = time.Parse(time.RFC3339, created)
		p.ExpiresAt, _ = time.Parse(time.RFC3339, expires)
		out = append(out, p)
	}
	return out, nil
}
