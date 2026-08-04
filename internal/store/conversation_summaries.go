package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ConversationSummary is one extracted summary of a conversation range.
// A row is created when the agent finishes compacting a session or
// when a new session_key is created (the previous session gets a final
// summary). Rows back the cross-session memory_search tool.
type ConversationSummary struct {
	ID             int64
	UserID         string
	AgentID        string
	SessionKey     string
	ChatterUserID  string
	Summary        string
	Keywords       []string
	Topic          string   // short topic label (empty on legacy rows)
	Segments       [][2]int // seq ranges this topic actually covers; empty → fall back to SeqStart/SeqEnd
	SeqStart       int
	SeqEnd         int
	EmbeddingModel string // empty if no embedding generated
	Importance     int    // 1-5 LLM-assigned value; 0 = legacy/unset
	AccessCount    int    // times surfaced by memory_search (reinforcement)
	AccessTimeSum  int64  // sum of recall unix ts; /AccessCount = mean recall time (scheme mean_time)
	LastAccessedAt time.Time
	CreatedAt      time.Time
}

// InsertConversationSummary writes the main row, upserting on the unique
// (agent_id, session_key, seq_start, seq_end) key so re-summarizing the
// same range (e.g. /compact twice) merges instead of duplicating. The
// FTS5 trigger auto-populates conversation_summaries_fts.
// Does NOT write the vec0 row — call InsertConversationSummaryVector
// separately when an embedding is available.
func (d *DBStore) InsertConversationSummary(
	ctx context.Context,
	s ConversationSummary,
) (int64, error) {
	keywordsJSON, err := json.Marshal(s.Keywords)
	if err != nil {
		return 0, fmt.Errorf("marshal keywords: %w", err)
	}
	segmentsJSON, err := marshalSegments(s.Segments)
	if err != nil {
		return 0, fmt.Errorf("marshal segments: %w", err)
	}

	var id int64
	switch d.dialect {
	case "postgres":
		// On conflict over the unique composite key, replace the mutable
		// content (summary/keywords/embedding_model/importance).
		// embedding_model resets to the caller's value so a re-summarize
		// without an embedder clears the stamp and the next vectorize
		// re-stamps it. access_count/last_accessed_at are NOT overwritten
		// (reinforcement state survives re-summarize).
		err = d.db.QueryRowContext(ctx, `
			INSERT INTO conversation_summaries
				(agent_id, session_key,
				 summary, keywords, seq_start, seq_end, embedding_model, importance, topic, segments)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (agent_id, session_key, seq_start, seq_end)
			DO UPDATE SET summary = EXCLUDED.summary,
			              keywords = EXCLUDED.keywords,
			              embedding_model = EXCLUDED.embedding_model,
			              importance = EXCLUDED.importance,
			              topic = EXCLUDED.topic,
			              segments = EXCLUDED.segments
			RETURNING id`,
			s.AgentID, s.SessionKey,
			s.Summary, string(keywordsJSON), s.SeqStart, s.SeqEnd,
			nilIfEmpty(s.EmbeddingModel), s.Importance, s.Topic, string(segmentsJSON),
		).Scan(&id)
	default:
		// SQLite upsert. RETURNING id (modernc supports it) gives the
		// rowid of either the inserted or the updated row — needed so
		// the caller can stamp the vector on the right summary.
		err = d.db.QueryRowContext(ctx, `
			INSERT INTO conversation_summaries
				(agent_id, session_key,
				 summary, keywords, seq_start, seq_end, embedding_model, importance, topic, segments)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(agent_id, session_key, seq_start, seq_end)
			DO UPDATE SET summary = excluded.summary,
			              keywords = excluded.keywords,
			              embedding_model = excluded.embedding_model,
			              importance = excluded.importance,
			              topic = excluded.topic,
			              segments = excluded.segments
			RETURNING id`,
			s.AgentID, s.SessionKey,
			s.Summary, string(keywordsJSON), s.SeqStart, s.SeqEnd,
			nilIfEmpty(s.EmbeddingModel), s.Importance, s.Topic, string(segmentsJSON),
		).Scan(&id)
	}
	return id, err
}

// marshalSegments serializes a slice of [start,end] seq pairs as JSON.
// Nil/empty → "[]" (the column default), which readers interpret as
// "fall back to seq_start/seq_end".
func marshalSegments(segs [][2]int) ([]byte, error) {
	if len(segs) == 0 {
		return []byte("[]"), nil
	}
	return json.Marshal(segs)
}

// unmarshalSegments is the scan-side inverse. Empty/invalid JSON → nil
// (caller falls back to SeqStart/SeqEnd).
func unmarshalSegments(raw string) [][2]int {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" || raw == "null" {
		return nil
	}
	var out [][2]int
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SearchConversationSummariesFTS returns ranked hits scoped to agent_id.
// Fluctio is single-user: an agent's memory is shared across all of its
// chatters (owner web / IM / cron), so recall keys on
// (agent_id, session_key, seq_start, seq_end) with no per-chatter
// dimension.
//
// Pipeline: SQL LIKE pre-filter → bigram token overlap scoring
// (keywords×3, summary×2) × recency decay → top-K.
//
// We fetch fetchMultiplier×limit candidates from SQL, score them in Go,
// and return the top `limit`. The multiplier trades recall for CPU.
// unicode61 FTS5 can't match CJK substrings, so LIKE handles both
// dialects; vector recall (MVP-2) will replace this entirely.
func (d *DBStore) SearchConversationSummariesFTS(
	ctx context.Context,
	agentID, query string,
	limit int,
) ([]ConversationSummary, error) {
	if limit <= 0 {
		limit = 10
	}

	// Fetch more candidates than the final limit so the scorer has a
	// pool to re-rank. 3× gives reasonable recall without over-fetching.
	const fetchMultiplier = 3
	fetchLimit := limit * fetchMultiplier
	if fetchLimit < 10 {
		fetchLimit = 10
	}

	// Tokenize the query into space-separated terms for LIKE pre-filter.
	terms := strings.Fields(query)
	if len(terms) == 0 {
		terms = []string{query}
	}

	clauses := make([]string, 0, len(terms))
	args := make([]any, 0, len(terms)+4)
	args = append(args, agentID)
	placeholder := 2 // 1-based for pg
	if d.dialect == "postgres" {
		for _, t := range terms {
			clauses = append(clauses,
				fmt.Sprintf("(summary ILIKE '%%' || $%d || '%%' OR keywords::text ILIKE '%%' || $%d || '%%')", placeholder, placeholder))
			args = append(args, t)
			placeholder++
		}
		args = append(args, fetchLimit)
		rows, err := d.db.QueryContext(ctx, `
			SELECT id, agent_id, session_key,
			       summary, keywords, seq_start, seq_end, embedding_model, importance, access_count, access_time_sum, last_accessed_at, created_at, topic, segments
			FROM conversation_summaries
			WHERE agent_id = $1
			  AND (`+strings.Join(clauses, " OR ")+`)
			ORDER BY created_at DESC
			LIMIT $`+fmt.Sprint(placeholder), args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		candidates, err := scanConversationSummaries(rows)
		if err != nil {
			return nil, err
		}
		return reRankSummaries(candidates, query, limit), nil
	}

	// SQLite — ? placeholders
	for _, t := range terms {
		_ = placeholder
		clauses = append(clauses,
			"(summary LIKE ? OR keywords LIKE ?)")
		args = append(args, "%"+t+"%", "%"+t+"%")
	}
	args = append(args, fetchLimit)
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, agent_id, session_key,
		       summary, keywords, seq_start, seq_end, embedding_model, importance, access_count, access_time_sum, last_accessed_at, created_at, topic, segments
		FROM conversation_summaries
		WHERE agent_id = ?
		  AND (`+strings.Join(clauses, " OR ")+`)
		ORDER BY created_at DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates, err := scanConversationSummaries(rows)
	if err != nil {
		return nil, err
	}
	return reRankSummaries(candidates, query, limit), nil
}

func scanConversationSummaries(rows *sql.Rows) ([]ConversationSummary, error) {
	var out []ConversationSummary
	for rows.Next() {
		var s ConversationSummary
		var keywordsJSON string
		var embModel sql.NullString
		var lastAccessed sql.NullTime
		var topic string
		var segmentsJSON string
		err := rows.Scan(
			&s.ID, &s.AgentID, &s.SessionKey,
			&s.Summary, &keywordsJSON, &s.SeqStart, &s.SeqEnd, &embModel,
			&s.Importance, &s.AccessCount, &s.AccessTimeSum, &lastAccessed, &s.CreatedAt,
			&topic, &segmentsJSON,
		)
		if err != nil {
			return nil, err
		}
		s.EmbeddingModel = embModel.String
		if lastAccessed.Valid {
			s.LastAccessedAt = lastAccessed.Time
		}
		_ = json.Unmarshal([]byte(keywordsJSON), &s.Keywords)
		if s.Keywords == nil {
			s.Keywords = []string{}
		}
		s.Topic = topic
		s.Segments = unmarshalSegments(segmentsJSON)
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListConversationSummariesByDateRange returns all summary rows for an
// agent whose created_at falls in [from, to), ordered oldest first.
// Used by the daily-diary generator to gather one day's topics. from/to
// are UTC instants; callers convert the UTC+8 day boundaries to UTC.
func (d *DBStore) ListConversationSummariesByDateRange(ctx context.Context, agentID string, from, to time.Time) ([]ConversationSummary, error) {
	const cols = `id, agent_id, session_key,
			       summary, keywords, seq_start, seq_end, embedding_model, importance, access_count, access_time_sum, last_accessed_at, created_at, topic, segments`
	var rows *sql.Rows
	var err error
	if d.dialect == "postgres" {
		rows, err = d.db.QueryContext(ctx,
			`SELECT `+cols+` FROM conversation_summaries WHERE agent_id = $1 AND created_at >= $2 AND created_at < $3 ORDER BY created_at ASC`,
			agentID, from, to)
	} else {
		rows, err = d.db.QueryContext(ctx,
			`SELECT `+cols+` FROM conversation_summaries WHERE agent_id = ? AND created_at >= ? AND created_at < ? ORDER BY created_at ASC`,
			agentID, from, to)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversationSummaries(rows)
}

// SetConversationSummaryEmbeddingModel stamps the model that produced a
// summary's vector. Called after a successful vec write so the periodic
// backfill skips the row on the next pass and a future model switch can
// detect drift.
func (d *DBStore) SetConversationSummaryEmbeddingModel(ctx context.Context, id int64, model string) error {
	switch d.dialect {
	case "postgres":
		_, err := d.db.ExecContext(ctx,
			`UPDATE conversation_summaries SET embedding_model = $1 WHERE id = $2`, model, id)
		return err
	default:
		_, err := d.db.ExecContext(ctx,
			`UPDATE conversation_summaries SET embedding_model = ? WHERE id = ?`, model, id)
		return err
	}
}

// IncrementConversationSummaryAccess is the reinforcement signal: bumps
// access_count and refreshes last_accessed_at for every recalled summary
// id. Called by memory_search after deciding what to surface, so
// frequently-recalled summaries score higher (and reset recency decay)
// on future queries. Empty/nil ids is a no-op.
func (d *DBStore) IncrementConversationSummaryAccess(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	nowUnix := time.Now().Unix()
	n := len(ids)
	placeholders := make([]string, n)
	args := make([]any, 0, n+1)
	// `now` appears in the SQL before the WHERE id IN (...) list, so it
	// must lead the args slice: sqlite ? binds by position, postgres $N
	// by number — leading with now keeps both correct (nowPh=$1 / ids=$2..).
	args = append(args, nowUnix)
	var nowPh string
	if d.dialect == "postgres" {
		nowPh = "$1"
	} else {
		nowPh = "?"
	}
	for i, id := range ids {
		if d.dialect == "postgres" {
			placeholders[i] = fmt.Sprintf("$%d", i+2)
		} else {
			placeholders[i] = "?"
		}
		args = append(args, id)
	}
	q := fmt.Sprintf(`UPDATE conversation_summaries
		SET access_count = access_count + 1,
		    access_time_sum = access_time_sum + %s,
		    last_accessed_at = CURRENT_TIMESTAMP
		WHERE id IN (%s)`, nowPh, strings.Join(placeholders, ","))
	_, err := d.db.ExecContext(ctx, q, args...)
	return err
}

// SetConversationSummaryMeta upserts a metadata key. Used for the
// "embedding_model_in_use" key that drives model-switch detection
// (when the configured embedding model differs from the in-use one,
// MVP-2's rebuild task re-embeds all summaries).
func (d *DBStore) SetConversationSummaryMeta(ctx context.Context, key, value string) error {
	switch d.dialect {
	case "postgres":
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO conversation_summaries_meta (key, value) VALUES ($1, $2)
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, value)
		return err
	default:
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO conversation_summaries_meta (key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
		return err
	}
}

// GetConversationSummaryMeta reads a metadata key. Returns "" (no error)
// if the key doesn't exist yet.
func (d *DBStore) GetConversationSummaryMeta(ctx context.Context, key string) (string, error) {
	var v string
	var err error
	switch d.dialect {
	case "postgres":
		err = d.db.QueryRowContext(ctx,
			`SELECT value FROM conversation_summaries_meta WHERE key = $1`, key).Scan(&v)
	default:
		err = d.db.QueryRowContext(ctx,
			`SELECT value FROM conversation_summaries_meta WHERE key = ?`, key).Scan(&v)
	}
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// ── Bigram tokenization + weighted scoring ──────────────────────────
// Borrowed from kb/scorer.go — converts query and summary text into
// English-word + CJK-bigram token sets, then scores by overlap with
// field weights: keywords×3, summary×2. Recency decay is applied on
// top so newer summaries outrank older ones at similar overlap.

var (
	cSummaryWordRE = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_-]+`)
	cSummaryCJKRE  = regexp.MustCompile(`[\p{Han}\x{3040}-\x{30ff}\x{ac00}-\x{d7af}]+`)
)

var cSummaryStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "to": true,
	"in": true, "on": true, "for": true, "and": true, "or": true,
	"is": true, "are": true, "was": true, "were": true,
	"with": true, "by": true, "from": true, "this": true, "that": true,
	"what": true, "why": true, "how": true, "when": true, "which": true, "who": true,
	"的": true, "了": true, "是": true, "和": true, "或": true,
	"在": true, "对": true, "为": true, "与": true, "及": true,
}

func tokenizeSummary(text string) []string {
	text = strings.ToLower(text)
	seen := make(map[string]bool)
	var tokens []string

	for _, m := range cSummaryWordRE.FindAllString(text, -1) {
		t := strings.ToLower(m)
		if len(t) < 2 || cSummaryStopwords[t] {
			continue
		}
		if !seen[t] {
			seen[t] = true
			tokens = append(tokens, t)
		}
	}

	for _, m := range cSummaryCJKRE.FindAllString(text, -1) {
		runes := []rune(m)
		if len(runes) == 1 {
			if cSummaryStopwords[string(runes)] {
				continue
			}
			t := string(runes)
			if !seen[t] {
				seen[t] = true
				tokens = append(tokens, t)
			}
			continue
		}
		for i := 0; i < len(runes)-1; i++ {
			bg := string(runes[i]) + string(runes[i+1])
			if cSummaryStopwords[bg] {
				continue
			}
			if !seen[bg] {
				seen[bg] = true
				tokens = append(tokens, bg)
			}
		}
	}
	return tokens
}

func tokenizeSummarySet(text string) map[string]bool {
	tokens := tokenizeSummary(text)
	s := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		s[t] = true
	}
	return s
}

func intersectCountSummary(a, b map[string]bool) int {
	if len(a) > len(b) {
		a, b = b, a
	}
	n := 0
	for t := range a {
		if b[t] {
			n++
		}
	}
	return n
}

func recencyWeightSummary(reference time.Time) float64 {
	if reference.IsZero() {
		return 0.5
	}
	days := time.Since(reference).Hours() / 24
	if days <= 0 {
		return 1.0
	}
	w := 1.0 / (1.0 + days/7.0)
	if w < 0.1 {
		return 0.1
	}
	return w
}

// Three-factor recall weights. importance carries the LLM-assigned value;
// access rewards frequently-recalled summaries (reinforcement, log1p-
// saturated). recency is the in-batch min-max of mean recall time (or
// creation time if never recalled) — newest=1, oldest=0 — so a single
// fresh recall can't refresh an otherwise-stale summary, and a uniformly
// old pool still preserves relative ordering.
const (
	importanceWeight = 1.0 // importance is 1-5, comparable to a keyword hit (×3)
	accessWeight     = 0.2 // each recall adds ~20% to the recency multiplier
)

func reRankSummaries(summaries []ConversationSummary, query string, topK int) []ConversationSummary {
	if topK <= 0 {
		topK = 10
	}

	qTokens := tokenizeSummarySet(query)
	if len(qTokens) == 0 {
		if len(summaries) > topK {
			return summaries[:topK]
		}
		return summaries
	}

	type scored struct {
		idx   int
		score float64
	}
	var ranked []scored

	// Batch-normalize recency over mean recall time (creation time for
	// never-recalled summaries). In-batch min-max keeps relative ordering
	// sharp even when the whole pool is old — absolute decay would crush
	// everything against its 0.1 floor and erase the signal (scheme
	// mean_time). The mean (not last access) means a single fresh recall
	// can't refresh an otherwise-stale summary into top rank.
	if len(summaries) == 0 {
		return nil
	}
	refTimes := make([]time.Time, len(summaries))
	for i, s := range summaries {
		if s.AccessCount > 0 && s.AccessTimeSum > 0 {
			refTimes[i] = time.Unix(s.AccessTimeSum/int64(s.AccessCount), 0)
		} else {
			refTimes[i] = s.CreatedAt
		}
	}
	minT, maxT := refTimes[0], refTimes[0]
	for _, t := range refTimes[1:] {
		if t.Before(minT) {
			minT = t
		}
		if t.After(maxT) {
			maxT = t
		}
	}

	for i, s := range summaries {
		summaryToks := tokenizeSummarySet(s.Summary)
		kwText := strings.Join(s.Keywords, " ")
		kwToks := tokenizeSummarySet(kwText)

		overlap := 3.0*float64(intersectCountSummary(qTokens, kwToks)) +
			2.0*float64(intersectCountSummary(qTokens, summaryToks))

		// Legacy rows have importance 0 (column added after the fact) —
		// treat them as a neutral 3 so they aren't silently buried.
		imp := s.Importance
		if imp == 0 {
			imp = 3
		}

		// recency: in-batch min-max of mean recall time (newest=1, oldest=0).
		recency := 1.0
		if maxT.After(minT) {
			recency = float64(refTimes[i].Sub(minT)) / float64(maxT.Sub(minT))
		}

		// baseScore + token overlap + importance, scaled by recency and a
		// reinforcement multiplier from access_count. Saturation (log1p)
		// caps the gain so a frequently-recalled summary can't dominate
		// unboundedly — anti-enrichment (scheme A).
		base := 0.5 + overlap + float64(imp)*importanceWeight
		reinforcement := 1.0 + math.Log1p(float64(s.AccessCount))*accessWeight
		finalScore := base * recency * reinforcement

		ranked = append(ranked, scored{idx: i, score: finalScore})
	}

	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})

	out := make([]ConversationSummary, 0, topK)
	for i := 0; i < len(ranked) && i < topK; i++ {
		out = append(out, summaries[ranked[i].idx])
	}
	return out
}

// ── Vector CRUD (vec0 / pgvector) ───────────────────────────────────

func float32ToBlob(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

// float32FromBlob is the inverse of float32ToBlob: it decodes a
// little-endian float32 byte slice back into a vector. Returns nil for
// empty or mis-sized input.
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

// InsertConversationSummaryVector writes an embedding row.
// SQLite: vec0 ignores INSERT OR REPLACE, so delete-then-insert makes
// re-vectorizing an existing summary_id idempotent (needed after a
// summary upsert or a force rebuild). Postgres: UPDATE the vector
// column directly.
func (d *DBStore) InsertConversationSummaryVector(ctx context.Context, summaryID int64, embedding []float32) error {
	if len(embedding) == 0 {
		return fmt.Errorf("empty embedding")
	}
	switch d.dialect {
	case "postgres":
		_, err := d.db.ExecContext(ctx,
			`UPDATE conversation_summaries SET embedding = $1::vector WHERE id = $2`,
			float32ToPGVector(embedding), summaryID)
		return err
	default:
		if _, err := d.db.ExecContext(ctx,
			`DELETE FROM conversation_summaries_vec WHERE summary_id = ?`, summaryID); err != nil {
			return err
		}
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO conversation_summaries_vec(summary_id, embedding) VALUES (?, ?)`,
			summaryID, float32ToBlob(embedding))
		return err
	}
}

func float32ToPGVector(vec []float32) string {
	parts := make([]string, len(vec))
	for i, v := range vec {
		parts[i] = fmt.Sprintf("%.8g", v)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// parsePGVectorText decodes the "[a,b,c]" text form pgvector returns
// when a vector column is scanned as a string. Inverse of float32ToPGVector.
func parsePGVectorText(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	vec := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, err
		}
		vec[i] = float32(f)
	}
	return vec, nil
}

// ConversationSummaryVectorShape reports the dimension + embedding model
// of one agent's EXISTING vectors, so the UI can warn when the configured
// dim/model diverges (vectors would fail to write, or go stale). Returns
// dim=0 when the agent has no vectors yet.
func (d *DBStore) ConversationSummaryVectorShape(ctx context.Context, agentID string) (dim int, model string, err error) {
	switch d.dialect {
	case "postgres":
		// pgvector's vector_dims() returns the stored dimension directly.
		err = d.db.QueryRowContext(ctx,
			`SELECT vector_dims(embedding) FROM conversation_summaries
			 WHERE agent_id = $1 AND embedding IS NOT NULL ORDER BY id DESC LIMIT 1`, agentID).Scan(&dim)
		if err != nil && errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
	default:
		var blob []byte
		err = d.db.QueryRowContext(ctx,
			`SELECT v.embedding FROM conversation_summaries_vec v
			 JOIN conversation_summaries s ON s.id = v.summary_id
			 WHERE s.agent_id = ? ORDER BY s.id DESC LIMIT 1`, agentID).Scan(&blob)
		if err == nil && len(blob) > 0 {
			dim = len(blob) / 4
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, "", err
	}
	err = nil
	_ = d.db.QueryRowContext(ctx,
		`SELECT embedding_model FROM conversation_summaries
		 WHERE agent_id = ? AND embedding_model IS NOT NULL AND embedding_model != ''
		 ORDER BY id DESC LIMIT 1`, agentID).Scan(&model)
	return dim, model, nil
}

// SearchConversationSummariesVector runs KNN over vec0 and returns the
// matching summary IDs with distances. Does NOT join to the main table —
// callers should batch-fetch summaries by ID.
// VecSummaryHit is a KNN result with its raw distance (smaller = more
// similar). Distance semantics depend on dialect: postgres cosine distance
// (0..2), sqlite-vec L2.
type VecSummaryHit struct {
	ID       int64
	Distance float64
}

// SearchConversationSummariesVectorScored is like
// SearchConversationSummariesVector but also returns the KNN distance so the
// caller can apply a relevance threshold. Used by memory_search.
func (d *DBStore) SearchConversationSummariesVectorScored(ctx context.Context, embedding []float32, limit int) ([]VecSummaryHit, error) {
	if limit <= 0 {
		limit = 10
	}
	if len(embedding) == 0 {
		return nil, nil
	}
	var rows *sql.Rows
	var err error
	switch d.dialect {
	case "postgres":
		rows, err = d.db.QueryContext(ctx,
			`SELECT id, (embedding <=> $1::vector) AS distance
			 FROM conversation_summaries
			 ORDER BY distance
			 LIMIT $2`,
			float32ToPGVector(embedding), limit)
	default:
		rows, err = d.db.QueryContext(ctx,
			`SELECT summary_id, distance
			 FROM conversation_summaries_vec
			 WHERE embedding MATCH ?
			 ORDER BY distance
			 LIMIT ?`,
			float32ToBlob(embedding), limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VecSummaryHit
	for rows.Next() {
		var h VecSummaryHit
		if err := rows.Scan(&h.ID, &h.Distance); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (d *DBStore) SearchConversationSummariesVector(ctx context.Context, embedding []float32, limit int) ([]int64, error) {
	if limit <= 0 {
		limit = 10
	}
	if len(embedding) == 0 {
		return nil, nil
	}

	var rows *sql.Rows
	var err error

	switch d.dialect {
	case "postgres":
		rows, err = d.db.QueryContext(ctx,
			`SELECT id FROM conversation_summaries
			 ORDER BY embedding <=> $1::vector
			 LIMIT $2`,
			float32ToPGVector(embedding), limit)
	default:
		rows, err = d.db.QueryContext(ctx,
			`SELECT summary_id FROM conversation_summaries_vec
			 WHERE embedding MATCH ?
			 ORDER BY distance
			 LIMIT ?`,
			float32ToBlob(embedding), limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetConversationSummaryEmbeddings batches fetch of the stored embedding
// vectors for the given summary IDs. IDs without a stored vector are
// omitted from the result map. Used by MMR to compute inter-candidate
// similarity without re-embedding.
func (d *DBStore) GetConversationSummaryEmbeddings(ctx context.Context, ids []int64) (map[int64][]float32, error) {
	out := make(map[int64][]float32, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if d.dialect == "postgres" {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		} else {
			placeholders[i] = "?"
		}
		args = append(args, id)
	}

	var rows *sql.Rows
	var err error
	switch d.dialect {
	case "postgres":
		rows, err = d.db.QueryContext(ctx,
			fmt.Sprintf(`SELECT id, embedding::text FROM conversation_summaries
				WHERE id IN (%s) AND embedding IS NOT NULL`,
				strings.Join(placeholders, ",")),
			args...)
	default:
		rows, err = d.db.QueryContext(ctx,
			fmt.Sprintf(`SELECT summary_id, embedding FROM conversation_summaries_vec
				WHERE summary_id IN (%s)`, strings.Join(placeholders, ",")),
			args...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		if d.dialect == "postgres" {
			var text sql.NullString
			if err := rows.Scan(&id, &text); err != nil {
				return nil, err
			}
			if !text.Valid || text.String == "" {
				continue
			}
			vec, perr := parsePGVectorText(text.String)
			if perr != nil {
				return nil, perr
			}
			out[id] = vec
		} else {
			var buf []byte
			if err := rows.Scan(&id, &buf); err != nil {
				return nil, err
			}
			if vec := float32FromBlob(buf); vec != nil {
				out[id] = vec
			}
		}
	}
	return out, rows.Err()
}

// ClearConversationSummaryVectors deletes every row from the vector
// table. Called before a rebuild.
// ClearConversationSummaryVectorsForAgent deletes the vec0 rows whose
// summary belongs to agentID — used by force re-vectorize so existing
// vectors are replaced (vec0 doesn't honor INSERT OR REPLACE, so a
// full rebuild deletes first). Clears the agent's embedding_model stamp
// too so the summaries re-queue for the periodic task if the rebuild
// is interrupted.
func (d *DBStore) ClearConversationSummaryVectorsForAgent(ctx context.Context, agentID string) error {
	switch d.dialect {
	case "postgres":
		_, err := d.db.ExecContext(ctx,
			`UPDATE conversation_summaries SET embedding = NULL, embedding_model = NULL
			 WHERE agent_id = $1`, agentID)
		return err
	default:
		if _, err := d.db.ExecContext(ctx,
			`DELETE FROM conversation_summaries_vec WHERE summary_id IN
			 (SELECT id FROM conversation_summaries WHERE agent_id = ?)`, agentID); err != nil {
			return err
		}
		_, err := d.db.ExecContext(ctx,
			`UPDATE conversation_summaries SET embedding_model = NULL WHERE agent_id = ?`, agentID)
		return err
	}
}

// ListConversationSummariesByAgent returns all summaries for one agent
// (across sessions/chatters), ascending by creation. Used by force
// re-vectorize to rebuild every vector regardless of current state.
func (d *DBStore) ListConversationSummariesByAgent(ctx context.Context, agentID string, limit int) ([]ConversationSummary, error) {
	if limit <= 0 {
		limit = 1000
	}
	var rows *sql.Rows
	var err error
	switch d.dialect {
	case "postgres":
		rows, err = d.db.QueryContext(ctx,
			`SELECT id, agent_id, session_key,
			        summary, keywords, seq_start, seq_end, embedding_model, importance, access_count, access_time_sum, last_accessed_at, created_at, topic, segments
			 FROM conversation_summaries
			 WHERE agent_id = $1
			 ORDER BY created_at
			 LIMIT $2`, agentID, limit)
	default:
		rows, err = d.db.QueryContext(ctx,
			`SELECT id, agent_id, session_key,
			        summary, keywords, seq_start, seq_end, embedding_model, importance, access_count, access_time_sum, last_accessed_at, created_at, topic, segments
			 FROM conversation_summaries
			 WHERE agent_id = ?
			 ORDER BY created_at
			 LIMIT ?`, agentID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversationSummaries(rows)
}

// ListConversationSummariesBySession returns every topic row for one
// session (across the session's lifetime), ordered by creation. Used by
// the incremental summary path to feed the LLM the existing topic list
// for merge. Empty slice for a session that has never been summarized.
func (d *DBStore) ListConversationSummariesBySession(ctx context.Context, agentID, sessionKey string) ([]ConversationSummary, error) {
	var rows *sql.Rows
	var err error
	switch d.dialect {
	case "postgres":
		rows, err = d.db.QueryContext(ctx,
			`SELECT id, agent_id, session_key,
			        summary, keywords, seq_start, seq_end, embedding_model, importance, access_count, access_time_sum, last_accessed_at, created_at, topic, segments
			 FROM conversation_summaries
			 WHERE agent_id = $1 AND session_key = $2
			 ORDER BY created_at`, agentID, sessionKey)
	default:
		rows, err = d.db.QueryContext(ctx,
			`SELECT id, agent_id, session_key,
			        summary, keywords, seq_start, seq_end, embedding_model, importance, access_count, access_time_sum, last_accessed_at, created_at, topic, segments
			 FROM conversation_summaries
			 WHERE agent_id = ? AND session_key = ?
			 ORDER BY created_at`, agentID, sessionKey)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversationSummaries(rows)
}

// DeleteConversationSummariesBySession removes every topic row (main +
// vector) for one session. Called by the incremental summary path before
// writing the merged topic list — the LLM returns the full updated set,
// so old rows are dropped and the new set inserted in their place.
func (d *DBStore) DeleteConversationSummariesBySession(ctx context.Context, agentID, sessionKey string) error {
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`DELETE FROM conversation_summaries WHERE agent_id = $1 AND session_key = $2`,
			agentID, sessionKey)
		return err
	}
	// SQLite: vec0 lives in a separate virtual table keyed by summary_id.
	// Delete its orphans first, then the main rows, in one tx so a
	// partial delete can't leave the session half-summarized.
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM conversation_summaries_vec WHERE summary_id IN (
			SELECT id FROM conversation_summaries WHERE agent_id = ? AND session_key = ?)`,
		agentID, sessionKey); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM conversation_summaries WHERE agent_id = ? AND session_key = ?`,
		agentID, sessionKey); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearConversationSummaryVectors deletes every row from the vector
// table. Called before a rebuild.
func (d *DBStore) ClearConversationSummaryVectors(ctx context.Context) error {
	switch d.dialect {
	case "postgres":
		_, err := d.db.ExecContext(ctx, `UPDATE conversation_summaries SET embedding = NULL`)
		return err
	default:
		_, err := d.db.ExecContext(ctx, `DELETE FROM conversation_summaries_vec`)
		return err
	}
}

// ListConversationSummariesNeedingVector returns summaries that have no
// embedding yet. When model is non-empty, also returns summaries that
// were embedded with a different model.
func (d *DBStore) ListConversationSummariesNeedingVector(ctx context.Context, model string, limit int) ([]ConversationSummary, error) {
	if limit <= 0 {
		limit = 100
	}

	var rows *sql.Rows
	var err error

	switch d.dialect {
	case "postgres":
		rows, err = d.db.QueryContext(ctx,
			`SELECT id, agent_id, session_key,
			        summary, keywords, seq_start, seq_end, embedding_model, importance, access_count, access_time_sum, last_accessed_at, created_at, topic, segments
			 FROM conversation_summaries
			 WHERE embedding IS NULL OR ($1 != '' AND (embedding_model IS NULL OR embedding_model != $1))
			 ORDER BY created_at
			 LIMIT $2`, model, limit)
	default:
		rows, err = d.db.QueryContext(ctx,
			`SELECT s.id, s.agent_id, s.session_key,
			        s.summary, s.keywords, s.seq_start, s.seq_end, s.embedding_model, s.importance, s.access_count, s.access_time_sum, s.last_accessed_at, s.created_at, s.topic, s.segments
			 FROM conversation_summaries s
			 LEFT JOIN conversation_summaries_vec v ON v.summary_id = s.id
			 WHERE v.summary_id IS NULL OR (? != '' AND (s.embedding_model IS NULL OR s.embedding_model != ?))
			 ORDER BY s.created_at
			 LIMIT ?`, model, model, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanConversationSummaries(rows)
}

// GetConversationSummariesByIDs fetches summaries by primary key.
// Used by memory_search after vector KNN returns matching IDs.
func (d *DBStore) GetConversationSummariesByIDs(ctx context.Context, ids []int64) ([]ConversationSummary, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	// Build IN clause dynamically.
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}

	q := fmt.Sprintf(`SELECT id, agent_id, session_key,
	       summary, keywords, seq_start, seq_end, embedding_model, importance, access_count, access_time_sum, last_accessed_at, created_at, topic, segments
	FROM conversation_summaries
	WHERE id IN (%s)
	ORDER BY created_at DESC`, strings.Join(placeholders, ","))

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanConversationSummaries(rows)
}
