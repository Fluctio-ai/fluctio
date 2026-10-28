package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// RecallDebugHit is one surfaced candidate in a recall debug event. Ranks and
// Scores are keyed by retrieval lane ("fts", "vector", "rerank") so offline
// analysis can replay per-lane rankings without re-running retrieval; HitType
// names the dominant signal ("fts", "vector", "both", "rerank").
type RecallDebugHit struct {
	ID      string             `json:"id"`
	Score   float64            `json:"score"` // final score on the gate's scale
	HitType string             `json:"hit_type"`
	Ranks   map[string]int     `json:"ranks,omitempty"`
	Scores  map[string]float64 `json:"scores,omitempty"`
}

// RecallDebugEvent records one recall call's funnel: how many candidates each
// lane produced, how many each gate dropped, and which hits survived with
// per-lane attribution. Pure observability — it answers "why was this
// recalled / why was this missed" without changing recall behavior. The
// bandit-facing memory_recall_events table is separate and untouched.
//
// Query text is stored verbatim, matching memory_recall_events' posture
// (single-owner debug data, pruned by the same retention sweep).
type RecallDebugEvent struct {
	AgentID    string
	Lane       string // wiki | flash_todo | bookmark | mem_inject | memory_search
	Trigger    string // auto (proactive injection) | tool (explicit search call)
	Query      string
	Candidates int               // candidates before the gates
	Gates      map[string]int    // gate name → dropped count
	Params     map[string]any    // knobs in effect (threshold, limit, floor, ...)
	Hits       []RecallDebugHit  // survivors with per-lane attribution
}

// migrateRecallDebugEvents creates the recall_debug_events table.
// Idempotent — CREATE IF NOT EXISTS, safe on every boot.
func (d *DBStore) migrateRecallDebugEvents(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS recall_debug_events (
			id           INTEGER PRIMARY KEY,
			agent_id     TEXT NOT NULL,
			lane         TEXT NOT NULL,
			trigger_kind TEXT NOT NULL DEFAULT '',
			query        TEXT NOT NULL DEFAULT '',
			candidates   INTEGER NOT NULL DEFAULT 0,
			gates        TEXT NOT NULL DEFAULT '',
			params       TEXT NOT NULL DEFAULT '',
			hits         TEXT NOT NULL DEFAULT '',
			created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_recall_debug_agent ON recall_debug_events(agent_id, created_at)`,
		// The retention sweep prunes by created_at alone (no agent
		// predicate), which the composite agent index can't serve.
		`CREATE INDEX IF NOT EXISTS idx_recall_debug_created ON recall_debug_events(created_at)`,
	}
	for _, s := range stmts {
		if _, err := d.db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// recallDebugColumns is the shared INSERT column list. The kb package writes
// the same table through its own *sql.DB handle (it must not import store);
// keep its insertRecallTrace column list in sync with this one.
const recallDebugColumns = `(agent_id, lane, trigger_kind, query, candidates, gates, params, hits)`

// InsertRecallDebugEvent records one recall funnel. Best-effort by contract:
// callers ignore the error so observability can never break a recall.
func (d *DBStore) InsertRecallDebugEvent(ctx context.Context, ev RecallDebugEvent) error {
	// Typed-nil guards must happen on the struct fields: once a nil slice
	// passes through `any`, the interface is non-nil and would marshal to
	// "null" — which the hardcase mining query (hits = '' OR '[]') can't see.
	marshal := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
	gatesJSON, paramsJSON, hitsJSON := "", "", ""
	if ev.Gates != nil {
		gatesJSON = marshal(ev.Gates)
	}
	if ev.Params != nil {
		paramsJSON = marshal(ev.Params)
	}
	if ev.Hits != nil {
		hitsJSON = marshal(ev.Hits)
	}
	q := `INSERT INTO recall_debug_events ` + recallDebugColumns + ` VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if d.dialect == "postgres" {
		q = `INSERT INTO recall_debug_events ` + recallDebugColumns + ` VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	}
	_, err := d.db.ExecContext(ctx, q, ev.AgentID, ev.Lane, ev.Trigger, ev.Query, ev.Candidates, gatesJSON, paramsJSON, hitsJSON)
	return err
}

// ListRecallDebugEvents returns recent debug events for one agent, newest
// first — the raw material for "what died where" inspection. lane "" means
// all lanes. days > 0 keeps only events newer than that many days.
func (d *DBStore) ListRecallDebugEvents(ctx context.Context, agentID, lane string, limit, days int) ([]RecallDebugEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	args := []any{agentID}
	where := `agent_id = ` + d.ph(1)
	if lane != "" {
		where += ` AND lane = ` + d.ph(len(args)+1)
		args = append(args, lane)
	}
	if days > 0 {
		where += ` AND created_at > ` + d.ph(len(args)+1)
		args = append(args, time.Now().Add(-time.Duration(days)*24*time.Hour).UTC().Format("2006-01-02 15:04:05"))
	}
	q := `SELECT lane, trigger_kind, query, candidates, gates, params, hits
		FROM recall_debug_events WHERE ` + where + `
		ORDER BY created_at DESC LIMIT ` + d.ph(len(args)+1)
	args = append(args, limit)
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecallDebugEvent
	for rows.Next() {
		var (
			ev                            RecallDebugEvent
			gatesJSON, paramsJSON, hitsJSON string
		)
		if err := rows.Scan(&ev.Lane, &ev.Trigger, &ev.Query, &ev.Candidates, &gatesJSON, &paramsJSON, &hitsJSON); err != nil {
			return nil, err
		}
		ev.AgentID = agentID
		if gatesJSON != "" {
			_ = json.Unmarshal([]byte(gatesJSON), &ev.Gates)
		}
		if paramsJSON != "" {
			_ = json.Unmarshal([]byte(paramsJSON), &ev.Params)
		}
		if hitsJSON != "" {
			_ = json.Unmarshal([]byte(hitsJSON), &ev.Hits)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// PruneRecallDebugEvents deletes recall_debug_events older than `before`, in
// bounded batches so a large backlog can't pin the write lock (same shape as
// PruneRecallEvents). Audit data, not source data.
func (d *DBStore) PruneRecallDebugEvents(ctx context.Context, before time.Time, batchSize int) (int64, error) {
	if batchSize <= 0 {
		batchSize = 1000
	}
	beforeStr := before.UTC().Format("2006-01-02 15:04:05")
	var total int64
	for {
		res, err := d.db.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM recall_debug_events WHERE id IN (
				SELECT id FROM recall_debug_events WHERE created_at < %s LIMIT %s)`,
				d.ph(1), d.ph(2)),
			beforeStr, batchSize)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < int64(batchSize) {
			break
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
	}
	return total, nil
}

// recallDebugEventCount is a test/debug helper: total rows for one agent.
func (d *DBStore) recallDebugEventCount(ctx context.Context, agentID string) (int, error) {
	var n int
	q := `SELECT COUNT(*) FROM recall_debug_events WHERE agent_id = ?`
	if d.dialect == "postgres" {
		q = `SELECT COUNT(*) FROM recall_debug_events WHERE agent_id = $1`
	}
	err := d.db.QueryRowContext(ctx, q, agentID).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}
