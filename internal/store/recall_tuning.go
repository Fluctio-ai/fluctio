package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// DefaultMMRLambda is the starting MMR lambda (relevance vs diversity)
// before any bandit tuning has selected a better value for an agent.
const DefaultMMRLambda = 0.6

// migrateRecallTuning creates the tables that back the bandit-style MMR
// lambda optimization:
//   - agent_recall_tuning: one row per agent holding the current best
//     mmr_lambda. Updated when stage-2b feedback shows an explored
//     lambda is statistically better.
//   - memory_recall_events: one row per memory_search call, recording
//     the lambda used, whether it was an ε-greedy exploration, and the
//     surfaced summary IDs. recall_id links a later thumbs-up/down to
//     the lambda that produced the recall.
//
// Idempotent — every stmt is CREATE ... IF NOT EXISTS, safe on every boot.
func (d *DBStore) migrateRecallTuning(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS agent_recall_tuning (
			agent_id   TEXT PRIMARY KEY,
			mmr_lambda REAL NOT NULL DEFAULT 0.6,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS memory_recall_events (
			id          INTEGER PRIMARY KEY,
			recall_id   TEXT NOT NULL,
			agent_id    TEXT NOT NULL,
			lambda      REAL NOT NULL,
			explored    INTEGER NOT NULL DEFAULT 0,
			summary_ids TEXT NOT NULL,
			created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_recall_event_recall_id ON memory_recall_events(recall_id)`,
		`CREATE INDEX IF NOT EXISTS idx_recall_event_agent ON memory_recall_events(agent_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS memory_recall_feedback (
			id         INTEGER PRIMARY KEY,
			recall_id  TEXT NOT NULL,
			up         INTEGER NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_recall_feedback_recall_id ON memory_recall_feedback(recall_id)`,
	}
	for _, s := range stmts {
		if _, err := d.db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// RecallEvent records one memory_search call for stage-2b feedback linkage.
type RecallEvent struct {
	RecallID   string  // unique id surfaced (later) for thumbs-up/down
	AgentID    string
	Lambda     float64 // the MMR lambda actually used
	Explored   bool    // true if this was an ε-greedy exploration
	SummaryIDs []int64 // surfaced summary IDs
	CreatedAt  time.Time
}

// GetAgentMMRLambda returns the agent's current best MMR lambda, or
// DefaultMMRLambda when the agent has no tuning row yet.
func (d *DBStore) GetAgentMMRLambda(ctx context.Context, agentID string) (float64, error) {
	var lambda float64
	q := `SELECT mmr_lambda FROM agent_recall_tuning WHERE agent_id = ?`
	if d.dialect == "postgres" {
		q = `SELECT mmr_lambda FROM agent_recall_tuning WHERE agent_id = $1`
	}
	err := d.db.QueryRowContext(ctx, q, agentID).Scan(&lambda)
	if err == sql.ErrNoRows {
		return DefaultMMRLambda, nil
	}
	if err != nil {
		return DefaultMMRLambda, err
	}
	return lambda, nil
}

// SetAgentMMRLambda upserts the agent's current best MMR lambda. Called
// by the stage-2b upgrade path when feedback shows an explored lambda wins.
func (d *DBStore) SetAgentMMRLambda(ctx context.Context, agentID string, lambda float64) error {
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO agent_recall_tuning (agent_id, mmr_lambda) VALUES ($1, $2)
			 ON CONFLICT (agent_id) DO UPDATE SET mmr_lambda = EXCLUDED.mmr_lambda, updated_at = CURRENT_TIMESTAMP`,
			agentID, lambda)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO agent_recall_tuning (agent_id, mmr_lambda) VALUES (?, ?)
		 ON CONFLICT(agent_id) DO UPDATE SET mmr_lambda = excluded.mmr_lambda, updated_at = CURRENT_TIMESTAMP`,
		agentID, lambda)
	return err
}

// InsertRecallEvent records a single memory_search recall so a later
// feedback signal can be attributed to the lambda that produced it.
func (d *DBStore) InsertRecallEvent(ctx context.Context, ev RecallEvent) error {
	idsJSON, err := json.Marshal(ev.SummaryIDs)
	if err != nil {
		return fmt.Errorf("marshal summary ids: %w", err)
	}
	explored := 0
	if ev.Explored {
		explored = 1
	}
	q := `INSERT INTO memory_recall_events (recall_id, agent_id, lambda, explored, summary_ids)
	      VALUES (?, ?, ?, ?, ?)`
	if d.dialect == "postgres" {
		q = `INSERT INTO memory_recall_events (recall_id, agent_id, lambda, explored, summary_ids)
		     VALUES ($1, $2, $3, $4, $5)`
	}
	_, err = d.db.ExecContext(ctx, q, ev.RecallID, ev.AgentID, ev.Lambda, explored, string(idsJSON))
	return err
}

// LambdaStat summarizes feedback for one explored lambda value.
type LambdaStat struct {
	Lambda float64
	Ups    int
	Downs  int
}

// InsertRecallFeedback records a thumbs-up/down against a recall_id. The
// recall_id links back to the lambda that produced the recall via
// memory_recall_events.
func (d *DBStore) InsertRecallFeedback(ctx context.Context, recallID string, up bool) error {
	upInt := 0
	if up {
		upInt = 1
	}
	q := `INSERT INTO memory_recall_feedback (recall_id, up) VALUES (?, ?)`
	if d.dialect == "postgres" {
		q = `INSERT INTO memory_recall_feedback (recall_id, up) VALUES ($1, $2)`
	}
	_, err := d.db.ExecContext(ctx, q, recallID, upInt)
	return err
}

// GetLambdaFeedbackStats aggregates feedback per explored lambda for one
// agent. Only explored recalls (e.explored=1) count — exploit recalls
// reuse the current best lambda and carry no exploration signal.
func (d *DBStore) GetLambdaFeedbackStats(ctx context.Context, agentID string) ([]LambdaStat, error) {
	q := `SELECT e.lambda,
			SUM(CASE WHEN f.up = 1 THEN 1 ELSE 0 END),
			SUM(CASE WHEN f.up = 0 THEN 1 ELSE 0 END)
		FROM memory_recall_events e
		JOIN memory_recall_feedback f ON f.recall_id = e.recall_id
		WHERE e.agent_id = ? AND e.explored = 1
		GROUP BY e.lambda`
	if d.dialect == "postgres" {
		q = `SELECT e.lambda,
			SUM(CASE WHEN f.up = 1 THEN 1 ELSE 0 END),
			SUM(CASE WHEN f.up = 0 THEN 1 ELSE 0 END)
		FROM memory_recall_events e
		JOIN memory_recall_feedback f ON f.recall_id = e.recall_id
		WHERE e.agent_id = $1 AND e.explored = 1
		GROUP BY e.lambda`
	}
	rows, err := d.db.QueryContext(ctx, q, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stats []LambdaStat
	for rows.Next() {
		var s LambdaStat
		if err := rows.Scan(&s.Lambda, &s.Ups, &s.Downs); err != nil {
			return nil, err
		}
		stats = append(stats, s)
	}
	return stats, rows.Err()
}

// recallUpgradeMinSamples / recallUpgradeWinThreshold gate a lambda
// upgrade: an explored lambda needs at least this many feedback samples
// and this win rate before it can replace the current best.
const (
	recallUpgradeMinSamples   = 20
	recallUpgradeWinThreshold = 0.6
)

// TryUpgradeLambda checks whether feedback shows an explored lambda is
// statistically stronger than the agent's current best, and if so,
// promotes it (seesaw: only upgrades when the candidate beats the
// current lambda's own win rate, or the current has no data). Returns
// whether an upgrade happened and the resulting lambda.
func (d *DBStore) TryUpgradeLambda(ctx context.Context, agentID string) (bool, float64, error) {
	stats, err := d.GetLambdaFeedbackStats(ctx, agentID)
	if err != nil {
		return false, 0, err
	}
	current, err := d.GetAgentMMRLambda(ctx, agentID)
	if err != nil {
		return false, 0, err
	}

	bestLambda := current
	bestRate := -1.0
	currentRate := -1.0
	for _, s := range stats {
		total := s.Ups + s.Downs
		if total < recallUpgradeMinSamples {
			continue
		}
		rate := float64(s.Ups) / float64(total)
		if s.Lambda == current {
			currentRate = rate
			continue
		}
		if rate > recallUpgradeWinThreshold && rate > bestRate {
			bestRate = rate
			bestLambda = s.Lambda
		}
	}

	// Upgrade only if a candidate is both strong on its own and beats
	// the current lambda (or the current has insufficient data).
	if bestLambda != current && (currentRate < 0 || bestRate > currentRate) {
		if err := d.SetAgentMMRLambda(ctx, agentID, bestLambda); err != nil {
			return false, current, err
		}
		return true, bestLambda, nil
	}
	return false, current, nil
}
