package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ShelvedCandidate is one open loop the summary extractor noticed in a
// conversation window: a topic the user deferred, a promise they made, or
// a question the assistant asked that went unanswered. It is a signal, not
// a task — the diary's shelved ledger (and later the accept/dismiss UI)
// reads these rows. The source anchor is (SessionKey, Seq) into
// session_messages; the raw excerpt is NOT duplicated here.
type ShelvedCandidate struct {
	ID           int64
	AgentID      string
	Kind         string // deferred | commitment | awaiting_user
	Status       string // open | resolved | dropped
	Topic        string
	Context      string
	Reason       string
	SessionKey   string
	Seq          int
	Since        string // YYYY-MM-DD first seen
	LastSeenAt   time.Time
	Source       string // summary | diary_fallback | manual
	Extractor    string // model@prompt-version
	ActionKind   string // todo | workflow | task | reminder (unused until accept)
	ActionRef    string
	ActionPrompt string
	SnoozeUntil  sql.NullTime
	ResolvedAt   sql.NullTime
	Resolution   string
	MergedInto   int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const shelvedCandidateColumns = `id, agent_id, kind, status, topic, context, reason,
	session_key, seq, since, last_seen_at, source, extractor,
	action_kind, action_ref, action_prompt, snooze_until, resolved_at,
	resolution, merged_into, created_at, updated_at`

// UpsertShelvedCandidates writes a window's candidates keyed on
// (agent_id, session_key, seq, kind), in one transaction. Re-extraction
// of the same window refreshes last_seen_at instead of inserting a
// duplicate; an already resolved row is left untouched.
func (d *DBStore) UpsertShelvedCandidates(ctx context.Context, candidates []ShelvedCandidate) error {
	if len(candidates) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range candidates {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO shelved_candidates
			(agent_id, kind, status, topic, context, reason, session_key, seq, since,
			 last_seen_at, source, extractor, action_kind, action_ref, action_prompt,
			 created_at, updated_at)
			VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)
			ON CONFLICT(agent_id, session_key, seq, kind) DO UPDATE SET
				topic=excluded.topic, context=excluded.context, reason=excluded.reason,
				last_seen_at=excluded.last_seen_at, extractor=excluded.extractor,
				action_prompt=excluded.action_prompt, updated_at=CURRENT_TIMESTAMP
			WHERE shelved_candidates.status='open'`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9),
			d.ph(10), d.ph(11), d.ph(12), d.ph(13), d.ph(14), d.ph(15)),
			c.AgentID, c.Kind, "open", c.Topic, c.Context, c.Reason, c.SessionKey, c.Seq, c.Since,
			c.LastSeenAt, c.Source, c.Extractor, c.ActionKind, c.ActionRef, c.ActionPrompt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListOpenShelvedCandidates returns the agent's open candidates, oldest
// first — the list handed to the next summary extraction for settlement.
func (d *DBStore) ListOpenShelvedCandidates(ctx context.Context, agentID string, limit int) ([]ShelvedCandidate, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM shelved_candidates
		WHERE agent_id = %s AND status = 'open'
		ORDER BY since ASC, id ASC LIMIT %s`,
		shelvedCandidateColumns, d.ph(1), d.ph(2)), agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanShelvedCandidates(rows)
}

// HasShelvedCandidates reports whether the agent has any candidate row.
// The diary uses false as the "this agent predates the summary extractor"
// signal to fall back to the legacy transcript rescan.
func (d *DBStore) HasShelvedCandidates(ctx context.Context, agentID string) (bool, error) {
	var ok bool
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT EXISTS(SELECT 1 FROM shelved_candidates WHERE agent_id = %s)`, d.ph(1)), agentID).Scan(&ok)
	return ok, err
}

// ListShelvedCandidatesResolvedBetween returns the rows settled inside
// [from, to) — the diary's "picked back up that day" loop.
func (d *DBStore) ListShelvedCandidatesResolvedBetween(ctx context.Context, agentID string, from, to time.Time) ([]ShelvedCandidate, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM shelved_candidates
		WHERE agent_id = %s AND status = 'resolved'
		AND resolved_at >= %s AND resolved_at < %s
		ORDER BY resolved_at ASC, id ASC LIMIT %s`,
		shelvedCandidateColumns, d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
		agentID, from, to, 200)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanShelvedCandidates(rows)
}

// ShelvedExtractRetry is one summary window whose shelved section was
// malformed while the topic summary succeeded. Since the session's
// last_summarized_seq advanced, that window would never be re-read — this
// row makes the next summary for the session re-run shelved-only
// extraction over it.
type ShelvedExtractRetry struct {
	ID         int64
	AgentID    string
	SessionKey string
	SeqStart   int
	SeqEnd     int
	Attempts   int
	LastError  string
}

// EnqueueShelvedExtractRetry records (or refreshes) one failed window.
// Attempts are not incremented here — only a real retry does that.
func (d *DBStore) EnqueueShelvedExtractRetry(ctx context.Context, agentID, sessionKey string, seqStart, seqEnd int, lastErr string) error {
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO shelved_extract_retries
		(agent_id, session_key, seq_start, seq_end, last_error, created_at, updated_at)
		VALUES (%s,%s,%s,%s,%s,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)
		ON CONFLICT(agent_id, session_key, seq_start, seq_end) DO UPDATE SET
			last_error=excluded.last_error, updated_at=CURRENT_TIMESTAMP`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)),
		agentID, sessionKey, seqStart, seqEnd, lastErr)
	return err
}

// ListShelvedExtractRetriesByAgent returns the agent's queued windows
// across every session, oldest first — the drain sweep's input.
func (d *DBStore) ListShelvedExtractRetriesByAgent(ctx context.Context, agentID string, limit int) ([]ShelvedExtractRetry, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT id, agent_id, session_key, seq_start, seq_end, attempts, last_error
		FROM shelved_extract_retries
		WHERE agent_id = %s
		ORDER BY id ASC LIMIT %s`, d.ph(1), d.ph(2)), agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanShelvedExtractRetries(rows)
}

// ListShelvedExtractRetries returns the queued windows for one session,
// oldest first.
func (d *DBStore) ListShelvedExtractRetries(ctx context.Context, agentID, sessionKey string, limit int) ([]ShelvedExtractRetry, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT id, agent_id, session_key, seq_start, seq_end, attempts, last_error
		FROM shelved_extract_retries
		WHERE agent_id = %s AND session_key = %s
		ORDER BY id ASC LIMIT %s`, d.ph(1), d.ph(2), d.ph(3)),
		agentID, sessionKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanShelvedExtractRetries(rows)
}

func scanShelvedExtractRetries(rows *sql.Rows) ([]ShelvedExtractRetry, error) {
	var out []ShelvedExtractRetry
	for rows.Next() {
		var r ShelvedExtractRetry
		var lastErr sql.NullString
		if err := rows.Scan(&r.ID, &r.AgentID, &r.SessionKey, &r.SeqStart, &r.SeqEnd, &r.Attempts, &lastErr); err != nil {
			return nil, err
		}
		r.LastError = lastErr.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// BumpShelvedExtractRetry records one failed retry attempt.
func (d *DBStore) BumpShelvedExtractRetry(ctx context.Context, id int64, lastErr string) error {
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE shelved_extract_retries
		SET attempts = attempts + 1, last_error = %s, updated_at = CURRENT_TIMESTAMP
		WHERE id = %s`, d.ph(1), d.ph(2)), lastErr, id)
	return err
}

// DeleteShelvedExtractRetry drops one queue row (settled or abandoned).
func (d *DBStore) DeleteShelvedExtractRetry(ctx context.Context, id int64) error {
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM shelved_extract_retries WHERE id = %s`, d.ph(1)), id)
	return err
}

// MarkShelvedCandidatesResolved closes the given ids (only those still
// open). Returns how many rows changed.
func (d *DBStore) MarkShelvedCandidatesResolved(ctx context.Context, agentID string, ids []int64, resolution string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(ids)+2)
	args = append(args, time.Now(), resolution, agentID)
	ph := make([]string, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
		ph = append(ph, d.ph(len(args)))
	}
	res, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE shelved_candidates
		SET status='resolved', resolved_at=%s, resolution=%s, updated_at=CURRENT_TIMESTAMP
		WHERE agent_id=%s AND status='open' AND id IN (%s)`,
		d.ph(1), d.ph(2), d.ph(3), strings.Join(ph, ",")), args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// MarkShelvedCandidateStatus settles one row to resolved or dropped with
// the settlement provenance in resolution ("manual" from a dashboard
// action, "expired" from the retire sweep). source — who created the
// candidate — is left untouched. Only open rows change; the bool reports
// whether this call did the settling (false = already settled or unknown
// id), making repeats idempotent.
func (d *DBStore) MarkShelvedCandidateStatus(ctx context.Context, agentID string, id int64, status, resolution string) (bool, error) {
	res, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE shelved_candidates
		SET status=%s, resolved_at=%s, resolution=%s, updated_at=CURRENT_TIMESTAMP
		WHERE agent_id=%s AND id=%s AND status='open'`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)),
		status, time.Now(), resolution, agentID, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RetireStaleShelvedCandidates drops every open candidate first seen
// before the cutoff date (YYYY-MM-DD, string-compared) across all agents
// — silence past the window reads as tacit acceptance. Rows are kept for
// traceability with resolution='expired'; resolved_at doubles as the
// settlement time. Idempotent: aged-out rows are gone, so a re-run
// matches zero.
func (d *DBStore) RetireStaleShelvedCandidates(ctx context.Context, cutoff string) (int64, error) {
	res, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE shelved_candidates
		SET status='dropped', resolved_at=%s, resolution='expired', updated_at=CURRENT_TIMESTAMP
		WHERE status='open' AND since < %s`, d.ph(1), d.ph(2)), time.Now(), cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanShelvedCandidates(rows *sql.Rows) ([]ShelvedCandidate, error) {
	var out []ShelvedCandidate
	for rows.Next() {
		var c ShelvedCandidate
		var context, reason, actionKind, actionRef, actionPrompt, resolution sql.NullString
		var lastSeen, snooze, resolved sql.NullTime
		var mergedInto sql.NullInt64
		if err := rows.Scan(&c.ID, &c.AgentID, &c.Kind, &c.Status, &c.Topic,
			&context, &reason, &c.SessionKey, &c.Seq, &c.Since, &lastSeen,
			&c.Source, &c.Extractor, &actionKind, &actionRef, &actionPrompt,
			&snooze, &resolved, &resolution, &mergedInto, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.Context = context.String
		c.Reason = reason.String
		c.ActionKind = actionKind.String
		c.ActionRef = actionRef.String
		c.ActionPrompt = actionPrompt.String
		c.Resolution = resolution.String
		c.SnoozeUntil = snooze
		c.ResolvedAt = resolved
		if mergedInto.Valid {
			c.MergedInto = mergedInto.Int64
		}
		if lastSeen.Valid {
			c.LastSeenAt = lastSeen.Time
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
