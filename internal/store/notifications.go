package store

import (
	"context"
	"fmt"
	"time"
)

// NotificationRecord is one entry in the user-facing notification queue.
// The proactive sweeps (due todos, cards digest, shelved ledger, diary
// blindspots) enqueue here in addition to their IM push; the Android app
// drains it via polling when backgrounded and via the SSE stream when in
// the foreground — the Gotify-style direct-delivery channel, no FCM.
type NotificationRecord struct {
	ID        int64     `json:"id"`
	UserID    string    `json:"-"`
	AgentID   string    `json:"agentId"`
	Kind      string    `json:"kind"` // todo_reminder | cards_digest | shelved_nudge | blindspot
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	DeepLink  string    `json:"deepLink"` // app-relative, e.g. /agents/<id>/knowledge/diary
	CreatedAt time.Time `json:"createdAt"`
}

// migrateNotifications creates the notifications queue. Separate from
// migrationSQL because the auto-increment id diverges: BIGSERIAL on
// Postgres, INTEGER PRIMARY KEY AUTOINCREMENT on SQLite (same shape as
// migrateConversationSummaries). Delivery is cursor-based row deletion —
// the client acks a watermark and everything up to it is deleted — so
// there is no acked_at column to keep dialect-compatible.
func (d *DBStore) migrateNotifications(ctx context.Context) error {
	idCol := "id INTEGER PRIMARY KEY AUTOINCREMENT"
	if d.dialect == "postgres" {
		idCol = "id BIGSERIAL PRIMARY KEY"
	}
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS notifications (
			%s,
			user_id TEXT NOT NULL,
			agent_id TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			body TEXT NOT NULL DEFAULT '',
			deep_link TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idCol),
		`CREATE INDEX IF NOT EXISTS idx_notifications_user ON notifications (user_id, id)`,
	}
	for _, s := range stmts {
		if _, err := d.db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("migrate notifications: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// SaveNotification enqueues one notification for userID and backfills the
// assigned row id. Callers fire-and-forget the error — a lost notification
// is a missed nudge, never a failed turn; every sweep already stamps its
// own dedup marker only after IM delivery succeeded.
func (d *DBStore) SaveNotification(ctx context.Context, rec *NotificationRecord) (int64, error) {
	if rec == nil || rec.UserID == "" {
		return 0, fmt.Errorf("store: notification requires user_id")
	}
	now := time.Now().UTC()
	if d.dialect == "postgres" {
		err := d.db.QueryRowContext(ctx,
			`INSERT INTO notifications (user_id, agent_id, kind, title, body, deep_link, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			rec.UserID, rec.AgentID, rec.Kind, rec.Title, rec.Body, rec.DeepLink, now).Scan(&rec.ID)
		if err != nil {
			return 0, err
		}
		return rec.ID, nil
	}
	res, err := d.db.ExecContext(ctx,
		`INSERT INTO notifications (user_id, agent_id, kind, title, body, deep_link, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rec.UserID, rec.AgentID, rec.Kind, rec.Title, rec.Body, rec.DeepLink, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	rec.ID = id
	return id, nil
}

// ListNotifications returns userID's pending notifications with id greater
// than sinceID, oldest first — the polling endpoint's page. limit is
// clamped to [1, 200] with a default of 50.
func (d *DBStore) ListNotifications(ctx context.Context, userID string, sinceID int64, limit int) ([]NotificationRecord, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, user_id, agent_id, kind, title, body, deep_link, created_at
			FROM notifications WHERE user_id = %s AND id > %s ORDER BY id ASC LIMIT %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		userID, sinceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NotificationRecord, 0)
	for rows.Next() {
		var rec NotificationRecord
		if err := rows.Scan(&rec.ID, &rec.UserID, &rec.AgentID, &rec.Kind, &rec.Title, &rec.Body, &rec.DeepLink, &rec.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// AckNotificationsTo deletes userID's notifications up to and including
// lastID — the cursor watermark the client has fully rendered/notified.
// Below-watermark deletion doubles as the retention story for acked rows.
func (d *DBStore) AckNotificationsTo(ctx context.Context, userID string, lastID int64) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM notifications WHERE user_id = %s AND id <= %s`, d.ph(1), d.ph(2)),
		userID, lastID)
	return err
}

// PruneNotifications deletes rows older than before regardless of ack
// state — the sweep-side retention so an unopened app can't grow the
// queue forever. Returns the deleted row count for the retention sweep's
// log line.
func (d *DBStore) PruneNotifications(ctx context.Context, before time.Time) (int64, error) {
	res, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM notifications WHERE created_at < %s`, d.ph(1)),
		before)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
