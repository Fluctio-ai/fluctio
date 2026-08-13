package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// WorkflowScheduleRow is one scheduled workflow fire (spec decision 16). Times
// are RFC3339 UTC strings, matching workflow_runs. OwnerUserID is the agent's
// owner (so the scheduler can resolve the agent at fire time); the run itself
// is recorded with owner="system".
type WorkflowScheduleRow struct {
	ID          string
	AgentID     string
	WorkflowID  string
	OwnerUserID string
	CronExpr    string
	Input       map[string]any
	Enabled     bool
	LastRun     string
	NextRun     string
	CreatedAt   string
}

// CreateWorkflowSchedule inserts a new schedule. ID + NextRun must be set by
// the caller; CreatedAt defaults to now when empty.
func (d *DBStore) CreateWorkflowSchedule(ctx context.Context, s WorkflowScheduleRow) error {
	input, err := json.Marshal(s.Input)
	if err != nil {
		return fmt.Errorf("marshal schedule input: %w", err)
	}
	if s.CreatedAt == "" {
		s.CreatedAt = nowRFC3339()
	}
	en := 0
	if s.Enabled {
		en = 1
	}
	_, err = d.db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO workflow_schedules (id, agent_id, workflow_id, owner_user_id, cron_expr, input_json, enabled, next_run, created_at)
		 VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9)),
		s.ID, s.AgentID, s.WorkflowID, s.OwnerUserID, s.CronExpr, string(input), en, s.NextRun, s.CreatedAt)
	return err
}

// ListWorkflowSchedules returns every schedule for one agent (any enable state).
func (d *DBStore) ListWorkflowSchedules(ctx context.Context, agentID string) ([]WorkflowScheduleRow, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT id, agent_id, workflow_id, owner_user_id, cron_expr, input_json, enabled, last_run, next_run, created_at
		 FROM workflow_schedules WHERE agent_id = %s ORDER BY created_at ASC`, d.ph(1)), agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWorkflowSchedules(rows)
}

// GetDueWorkflowSchedules returns enabled schedules due at or before now.
func (d *DBStore) GetDueWorkflowSchedules(ctx context.Context, now time.Time) ([]WorkflowScheduleRow, error) {
	nowStr := now.UTC().Format(time.RFC3339)
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT id, agent_id, workflow_id, owner_user_id, cron_expr, input_json, enabled, last_run, next_run, created_at
		 FROM workflow_schedules WHERE enabled = 1 AND next_run <= %s ORDER BY next_run ASC`, d.ph(1)), nowStr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWorkflowSchedules(rows)
}

// LockWorkflowSchedule claims a schedule for this instance (advisory lock with
// a 5-minute TTL so a crashed instance's stale lock eventually releases).
func (d *DBStore) LockWorkflowSchedule(ctx context.Context, id, instanceID string) (bool, error) {
	now := nowRFC3339()
	fiveMinAgo := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	res, err := d.db.ExecContext(ctx, fmt.Sprintf(
		`UPDATE workflow_schedules SET locked_by = %s, locked_at = %s
		 WHERE id = %s AND (locked_by IS NULL OR locked_at < %s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4)), instanceID, now, id, fiveMinAgo)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UpdateWorkflowScheduleRun records a fire: lastRun set to now, nextRun set to
// the next occurrence, and the lock cleared.
func (d *DBStore) UpdateWorkflowScheduleRun(ctx context.Context, id, lastRun, nextRun string) error {
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(
		`UPDATE workflow_schedules SET last_run = %s, next_run = %s, locked_by = NULL, locked_at = NULL WHERE id = %s`,
		d.ph(1), d.ph(2), d.ph(3)), lastRun, nextRun, id)
	return err
}

// SetWorkflowScheduleEnabled toggles a schedule without deleting it.
func (d *DBStore) SetWorkflowScheduleEnabled(ctx context.Context, id string, enabled bool) error {
	en := 0
	if enabled {
		en = 1
	}
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(
		`UPDATE workflow_schedules SET enabled = %s WHERE id = %s`, d.ph(1), d.ph(2)), en, id)
	return err
}

// DeleteWorkflowSchedule removes a schedule.
func (d *DBStore) DeleteWorkflowSchedule(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM workflow_schedules WHERE id = %s`, d.ph(1)), id)
	return err
}

func scanWorkflowSchedules(rows *sql.Rows) ([]WorkflowScheduleRow, error) {
	var out []WorkflowScheduleRow
	for rows.Next() {
		var s WorkflowScheduleRow
		var inputJSON string
		var enabled int
		var lastRun sql.NullString
		if err := rows.Scan(&s.ID, &s.AgentID, &s.WorkflowID, &s.OwnerUserID, &s.CronExpr, &inputJSON, &enabled, &lastRun, &s.NextRun, &s.CreatedAt); err != nil {
			return nil, err
		}
		s.LastRun = lastRun.String
		s.Enabled = enabled != 0
		if inputJSON != "" {
			_ = json.Unmarshal([]byte(inputJSON), &s.Input)
		}
		if s.Input == nil {
			s.Input = map[string]any{}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
