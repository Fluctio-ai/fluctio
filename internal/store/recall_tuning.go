package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/embedding"
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
			user_id     TEXT NOT NULL DEFAULT '',
			session_key TEXT NOT NULL DEFAULT '',
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

// migrateRecallEventSession adds session_key to memory_recall_events for
// existing DBs (the column ships in migrateRecallTuning's CREATE for new
// DBs). Idempotent via tableHasColumn. session_key lets the implicit-
// feedback sweep locate the N messages after a recall.
func (d *DBStore) migrateRecallEventSession(ctx context.Context) error {
	hasTable, err := d.tableExists(ctx, "memory_recall_events")
	if err != nil {
		return err
	}
	if !hasTable {
		return nil
	}
	has, err := d.tableHasColumn(ctx, "memory_recall_events", "session_key")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err = d.db.ExecContext(ctx, `ALTER TABLE memory_recall_events ADD COLUMN session_key TEXT NOT NULL DEFAULT ''`)
	return err
}

// migrateRecallEventUser adds user_id to memory_recall_events for existing
// DBs (ships in migrateRecallTuning's CREATE for new DBs). The implicit-
// feedback sweep needs it to look up session messages (queried by
// userID + sessionKey). Idempotent via tableHasColumn.
func (d *DBStore) migrateRecallEventUser(ctx context.Context) error {
	hasTable, err := d.tableExists(ctx, "memory_recall_events")
	if err != nil {
		return err
	}
	if !hasTable {
		return nil
	}
	has, err := d.tableHasColumn(ctx, "memory_recall_events", "user_id")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err = d.db.ExecContext(ctx, `ALTER TABLE memory_recall_events ADD COLUMN user_id TEXT NOT NULL DEFAULT ''`)
	return err
}

// RecallEvent records one memory_search call for stage-2b feedback linkage.
type RecallEvent struct {
	RecallID   string  // unique id surfaced (later) for thumbs-up/down
	AgentID    string
	UserID     string  // owner user_id (implicit feedback: look up session messages)
	SessionKey string  // chat session the recall happened in (implicit feedback)
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
	q := `INSERT INTO memory_recall_events (recall_id, agent_id, user_id, session_key, lambda, explored, summary_ids)
	      VALUES (?, ?, ?, ?, ?, ?, ?)`
	if d.dialect == "postgres" {
		q = `INSERT INTO memory_recall_events (recall_id, agent_id, user_id, session_key, lambda, explored, summary_ids)
		     VALUES ($1, $2, $3, $4, $5, $6, $7)`
	}
	_, err = d.db.ExecContext(ctx, q, ev.RecallID, ev.AgentID, ev.UserID, ev.SessionKey, ev.Lambda, explored, string(idsJSON))
	return err
}

// GetRecallEventAgentID resolves which agent a recall_id belongs to, so
// feedback can be routed to that agent's tuning without trusting the
// client to name the agent. Returns "" + nil error when the id is unknown.
func (d *DBStore) GetRecallEventAgentID(ctx context.Context, recallID string) (string, error) {
	var agentID string
	q := `SELECT agent_id FROM memory_recall_events WHERE recall_id = ?`
	if d.dialect == "postgres" {
		q = `SELECT agent_id FROM memory_recall_events WHERE recall_id = $1`
	}
	err := d.db.QueryRowContext(ctx, q, recallID).Scan(&agentID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return agentID, nil
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

// RecallStats summarizes one agent's recall activity for the tuning panel.
type RecallStats struct {
	TotalRecalls    int
	ExploredRecalls int
}

// GetRecallStats returns total + explored recall counts for an agent.
func (d *DBStore) GetRecallStats(ctx context.Context, agentID string) (RecallStats, error) {
	var s RecallStats
	q := `SELECT COUNT(*), COALESCE(SUM(explored), 0) FROM memory_recall_events WHERE agent_id = ?`
	if d.dialect == "postgres" {
		q = `SELECT COUNT(*), COALESCE(SUM(explored), 0) FROM memory_recall_events WHERE agent_id = $1`
	}
	err := d.db.QueryRowContext(ctx, q, agentID).Scan(&s.TotalRecalls, &s.ExploredRecalls)
	return s, err
}

// ListRecentRecallEvents returns the agent's most recent recall events
// (newest first), for the tuning panel's manual-feedback section.
func (d *DBStore) ListRecentRecallEvents(ctx context.Context, agentID string, limit int) ([]RecallEvent, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT recall_id, agent_id, user_id, session_key, lambda, explored, summary_ids, created_at
		FROM memory_recall_events
		WHERE agent_id = ?
		ORDER BY created_at DESC LIMIT ?`
	if d.dialect == "postgres" {
		q = `SELECT recall_id, agent_id, user_id, session_key, lambda, explored, summary_ids, created_at
			FROM memory_recall_events
			WHERE agent_id = $1
			ORDER BY created_at DESC LIMIT $2`
	}
	rows, err := d.db.QueryContext(ctx, q, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecallEvent
	for rows.Next() {
		var (
			ev          RecallEvent
			explored    int
			summaryJSON string
		)
		if err := rows.Scan(&ev.RecallID, &ev.AgentID, &ev.UserID, &ev.SessionKey, &ev.Lambda, &explored, &summaryJSON, &ev.CreatedAt); err != nil {
			return nil, err
		}
		ev.Explored = explored != 0
		_ = json.Unmarshal([]byte(summaryJSON), &ev.SummaryIDs)
		if ev.SummaryIDs == nil {
			ev.SummaryIDs = []int64{}
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// PreviewRecall runs a basic recall (FTS + scoring) for the tuning panel's
// test box. NOTE: does not include vector recall, reranker, or MMR — it's
// a preview of which summaries match the query, not a full reproduction
// of memory_search. Use it to eyeball recall coverage, not lambda effects.
func (d *DBStore) PreviewRecall(ctx context.Context, agentID, query string, limit int) ([]ConversationSummary, error) {
	if limit <= 0 {
		limit = 10
	}
	hits, err := d.SearchConversationSummariesFTS(ctx, agentID, query, limit*3)
	if err != nil {
		return nil, err
	}
	return reRankSummaries(hits, query, limit), nil
}

// ListSessionMessagesAfterTime returns up to limit messages in a session
// created after `after`, ascending by created_at. Used by the implicit-
// feedback sweep to read what the user said following a recall.
func (d *DBStore) ListSessionMessagesAfterTime(ctx context.Context, userID, agentID, sessionKey string, after time.Time, limit int) ([]SessionMessage, error) {
	if limit <= 0 {
		limit = 10
	}
	var rows *sql.Rows
	var err error
	if d.dialect == "postgres" {
		rows, err = d.db.QueryContext(ctx,
			`SELECT seq, role, content, content_parts, tool_calls, tool_call_id, name, metadata, thinking, raw_assistant, origin, created_at
			 FROM session_messages
			 WHERE user_id = $1 AND agent_id = $2 AND session_key = $3 AND created_at > $4
			 ORDER BY created_at ASC LIMIT $5`,
			userID, agentID, sessionKey, after, limit)
	} else {
		rows, err = d.db.QueryContext(ctx,
			`SELECT seq, role, content, content_parts, tool_calls, tool_call_id, name, metadata, thinking, raw_assistant, origin, created_at
			 FROM session_messages
			 WHERE user_id = ? AND agent_id = ? AND session_key = ? AND created_at > ?
			 ORDER BY created_at ASC LIMIT ?`,
			userID, agentID, sessionKey, after, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionMessage
	for rows.Next() {
		var m SessionMessage
		var contentParts, toolCalls, metadata, rawAssistant string
		if err := rows.Scan(&m.Seq, &m.Role, &m.Content, &contentParts, &toolCalls, &m.ToolCallID, &m.Name, &metadata, &m.Thinking, &rawAssistant, &m.Origin, &m.Timestamp); err != nil {
			return nil, err
		}
		if contentParts != "" && contentParts != "null" {
			var v interface{}
			if json.Unmarshal([]byte(contentParts), &v) == nil {
				m.ContentParts = v
			}
		}
		if toolCalls != "" && toolCalls != "null" {
			var v interface{}
			if json.Unmarshal([]byte(toolCalls), &v) == nil {
				m.ToolCalls = v
			}
		}
		if metadata != "" && metadata != "null" {
			var v map[string]interface{}
			if json.Unmarshal([]byte(metadata), &v) == nil {
				m.Metadata = v
			}
		}
		if rawAssistant != "" && rawAssistant != "null" {
			m.RawAssistant = json.RawMessage(rawAssistant)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ImplicitFeedbackConfig tunes the implicit-feedback sweep.
type ImplicitFeedbackConfig struct {
	WindowMessages int     // post-recall messages to consider
	UpThreshold    float64 // max cosine >= this records thumbs-up
	DownThreshold  float64 // max cosine <= this records thumbs-down
	MaxAgeMinutes  int     // only sweep events older than this (give convo time)
	BatchLimit     int     // max events per sweep
}

// DefaultImplicitFeedbackConfig — 3-message window, cosine 0.5/0.3 split,
// 10-minute cooldown, 50-event batch.
var DefaultImplicitFeedbackConfig = ImplicitFeedbackConfig{
	WindowMessages: 3,
	UpThreshold:    0.5,
	DownThreshold:  0.3,
	MaxAgeMinutes:  10,
	BatchLimit:     50,
}

// SweepImplicitFeedback is the implicit signal source for the bandit: for
// each explored recall whose conversation has progressed and has no
// feedback yet, it embeds the messages that followed and compares (max
// cosine) to the recalled summaries' embeddings. Stays on topic → up;
// clearly leaves → down; uncertain middle is unrecorded. Returns the
// feedback count written. Caller builds the embedder from agent memory
// config; store imports embedding without a cycle.
func (d *DBStore) SweepImplicitFeedback(ctx context.Context, agentID string, emb embedding.Embedder, cfg ImplicitFeedbackConfig) (int, error) {
	if emb == nil || !emb.Available() {
		return 0, nil
	}
	if cfg.WindowMessages <= 0 {
		cfg = DefaultImplicitFeedbackConfig
	}
	cutoff := time.Now().Add(-time.Duration(cfg.MaxAgeMinutes) * time.Minute)
	q := `SELECT recall_id, agent_id, user_id, session_key, summary_ids, created_at
		FROM memory_recall_events
		WHERE explored = 1 AND agent_id = ? AND session_key != '' AND user_id != '' AND created_at < ?
		  AND NOT EXISTS (SELECT 1 FROM memory_recall_feedback f WHERE f.recall_id = memory_recall_events.recall_id)
		ORDER BY created_at LIMIT ?`
	if d.dialect == "postgres" {
		q = `SELECT recall_id, agent_id, user_id, session_key, summary_ids, created_at
			FROM memory_recall_events
			WHERE explored = 1 AND agent_id = $1 AND session_key != '' AND user_id != '' AND created_at < $2
			  AND NOT EXISTS (SELECT 1 FROM memory_recall_feedback f WHERE f.recall_id = memory_recall_events.recall_id)
			ORDER BY created_at LIMIT $3`
	}
	rows, err := d.db.QueryContext(ctx, q, agentID, cutoff, cfg.BatchLimit)
	if err != nil {
		return 0, err
	}

	// Materialize events and close rows BEFORE issuing the per-event
	// follow-up queries (ListSessionMessagesAfterTime, GetConversation-
	// SummaryEmbeddings). On the shared-cache in-memory sqlite the pool
	// serializes hard, so a still-open rows would deadlock those queries.
	type pendingEvent struct {
		recallID, agentID, userID, sessionKey string
		summaryIDs                            []int64
		created                               time.Time
	}
	var events []pendingEvent
	for rows.Next() {
		var (
			recallID       string
			agentID        string
			userID         string
			sessionKey     string
			summaryIDsJSON string
			created        time.Time
		)
		if err := rows.Scan(&recallID, &agentID, &userID, &sessionKey, &summaryIDsJSON, &created); err != nil {
			rows.Close()
			return 0, err
		}
		var summaryIDs []int64
		if err := json.Unmarshal([]byte(summaryIDsJSON), &summaryIDs); err != nil || len(summaryIDs) == 0 {
			continue
		}
		events = append(events, pendingEvent{recallID, agentID, userID, sessionKey, summaryIDs, created})
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return 0, rowsErr
	}

	processed := 0
	for _, ev := range events {
		msgs, err := d.ListSessionMessagesAfterTime(ctx, ev.userID, ev.agentID, ev.sessionKey, ev.created, cfg.WindowMessages)
		if err != nil || len(msgs) < cfg.WindowMessages {
			continue
		}
		texts := make([]string, 0, len(msgs))
		for _, m := range msgs {
			if strings.TrimSpace(m.Content) != "" {
				texts = append(texts, m.Content)
			}
		}
		if len(texts) == 0 {
			continue
		}
		sumEmbs, err := d.GetConversationSummaryEmbeddings(ctx, ev.summaryIDs)
		if err != nil || len(sumEmbs) == 0 {
			continue
		}
		msgVecs, err := emb.Embed(ctx, texts)
		if err != nil || len(msgVecs) == 0 {
			continue
		}
		maxCos := 0.0
		for _, mv := range msgVecs {
			for _, sv := range sumEmbs {
				if c := cosineF32(mv, sv); c > maxCos {
					maxCos = c
				}
			}
		}
		if maxCos >= cfg.UpThreshold {
			if err := d.InsertRecallFeedback(ctx, ev.recallID, true); err == nil {
				processed++
			}
		} else if maxCos <= cfg.DownThreshold {
			if err := d.InsertRecallFeedback(ctx, ev.recallID, false); err == nil {
				processed++
			}
		}
	}
	return processed, nil
}

// cosineF32 returns the cosine similarity of two equal-length float32
// vectors as a float64 in [-1,1]; 0 for empty/mismatched input.
func cosineF32(a, b []float32) float64 {
	n := len(a)
	if n == 0 || n != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		af, bf := float64(a[i]), float64(b[i])
		dot += af * bf
		na += af * af
		nb += bf * bf
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
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
