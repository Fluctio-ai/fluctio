package kb

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CardIntervals is the Ebbinghaus-lite spacing ladder: one grade of
// "remembered" advances interval_index one step; "fuzzy" re-waits the
// same step; "forgot" resets to step 0. A card that clears the last step
// is mastered and leaves the rotation.
var CardIntervals = [6]int{1, 2, 4, 7, 15, 30}

// cardsCST is the UTC+8 zone card days are grouped by — same day boundary
// as the daily diary, so "due today" and the streak align with the day the
// user actually perceives.
var cardsCST = time.FixedZone("CST", 8*3600)

// cardDayBound returns the end of the given clock day in cardsCST (i.e.
// the instant "due today" is measured against: due_at <= bound).
func cardDayBound(t time.Time) time.Time {
	c := t.In(cardsCST)
	return time.Date(c.Year(), c.Month(), c.Day(), 23, 59, 59, 0, cardsCST)
}

const cardColumns = `id, agent_id, question, answer, source_type, source_ref, source_excerpt,
	status, interval_index, due_at, last_reviewed_at, review_count, lapse_count, created_at, updated_at`

// cardScanner is satisfied by both *sql.Row and *sql.Rows, mirroring
// bookmarkScanner.
type cardScanner interface {
	Scan(dest ...interface{}) error
}

func scanCard(row cardScanner) (KBCard, bool) {
	var c KBCard
	var dueAt, lastReviewed, createdAt, updatedAt sql.NullString
	if err := row.Scan(&c.ID, &c.AgentID, &c.Question, &c.Answer, &c.SourceType, &c.SourceRef, &c.SourceExcerpt,
		&c.Status, &c.IntervalIndex, &dueAt, &lastReviewed, &c.ReviewCount, &c.LapseCount, &createdAt, &updatedAt); err != nil {
		return KBCard{}, false
	}
	if createdAt.Valid {
		c.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
	}
	if updatedAt.Valid {
		c.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt.String)
	}
	if dueAt.Valid && dueAt.String != "" {
		if t, err := time.Parse(time.RFC3339, dueAt.String); err == nil {
			c.DueAt = &t
		}
	}
	if lastReviewed.Valid && lastReviewed.String != "" {
		if t, err := time.Parse(time.RFC3339, lastReviewed.String); err == nil {
			c.LastReviewedAt = &t
		}
	}
	return c, true
}

// SaveCard inserts one kb_cards row. sourceType: "diary" / "wiki" /
// "manual". A zero DueAt schedules the card for tomorrow (cards enter the
// rotation one day after they're created); pass an explicit DueAt to
// leave a card unscheduled (nil semantics are the caller's). When an
// embedder is wired, question+answer is embedded asynchronously so
// generation-time dedup can match against it. Returns the new card id.
func (s *KBStore) SaveCard(ctx context.Context, agentID, question, answer, sourceType, sourceRef, sourceExcerpt string) (string, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return "", fmt.Errorf("question is required")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	id := uuid.New().String()
	dueAt := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO kb_cards (id, agent_id, question, answer, source_type, source_ref, source_excerpt, status, interval_index, due_at, review_count, lapse_count, created_at, updated_at)
			VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5), s.ph(6), s.ph(7), s.ph(8), s.ph(9), s.ph(10), s.ph(11), s.ph(12), s.ph(13), s.ph(14)),
		id, agentID, question, answer, sourceType, sourceRef, sourceExcerpt, "active", 0, dueAt, 0, 0, now, now)
	if err != nil {
		return "", fmt.Errorf("insert card: %w", err)
	}
	if s.embedder != nil && s.embedder.Available() {
		go s.embedCard(context.Background(), agentID, id)
	}
	return id, nil
}

// embedCard embeds question+answer for one card and upserts it into
// kb_card_embeddings — the vector leg of generation-time dedup. Best-effort
// and silent on failure, mirroring embedBookmark.
func (s *KBStore) embedCard(ctx context.Context, agentID, cardID string) {
	if s.embedder == nil || !s.embedder.Available() {
		return
	}
	var question, answer string
	err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT question, answer FROM kb_cards WHERE id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		cardID, agentID).Scan(&question, &answer)
	if err != nil {
		return
	}
	text := strings.TrimSpace(question + "\n" + answer)
	if text == "" {
		return
	}
	vecs, err := s.embedder.Embed(ctx, []string{text})
	if err != nil || len(vecs) != 1 {
		return
	}
	_ = s.SaveCardEmbedding(ctx, agentID, cardID, vecs[0], s.embedder.Model())
}

// SaveCardEmbedding upserts the question+answer embedding for one card.
func (s *KBStore) SaveCardEmbedding(ctx context.Context, agentID, cardID string, vec []float32, model string) error {
	q := `INSERT INTO kb_card_embeddings (card_id, agent_id, embedding, dim, model, updated_at)
		VALUES (` + s.ph(1) + `,` + s.ph(2) + `,` + s.ph(3) + `,` + s.ph(4) + `,` + s.ph(5) + `,CURRENT_TIMESTAMP)
		ON CONFLICT(card_id) DO UPDATE SET agent_id=excluded.agent_id, embedding=excluded.embedding,
			dim=excluded.dim, model=excluded.model, updated_at=CURRENT_TIMESTAMP`
	_, err := s.db.ExecContext(ctx, q, cardID, agentID, kbFloat32ToBlob(vec), len(vec), model)
	return err
}

// GetCard reads one card; returns an error when absent.
func (s *KBStore) GetCard(ctx context.Context, agentID, id string) (*KBCard, error) {
	row := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s FROM kb_cards WHERE id = %s AND agent_id = %s`,
			cardColumns, s.ph(1), s.ph(2)),
		id, agentID)
	c, ok := scanCard(row)
	if !ok {
		return nil, fmt.Errorf("card not found")
	}
	return &c, nil
}

// ListCards pages cards for the cards library. filter: "due" (active,
// due_at <= end of today CST), "all" (default; every status), "active",
// "mastered", "archived", "new" (never reviewed). source filters on
// source_type ("" = any). q matches question/answer case-insensitively
// (LIKE). Ordered newest-first so the default library view matches the
// other KB views.
func (s *KBStore) ListCards(ctx context.Context, agentID, filter, source, q string, limit, offset int) ([]KBCard, error) {
	if limit <= 0 {
		limit = 50
	}
	var conds []string
	var args []interface{}
	n := 0
	add := func(sqlPart string, val interface{}) {
		n++
		conds = append(conds, fmt.Sprintf(sqlPart, s.ph(n)))
		args = append(args, val)
	}
	add(`agent_id = %s`, agentID)
	switch filter {
	case "due":
		conds = append(conds, `status = 'active'`, `due_at != ''`)
		n++
		conds = append(conds, fmt.Sprintf(`due_at <= %s`, s.ph(n)))
		args = append(args, cardDayBound(time.Now()).UTC().Format(time.RFC3339))
	case "active", "mastered", "archived":
		add(`status = %s`, filter)
	case "new":
		conds = append(conds, `status = 'active'`, `review_count = 0`)
	case "all", "":
	default:
		return nil, fmt.Errorf("unknown filter %q", filter)
	}
	if source != "" {
		add(`source_type = %s`, source)
	}
	if kw := strings.TrimSpace(q); kw != "" {
		kw = "%" + strings.ToLower(kw) + "%"
		// Two placeholders, two args — sqlite needs one arg per ? (postgres
		// would allow $n reuse, but the uniform form works on both).
		n++
		conds = append(conds, fmt.Sprintf(`(LOWER(question) LIKE %s OR LOWER(answer) LIKE %s)`, s.ph(n), s.ph(n+1)))
		n++
		args = append(args, kw, kw)
	}
	n++
	limitPh := fmt.Sprintf(`%s`, s.ph(n))
	args = append(args, limit)
	n++
	offsetPh := fmt.Sprintf(`%s`, s.ph(n))
	args = append(args, offset)
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM kb_cards WHERE %s ORDER BY created_at DESC LIMIT %s OFFSET %s`,
			cardColumns, strings.Join(conds, " AND "), limitPh, offsetPh),
		args...)
	if err != nil {
		return nil, fmt.Errorf("list cards: %w", err)
	}
	defer rows.Close()
	var out []KBCard
	for rows.Next() {
		c, ok := scanCard(rows)
		if !ok {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// CardStats computes the cards dashboard header: due-today count, status
// counts, and the consecutive-day review streak. A day counts toward the
// streak when the agent graded at least one card that CST day; the streak
// starts from today, or from yesterday when today has no review yet (the
// current day shouldn't break the streak before it's over).
func (s *KBStore) CardStats(ctx context.Context, agentID string) (KBCardStats, error) {
	var st KBCardStats
	bound := cardDayBound(time.Now()).UTC().Format(time.RFC3339)
	err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT
			COALESCE(SUM(CASE WHEN status='active' AND due_at!='' AND due_at <= %s THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status='active' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status='mastered' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status='archived' THEN 1 ELSE 0 END),0)
			FROM kb_cards WHERE agent_id = %s`, s.ph(1), s.ph(2)),
		bound, agentID).Scan(&st.DueToday, &st.Active, &st.Mastered, &st.Archived)
	if err != nil {
		return st, fmt.Errorf("card stats: %w", err)
	}
	// Streak: distinct CST review dates, newest-first, counted back from
	// today (or yesterday). reviewed_at rows carry RFC3339 UTC timestamps;
	// grouping in SQL over a text column is dialect-fragile, so the walk
	// happens here — review rows are bounded (a few per card per day).
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT reviewed_at FROM kb_card_reviews WHERE agent_id = %s ORDER BY reviewed_at DESC`, s.ph(1)),
		agentID)
	if err != nil {
		return st, nil // streak is best-effort
	}
	defer rows.Close()
	seen := map[string]bool{}
	var dates []string
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil || !raw.Valid || raw.String == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw.String)
		if err != nil {
			continue
		}
		key := t.In(cardsCST).Format("2006-01-02")
		if !seen[key] {
			seen[key] = true
			dates = append(dates, key)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	if len(dates) > 0 {
		nowCST := time.Now().In(cardsCST)
		today := nowCST.Format("2006-01-02")
		yesterday := nowCST.AddDate(0, 0, -1).Format("2006-01-02")
		// The streak starts at the newest review day, which must be today
		// (or yesterday — today shouldn't break the streak before it ends),
		// then walks consecutive calendar days.
		if dates[0] == today || dates[0] == yesterday {
			cursor, _ := time.ParseInLocation("2006-01-02", dates[0], cardsCST)
			for _, d := range dates {
				if d != cursor.Format("2006-01-02") {
					break
				}
				st.StreakDays++
				cursor = cursor.AddDate(0, 0, -1)
			}
		}
	}
	return st, nil
}

// UpdateCard overwrites question and/or answer — the editable content —
// and re-embeds so dedup matches the new text. Review state is untouched
// (editing is not reviewing).
func (s *KBStore) UpdateCard(ctx context.Context, agentID, id, question, answer string) error {
	var sets []string
	var args []interface{}
	n := 0
	if question != "" {
		n++
		sets = append(sets, fmt.Sprintf("question = %s", s.ph(n)))
		args = append(args, question)
	}
	if answer != "" {
		n++
		sets = append(sets, fmt.Sprintf("answer = %s", s.ph(n)))
		args = append(args, answer)
	}
	if len(sets) == 0 {
		return nil
	}
	n++
	sets = append(sets, fmt.Sprintf("updated_at = %s", s.ph(n)))
	args = append(args, time.Now().UTC().Format(time.RFC3339))
	n++
	args = append(args, id, agentID)
	q := fmt.Sprintf(`UPDATE kb_cards SET %s WHERE id = %s AND agent_id = %s`,
		strings.Join(sets, ", "), s.ph(n), s.ph(n+1))
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("update card: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("card not found")
	}
	if s.embedder != nil && s.embedder.Available() {
		go s.embedCard(context.Background(), agentID, id)
	}
	return nil
}

// DeleteCard removes a card plus its reviews and embedding.
func (s *KBStore) DeleteCard(ctx context.Context, agentID, id string) error {
	_, _ = s.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM kb_card_embeddings WHERE card_id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		id, agentID)
	_, _ = s.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM kb_card_reviews WHERE card_id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		id, agentID)
	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM kb_cards WHERE id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		id, agentID)
	if err != nil {
		return fmt.Errorf("delete card: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("card not found")
	}
	return nil
}

// setCardStatus stamps status (archive / restore / master). Restoring an
// archived card returns it to active with its review state intact.
func (s *KBStore) setCardStatus(ctx context.Context, agentID, id, status string) error {
	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE kb_cards SET status = %s, updated_at = %s WHERE id = %s AND agent_id = %s`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4)),
		status, time.Now().UTC().Format(time.RFC3339), id, agentID)
	if err != nil {
		return fmt.Errorf("update card status: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("card not found")
	}
	return nil
}

// ArchiveCard hides a card from the rotation and library defaults.
func (s *KBStore) ArchiveCard(ctx context.Context, agentID, id string) error {
	return s.setCardStatus(ctx, agentID, id, "archived")
}

// RestoreCard brings an archived card back as active.
func (s *KBStore) RestoreCard(ctx context.Context, agentID, id string) error {
	return s.setCardStatus(ctx, agentID, id, "active")
}

// ReviewCard applies one graded review and returns the updated card.
// Grade semantics over CardIntervals:
//
//	remembered → interval_index+1; past the last step → mastered (due_at
//	  cleared, leaves the rotation); otherwise due_at = now + next interval
//	fuzzy       → interval unchanged, due_at = now + current interval
//	forgot      → interval_index reset to 0, lapse_count+1, due_at = +1d
//
// Reviewing an archived card is rejected (unarchive first); a mastered
// card can still be reviewed — "forgot" reactivates it at step 0, other
// grades keep it mastered. Each call appends one kb_card_reviews row so
// the timeline and streak stay faithful.
func (s *KBStore) ReviewCard(ctx context.Context, agentID, id, grade string) (*KBCard, error) {
	card, err := s.GetCard(ctx, agentID, id)
	if err != nil {
		return nil, err
	}
	switch grade {
	case "remembered", "fuzzy", "forgot":
	default:
		return nil, fmt.Errorf("invalid grade %q (forgot|fuzzy|remembered)", grade)
	}
	if card.Status == "archived" {
		return nil, fmt.Errorf("card is archived")
	}
	now := time.Now().UTC()
	prevIdx := card.IntervalIndex
	newIdx := prevIdx
	newStatus := card.Status
	var newDue time.Time
	switch grade {
	case "remembered":
		newIdx = prevIdx + 1
		if newIdx >= len(CardIntervals) {
			newStatus = "mastered"
		} else {
			newDue = now.AddDate(0, 0, CardIntervals[newIdx])
		}
	case "fuzzy":
		idx := prevIdx
		if idx >= len(CardIntervals) {
			idx = len(CardIntervals) - 1
		}
		newDue = now.AddDate(0, 0, CardIntervals[idx])
	case "forgot":
		newIdx = 0
		newStatus = "active"
		newDue = now.AddDate(0, 0, CardIntervals[0])
	}
	dueStr := ""
	if !newDue.IsZero() {
		dueStr = newDue.Format(time.RFC3339)
	}
	_, err = s.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE kb_cards SET interval_index = %s, status = %s, due_at = %s,
			last_reviewed_at = %s, review_count = review_count + 1, lapse_count = lapse_count + %s, updated_at = %s
			WHERE id = %s AND agent_id = %s`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5), s.ph(6), s.ph(7), s.ph(8)),
		newIdx, newStatus, dueStr, now.Format(time.RFC3339), boolInt(grade == "forgot"), now.Format(time.RFC3339), id, agentID)
	if err != nil {
		return nil, fmt.Errorf("review card: %w", err)
	}
	_, _ = s.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO kb_card_reviews (card_id, agent_id, grade, prev_interval_index, new_interval_index, new_due_at, reviewed_at)
			VALUES (%s,%s,%s,%s,%s,%s,%s)`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5), s.ph(6), s.ph(7)),
		id, agentID, grade, prevIdx, newIdx, dueStr, now.Format(time.RFC3339))
	updated, err := s.GetCard(ctx, agentID, id)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// ListCardReviews returns one card's review timeline, newest first.
func (s *KBStore) ListCardReviews(ctx context.Context, agentID, cardID string) ([]KBCardReview, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, card_id, agent_id, grade, prev_interval_index, new_interval_index, new_due_at, reviewed_at
			FROM kb_card_reviews WHERE card_id = %s AND agent_id = %s ORDER BY reviewed_at DESC, id DESC`,
			s.ph(1), s.ph(2)),
		cardID, agentID)
	if err != nil {
		return nil, fmt.Errorf("list card reviews: %w", err)
	}
	defer rows.Close()
	var out []KBCardReview
	for rows.Next() {
		var rv KBCardReview
		var newDue, reviewedAt sql.NullString
		if err := rows.Scan(&rv.ID, &rv.CardID, &rv.AgentID, &rv.Grade, &rv.PrevIntervalIndex, &rv.NewIntervalIndex, &newDue, &reviewedAt); err != nil {
			continue
		}
		if newDue.Valid && newDue.String != "" {
			if t, err := time.Parse(time.RFC3339, newDue.String); err == nil {
				rv.NewDueAt = &t
			}
		}
		if reviewedAt.Valid {
			rv.ReviewedAt, _ = time.Parse(time.RFC3339, reviewedAt.String)
		}
		out = append(out, rv)
	}
	return out, nil
}

// CheckCardDuplicate is the generation-time dedup for new cards: an exact
// normalized-question match against a non-archived card always blocks
// (keyword leg, always on), and when an embedder is wired the question is
// embedded and matched against kb_card_embeddings with cosine > 0.90
// (vector leg). Returns nil when the question is fresh.
func (s *KBStore) CheckCardDuplicate(ctx context.Context, agentID, question string) *KBCard {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil
	}
	// Keyword leg: same normalized question among active/mastered cards.
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM kb_cards WHERE agent_id = %s AND status != 'archived' AND LOWER(TRIM(question)) = LOWER(%s) LIMIT 1`,
			cardColumns, s.ph(1), s.ph(2)),
		agentID, question)
	if err == nil {
		defer rows.Close()
		if rows.Next() {
			if c, ok := scanCard(rows); ok {
				return &c
			}
		}
		rows.Close()
	}
	// Vector leg: cosine over stored card embeddings.
	if s.embedder == nil || !s.embedder.Available() {
		return nil
	}
	vecs, err := s.embedder.Embed(ctx, []string{question})
	if err != nil || len(vecs) != 1 {
		return nil
	}
	q := vecs[0]
	erows, err := s.db.QueryContext(ctx,
		`SELECT e.embedding FROM kb_card_embeddings e
		 JOIN kb_cards c ON c.id = e.card_id
		 WHERE e.agent_id = `+s.ph(1)+` AND c.status != 'archived'`, agentID)
	if err != nil {
		return nil
	}
	defer erows.Close()
	for erows.Next() {
		var blob []byte
		if err := erows.Scan(&blob); err != nil {
			return nil
		}
		vec := kbFloat32FromBlob(blob)
		if len(vec) != len(q) {
			continue
		}
		if kbCosine(q, vec) > 0.90 {
			slog.Debug("kb card dup: vector leg hit", "agent", agentID)
			return &KBCard{Question: question}
		}
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
