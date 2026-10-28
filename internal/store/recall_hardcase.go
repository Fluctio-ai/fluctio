package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
)

// Hardcase mining: zero-hit recall queries grouped into a review queue.
// Repetitive misses are the cheapest golden-case source — the MimirQ lesson
// is to mine them deterministically instead of triaging one-by-one. The
// queue is owner-reviewed: a hardcase graduates into the golden set only
// when a human decides what SHOULD have been hit (recallcheck drafts carry
// empty expect_ids for exactly that reason).

// migrateRecallHardcases creates the hardcase queue and the sweep watermark
// table. Idempotent — CREATE IF NOT EXISTS on every boot.
func (d *DBStore) migrateRecallHardcases(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS recall_hardcases (
			id          INTEGER PRIMARY KEY,
			agent_id    TEXT NOT NULL,
			query_hash  TEXT NOT NULL,
			query       TEXT NOT NULL DEFAULT '',
			lanes       TEXT NOT NULL DEFAULT '',
			count       INTEGER NOT NULL DEFAULT 1,
			first_seen  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			last_seen   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			status      TEXT NOT NULL DEFAULT 'pending'
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_hardcase_key ON recall_hardcases(agent_id, query_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_hardcase_status ON recall_hardcases(agent_id, status, count)`,
		// Sweep watermark: single-row key/value so mined windows never
		// overlap (double-counting occurrences).
		`CREATE TABLE IF NOT EXISTS recall_sweep_meta (
			k TEXT PRIMARY KEY,
			v TEXT NOT NULL DEFAULT ''
		)`,
	}
	for _, s := range stmts {
		if _, err := d.db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// HardcaseQueryHash normalizes a query for grouping: trimmed + lowercased,
// so " Milk " and "milk" aggregate. PII posture matches recall_debug_events
// (owner-debug data, single-user).
func HardcaseQueryHash(query string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(query))))
	return fmt.Sprintf("%x", sum[:8])
}

// sweepMetaGet/sweepMetaSet read and write the single-row watermark.
func (d *DBStore) sweepMetaGet(ctx context.Context, key string) (string, error) {
	var v string
	err := d.db.QueryRowContext(ctx,
		`SELECT v FROM recall_sweep_meta WHERE k = `+d.ph(1), key).Scan(&v)
	return v, err
}

func (d *DBStore) sweepMetaSet(ctx context.Context, key, value string) error {
	q := `INSERT INTO recall_sweep_meta (k, v) VALUES (?, ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v`
	if d.dialect == "postgres" {
		q = `INSERT INTO recall_sweep_meta (k, v) VALUES ($1, $2)
			ON CONFLICT(k) DO UPDATE SET v = EXCLUDED.v`
	}
	_, err := d.db.ExecContext(ctx, q, key, value)
	return err
}

const hardcaseWatermarkKey = "hardcase_last_mine"

// MineHardcases aggregates zero-hit recall_debug_events newer than the
// last watermark into recall_hardcases (upsert by agent+query hash, so
// repeats increment count instead of duplicating). Returns the number of
// groups upserted. First run mines the last `lookback` (default 24h).
// Zero-hit means the hits column is empty or '[]' — whatever the gates
// were, nothing survived to show the user.
func (d *DBStore) MineHardcases(ctx context.Context, lookback time.Duration) (int, error) {
	has, err := d.tableExists(ctx, "recall_debug_events")
	if err != nil || !has {
		return 0, err
	}
	sinceStr, serr := d.sweepMetaGet(ctx, hardcaseWatermarkKey)
	var since time.Time
	if serr != nil || sinceStr == "" {
		since = time.Now().Add(-lookback)
	} else if t, err := time.Parse(time.RFC3339, sinceStr); err == nil {
		since = t
	} else {
		since = time.Now().Add(-lookback)
	}

	sinceFmt := since.UTC().Format("2006-01-02 15:04:05")
	q := `SELECT agent_id, query, COUNT(*), GROUP_CONCAT(DISTINCT lane)
		FROM recall_debug_events
		WHERE created_at > ` + d.ph(1) + ` AND (hits = '' OR hits = '[]')
		GROUP BY agent_id, query`
	if d.dialect == "postgres" {
		q = `SELECT agent_id, query, COUNT(*), STRING_AGG(DISTINCT lane, ',')
			FROM recall_debug_events
			WHERE created_at > ` + d.ph(1) + ` AND (hits = '' OR hits = '[]')
			GROUP BY agent_id, query`
	}
	rows, err := d.db.QueryContext(ctx, q, sinceFmt)
	if err != nil {
		return 0, err
	}
	type group struct {
		agentID, query string
		n              int
		lanes          string
	}
	var groups []group
	for rows.Next() {
		var g group
		if err := rows.Scan(&g.agentID, &g.query, &g.n, &g.lanes); err != nil {
			rows.Close()
			return 0, err
		}
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	upsert := `INSERT INTO recall_hardcases (agent_id, query_hash, query, lanes, count, last_seen)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`
	if d.dialect == "postgres" {
		upsert = `INSERT INTO recall_hardcases (agent_id, query_hash, query, lanes, count, last_seen)
			VALUES ($1, $2, $3, $4, $5, CURRENT_TIMESTAMP)`
	}
	upsert += `
		ON CONFLICT(agent_id, query_hash) DO UPDATE SET
			count = count + excluded.count,
			lanes = excluded.lanes,
			last_seen = CURRENT_TIMESTAMP`
	n := 0
	touched := map[string]bool{}
	for _, g := range groups {
		h := HardcaseQueryHash(g.query)
		// Lanes union: keep the existing set plus the newly seen ones.
		var existing string
		_ = d.db.QueryRowContext(ctx,
			`SELECT lanes FROM recall_hardcases WHERE agent_id = `+d.ph(1)+` AND query_hash = `+d.ph(2),
			g.agentID, h).Scan(&existing)
		lanes := unionCSV(existing, g.lanes)
		if _, err := d.db.ExecContext(ctx, upsert, g.agentID, h, g.query, lanes, g.n); err != nil {
			return n, err
		}
		// Case-normalized duplicates collapse onto one row via the unique
		// (agent, hash) key — count distinct rows, not SQL groups.
		if !touched[g.agentID+"\x00"+h] {
			touched[g.agentID+"\x00"+h] = true
			n++
		}
	}
	// Advance the watermark only after every group landed: a failed run
	// leaves the watermark put so the next tick re-mines the same window
	// (idsempotent upserts make a re-run safe; a lost window doesn't).
	return n, d.sweepMetaSet(ctx, hardcaseWatermarkKey, time.Now().UTC().Format(time.RFC3339))
}

// unionCSV merges two comma-separated lane lists without duplicates.
func unionCSV(a, b string) string {
	seen := map[string]bool{}
	var out []string
	for _, part := range append(strings.Split(a, ","), strings.Split(b, ",")...) {
		p := strings.TrimSpace(part)
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return strings.Join(out, ",")
}

// Hardcase is one row of the review queue.
type Hardcase struct {
	Query     string    `json:"query"`
	Lanes     string    `json:"lanes"`
	Count     int       `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Status    string    `json:"status"`
}

// ListHardcases returns the agent's queue, most frequent first. status ""
// means pending only.
func (d *DBStore) ListHardcases(ctx context.Context, agentID, status string, limit int) ([]Hardcase, error) {
	if limit <= 0 {
		limit = 50
	}
	where := `agent_id = ` + d.ph(1)
	args := []any{agentID}
	if status != "" {
		where += ` AND status = ` + d.ph(len(args)+1)
		args = append(args, status)
	} else {
		where += ` AND status = 'pending'`
	}
	q := `SELECT query, lanes, count, first_seen, last_seen, status
		FROM recall_hardcases WHERE ` + where + `
		ORDER BY count DESC, last_seen DESC LIMIT ` + d.ph(len(args)+1)
	args = append(args, limit)
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hardcase
	for rows.Next() {
		var h Hardcase
		if err := rows.Scan(&h.Query, &h.Lanes, &h.Count, &h.FirstSeen, &h.LastSeen, &h.Status); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SetHardcaseStatus moves a queued query to reviewed/dismissed/golden.
func (d *DBStore) SetHardcaseStatus(ctx context.Context, agentID, queryHash, status string) error {
	// Args are bound by POSITION (sqlite "?"), so they follow the literal
	// placeholder order, not the ph() numbering.
	q := `UPDATE recall_hardcases SET status = ` + d.ph(3) + ` WHERE agent_id = ` + d.ph(1) + ` AND query_hash = ` + d.ph(2)
	_, err := d.db.ExecContext(ctx, q, status, agentID, queryHash)
	return err
}
