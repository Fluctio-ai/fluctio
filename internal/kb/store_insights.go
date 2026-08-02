package kb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// GetInsights returns the stored deep-reading insights for one article source,
// or (nil, nil) when none have been generated yet. A missing row is not an
// error — callers render the "generate" affordance in that case. Each section
// is decoded independently so a malformed blob in one never hides the rest.
func (s *KBStore) GetInsights(ctx context.Context, agentID, sourceID string) (*ArticleInsights, error) {
	var summaryJSON, quotesJSON, actionsJSON, sproutsJSON, genStr string
	err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT summary, quotes, actions, sprouts, generated_at
			FROM kb_article_insights WHERE source_id = %s AND agent_id = %s`,
			s.ph(1), s.ph(2)),
		sourceID, agentID).Scan(&summaryJSON, &quotesJSON, &actionsJSON, &sproutsJSON, &genStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get insights: %w", err)
	}
	ins := &ArticleInsights{SourceID: sourceID}
	_ = json.Unmarshal([]byte(summaryJSON), &ins.Summary)
	_ = json.Unmarshal([]byte(quotesJSON), &ins.Quotes)
	_ = json.Unmarshal([]byte(actionsJSON), &ins.Actions)
	_ = json.Unmarshal([]byte(sproutsJSON), &ins.Sprouts)
	if t, err := time.Parse(time.RFC3339, genStr); err == nil {
		ins.GeneratedAt = t
	}
	return ins, nil
}

// SaveInsights upserts the four insight sections for one article source,
// stamping generated_at = now. Each section is stored as its own JSON blob so
// the page can render them independently and a section can be re-built alone.
func (s *KBStore) SaveInsights(ctx context.Context, agentID, sourceID string, ins *ArticleInsights) error {
	now := time.Now().UTC().Format(time.RFC3339)
	summaryJSON, _ := json.Marshal(ins.Summary)
	quotesJSON, _ := json.Marshal(ins.Quotes)
	actionsJSON, _ := json.Marshal(ins.Actions)
	sproutsJSON, _ := json.Marshal(ins.Sprouts)
	_, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO kb_article_insights (source_id, agent_id, summary, quotes, actions, sprouts, generated_at)
			VALUES (%s,%s,%s,%s,%s,%s,%s)
			ON CONFLICT(source_id) DO UPDATE SET
				agent_id=excluded.agent_id, summary=excluded.summary, quotes=excluded.quotes,
				actions=excluded.actions, sprouts=excluded.sprouts, generated_at=excluded.generated_at`,
			s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5), s.ph(6), s.ph(7)),
		sourceID, agentID, string(summaryJSON), string(quotesJSON), string(actionsJSON), string(sproutsJSON), now)
	if err != nil {
		return fmt.Errorf("save insights: %w", err)
	}
	return nil
}

// DeleteInsights removes the insights row for a source. Called by DeleteSource
// (cascade) and exposed for a future "regenerate" that wants to clear first.
func (s *KBStore) DeleteInsights(ctx context.Context, agentID, sourceID string) error {
	_, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM kb_article_insights WHERE source_id = %s AND agent_id = %s`, s.ph(1), s.ph(2)),
		sourceID, agentID)
	return err
}
