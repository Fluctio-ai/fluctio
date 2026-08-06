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

// DailyDiary is one day's generated diary for one agent.
type DailyDiary struct {
	AgentID     string           `json:"agentId"`
	Date        string           `json:"date"` // YYYY-MM-DD, UTC+8
	Overview    string           `json:"overview"`
	Themes      []DiaryTheme     `json:"themes"`
	Blindspots  []DiaryBlindspot `json:"blindspots"`
	Archives    []string         `json:"archives"`
	Model       string           `json:"model"`
	GeneratedAt time.Time        `json:"generatedAt"`
}

const dailyDiaryColumns = `agent_id, date, overview, themes, blindspots, archives, model, generated_at`

// scanDailyDiary scans one row from either a *sql.Row or *sql.Rows.
func scanDailyDiary(s interface{ Scan(...any) error }) (DailyDiary, error) {
	var dia DailyDiary
	var overview, themesJSON, blindsJSON, archivesJSON, model sql.NullString
	if err := s.Scan(
		&dia.AgentID, &dia.Date, &overview, &themesJSON, &blindsJSON, &archivesJSON, &model, &dia.GeneratedAt,
	); err != nil {
		return dia, err
	}
	dia.Overview = overview.String
	dia.Model = model.String
	_ = json.Unmarshal([]byte(themesJSON.String), &dia.Themes)
	_ = json.Unmarshal([]byte(blindsJSON.String), &dia.Blindspots)
	_ = json.Unmarshal([]byte(archivesJSON.String), &dia.Archives)
	if dia.Themes == nil {
		dia.Themes = []DiaryTheme{}
	}
	if dia.Blindspots == nil {
		dia.Blindspots = []DiaryBlindspot{}
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
			archives = EXCLUDED.archives,
			model = EXCLUDED.model,
			generated_at = EXCLUDED.generated_at`
	if d.dialect == "postgres" {
		_, err = d.db.ExecContext(ctx, `INSERT INTO daily_diary (`+dailyDiaryColumns+`)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`+conflict,
			dia.AgentID, dia.Date, dia.Overview, themesJSON, blindsJSON, archivesJSON, dia.Model, dia.GeneratedAt)
		return err
	}
	_, err = d.db.ExecContext(ctx, `INSERT INTO daily_diary (`+dailyDiaryColumns+`)
		VALUES (?,?,?,?,?,?,?,?)`+conflict,
		dia.AgentID, dia.Date, dia.Overview, themesJSON, blindsJSON, archivesJSON, dia.Model, dia.GeneratedAt)
	return err
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
