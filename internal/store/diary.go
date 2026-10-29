package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// DiarySegRef is a clickable #seq-N reference: the session plus the seq
// range it spans. The frontend renders it as a link into that session's
// view, scrolled to the range.
type DiarySegRef struct {
	Session string `json:"session"`
	Start   int    `json:"start"`
	End     int    `json:"end"`
}

// DiaryTheme is one topic group in a daily diary, distilled by the LLM
// from the day's conversation messages (session_messages), grouped by theme.
type DiaryTheme struct {
	Title    string        `json:"title"`
	Summary  string        `json:"summary"`
	Points   []string      `json:"points"`
	Topics   []string      `json:"topics,omitempty"`
	Session  string        `json:"session,omitempty"`
	Segments []DiarySegRef `json:"segments,omitempty"`
}

// DiaryBlindspot is one "you might have missed" point the LLM flags
// from the day's conversation — important but not followed up.
type DiaryBlindspot struct {
	Point  string `json:"point"`
	Reason string `json:"reason"`
}

// DiaryShelved is one topic the user explicitly deferred ("回头再说") and
// hasn't returned to — a rolling ledger carried across diaries: each day's
// Pass 3 inherits the previous ledger, settles entries the day picked back
// up (resolved, shown that day then retired), and drops open entries older
// than the shelved window. It flags, never acts — the entry's Session/Seq
// deep link is the user's way back into the deferred context.
type DiaryShelved struct {
	Topic   string `json:"topic"`
	Context string `json:"context"` // one line: the situation when deferred
	Since   string `json:"since"`   // YYYY-MM-DD first shelved
	Session string `json:"session"` // deep-link target
	Seq     int    `json:"seq"`
	Status  string `json:"status"` // "open" | "resolved"
}

// DailyDiary is one day's generated diary for one agent.
type DailyDiary struct {
	AgentID     string           `json:"agentId"`
	Date        string           `json:"date"` // YYYY-MM-DD, UTC+8
	Overview    string           `json:"overview"`
	Themes      []DiaryTheme     `json:"themes"`
	Blindspots  []DiaryBlindspot `json:"blindspots"`
	Shelved     []DiaryShelved   `json:"shelved"`
	Archives    []string         `json:"archives"`
	Model       string           `json:"model"`
	GeneratedAt time.Time        `json:"generatedAt"`
}

const dailyDiaryColumns = `agent_id, date, overview, themes, blindspots, shelved, archives, model, generated_at`

// scanDailyDiary scans one row from either a *sql.Row or *sql.Rows.
func scanDailyDiary(s interface{ Scan(...any) error }) (DailyDiary, error) {
	var dia DailyDiary
	var overview, themesJSON, blindsJSON, shelvedJSON, archivesJSON, model sql.NullString
	if err := s.Scan(
		&dia.AgentID, &dia.Date, &overview, &themesJSON, &blindsJSON, &shelvedJSON, &archivesJSON, &model, &dia.GeneratedAt,
	); err != nil {
		return dia, err
	}
	dia.Overview = overview.String
	dia.Model = model.String
	_ = json.Unmarshal([]byte(themesJSON.String), &dia.Themes)
	_ = json.Unmarshal([]byte(blindsJSON.String), &dia.Blindspots)
	_ = json.Unmarshal([]byte(shelvedJSON.String), &dia.Shelved)
	_ = json.Unmarshal([]byte(archivesJSON.String), &dia.Archives)
	if dia.Themes == nil {
		dia.Themes = []DiaryTheme{}
	}
	if dia.Blindspots == nil {
		dia.Blindspots = []DiaryBlindspot{}
	}
	if dia.Shelved == nil {
		dia.Shelved = []DiaryShelved{}
	}
	if dia.Archives == nil {
		dia.Archives = []string{}
	}
	return dia, nil
}

// InsertDailyDiary upserts one diary entry on (agent_id, date). Re-running
// the generator for the same day overwrites cleanly.
func (d *DBStore) InsertDailyDiary(ctx context.Context, dia DailyDiary) error {
	themesJSON, err := json.Marshal(dia.Themes)
	if err != nil {
		return fmt.Errorf("marshal themes: %w", err)
	}
	blindsJSON, err := json.Marshal(dia.Blindspots)
	if err != nil {
		return fmt.Errorf("marshal blindspots: %w", err)
	}
	shelved := dia.Shelved
	if shelved == nil {
		shelved = []DiaryShelved{}
	}
	shelvedJSON, err := json.Marshal(shelved)
	if err != nil {
		return fmt.Errorf("marshal shelved: %w", err)
	}
	archives := dia.Archives
	if archives == nil {
		archives = []string{}
	}
	archivesJSON, err := json.Marshal(archives)
	if err != nil {
		return fmt.Errorf("marshal archives: %w", err)
	}
	if dia.GeneratedAt.IsZero() {
		dia.GeneratedAt = time.Now()
	}
	conflict := ` ON CONFLICT(agent_id, date) DO UPDATE SET
			overview = EXCLUDED.overview,
			themes = EXCLUDED.themes,
			blindspots = EXCLUDED.blindspots,
			shelved = EXCLUDED.shelved,
			archives = EXCLUDED.archives,
			model = EXCLUDED.model,
			generated_at = EXCLUDED.generated_at`
	if d.dialect == "postgres" {
		_, err = d.db.ExecContext(ctx, `INSERT INTO daily_diary (`+dailyDiaryColumns+`)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`+conflict,
			dia.AgentID, dia.Date, dia.Overview, themesJSON, blindsJSON, shelvedJSON, archivesJSON, dia.Model, dia.GeneratedAt)
		return err
	}
	_, err = d.db.ExecContext(ctx, `INSERT INTO daily_diary (`+dailyDiaryColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?)`+conflict,
		dia.AgentID, dia.Date, dia.Overview, themesJSON, blindsJSON, shelvedJSON, archivesJSON, dia.Model, dia.GeneratedAt)
	return err
}

// DeleteDailyDiary removes one (agent, date) row. Returns (false, nil)
// when the day has no diary — deletion is idempotent, the caller decides
// whether to surface that as a 404.
func (d *DBStore) DeleteDailyDiary(ctx context.Context, agentID, date string) (bool, error) {
	var q string
	if d.dialect == "postgres" {
		q = `DELETE FROM daily_diary WHERE agent_id = $1 AND date = $2`
	} else {
		q = `DELETE FROM daily_diary WHERE agent_id = ? AND date = ?`
	}
	res, err := d.db.ExecContext(ctx, q, agentID, date)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GetDailyDiary reads one entry; returns (nil, nil) when absent.
func (d *DBStore) GetDailyDiary(ctx context.Context, agentID, date string) (*DailyDiary, error) {
	var q string
	if d.dialect == "postgres" {
		q = `SELECT ` + dailyDiaryColumns + ` FROM daily_diary WHERE agent_id = $1 AND date = $2`
	} else {
		q = `SELECT ` + dailyDiaryColumns + ` FROM daily_diary WHERE agent_id = ? AND date = ?`
	}
	dia, err := scanDailyDiary(d.db.QueryRowContext(ctx, q, agentID, date))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &dia, nil
}

// ListDailyDiaries returns entries for an agent in [from, to] (inclusive
// on both; dates are YYYY-MM-DD strings), newest first.
func (d *DBStore) ListDailyDiaries(ctx context.Context, agentID, from, to string) ([]DailyDiary, error) {
	var q string
	if d.dialect == "postgres" {
		q = `SELECT ` + dailyDiaryColumns + ` FROM daily_diary WHERE agent_id = $1 AND date >= $2 AND date <= $3 ORDER BY date DESC`
	} else {
		q = `SELECT ` + dailyDiaryColumns + ` FROM daily_diary WHERE agent_id = ? AND date >= ? AND date <= ? ORDER BY date DESC`
	}
	rows, err := d.db.QueryContext(ctx, q, agentID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DailyDiary
	for rows.Next() {
		dia, err := scanDailyDiary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dia)
	}
	return out, rows.Err()
}

// LatestShelvedLedger returns the most recent diary on-or-before `to`
// (YYYY-MM-DD) that carries a non-empty shelved ledger — one narrow row
// (date + shelved only), not a 30-day wide scan. The push sweep and the
// Pass-3 inheritance both key on this "latest non-empty = chain head"
// rule; empty ledgers (Pass 3 off/failed/cleared that day, stored as '[]')
// are not chain heads. Returns (nil, "", nil) when no diary carries one.
// latestNonEmptyDiarySection returns (json, date) of the most recent diary
// dated on or before `to` whose `column` section is non-empty (the
// "what counts as an empty section" predicate lives here once). Callers
// unmarshal into their own typed slice.
func (d *DBStore) latestNonEmptyDiarySection(ctx context.Context, agentID, to, column string) (string, sql.NullString, error) {
	var q string
	if d.dialect == "postgres" {
		q = fmt.Sprintf(`SELECT date, %s FROM daily_diary WHERE agent_id = $1 AND date <= $2
			AND %s IS NOT NULL AND %s <> '' AND %s <> '[]'
			ORDER BY date DESC LIMIT 1`, column, column, column, column)
	} else {
		q = fmt.Sprintf(`SELECT date, %s FROM daily_diary WHERE agent_id = ? AND date <= ?
			AND %s IS NOT NULL AND %s <> '' AND %s <> '[]'
			ORDER BY date DESC LIMIT 1`, column, column, column, column)
	}
	var date string
	var sectionJSON sql.NullString
	err := d.db.QueryRowContext(ctx, q, agentID, to).Scan(&date, &sectionJSON)
	if err == sql.ErrNoRows {
		return "", sql.NullString{}, nil
	}
	if err != nil {
		return "", sql.NullString{}, err
	}
	return date, sectionJSON, nil
}

func (d *DBStore) LatestShelvedLedger(ctx context.Context, agentID, to string) ([]DiaryShelved, string, error) {
	date, shelvedJSON, err := d.latestNonEmptyDiarySection(ctx, agentID, to, "shelved")
	if err != nil || date == "" {
		return nil, "", err
	}
	var out []DiaryShelved
	_ = json.Unmarshal([]byte(shelvedJSON.String), &out)
	return out, date, nil
}

// LatestBlindspots returns the blindspot section of the agent's most
// recent diary dated on or before `to` — the blindspot push sweep's
// source. Only diaries with a non-empty blindspot array match, so (nil,
// "", nil) means "nothing to nudge about" (the generator's 宁可空不凑数
// discipline makes that the common case).
func (d *DBStore) LatestBlindspots(ctx context.Context, agentID, to string) ([]DiaryBlindspot, string, error) {
	date, blindJSON, err := d.latestNonEmptyDiarySection(ctx, agentID, to, "blindspots")
	if err != nil || date == "" {
		return nil, "", err
	}
	var out []DiaryBlindspot
	_ = json.Unmarshal([]byte(blindJSON.String), &out)
	return out, date, nil
}
