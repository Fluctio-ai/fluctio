package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// nowRFC3339 is the shared timestamp format for the workflow tables.
func nowRFC3339() string {
	t := time.Now().UTC()
	return t.Format(time.RFC3339)
}

// migrateWorkflowTables creates the workflow run + node-output tables.
// Idempotent — every statement is CREATE ... IF NOT EXISTS, safe on every boot.
//
// Schema follows spec decision 10:
//   - workflow_runs is the run-level record (id / def id+version / input /
//     status / session_id? / owner? / start+finish time).
//   - workflow_node_outputs holds one row per node execution attempt, so a
//     resumed run can append a new attempt for the same (run, node) without
//     overwriting the failure scene (spec decision 15). Tracer bullet always
//     writes attempt=1; the column exists so later tickets don't reshape the
//     table.
func (d *DBStore) migrateWorkflowTables(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS workflow_runs (
			id           TEXT PRIMARY KEY,
			def_id       TEXT NOT NULL,
			version      INTEGER NOT NULL,
			input_json   TEXT NOT NULL DEFAULT '{}',
			status       TEXT NOT NULL DEFAULT 'running',
			session_id   TEXT NOT NULL DEFAULT '',
			owner        TEXT NOT NULL DEFAULT '',
			error        TEXT NOT NULL DEFAULT '',
			started_at   TEXT NOT NULL,
			finished_at  TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_workflow_runs_def ON workflow_runs (def_id)`,
		`CREATE TABLE IF NOT EXISTS workflow_node_outputs (
			run_id       TEXT NOT NULL,
			node_id      TEXT NOT NULL,
			attempt      INTEGER NOT NULL DEFAULT 1,
			status       TEXT NOT NULL,
			output_json  TEXT NOT NULL DEFAULT '{}',
			error        TEXT NOT NULL DEFAULT '',
			side_effect  TEXT NOT NULL DEFAULT '',
			created_at   TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_workflow_node_outputs_run ON workflow_node_outputs (run_id, node_id, attempt)`,
	}
	for _, s := range stmts {
		if _, err := d.db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("migrate workflow tables: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// WorkflowNodeOutputRow is one persisted node-execution outcome for a run.
type WorkflowNodeOutputRow struct {
	NodeID  string
	Attempt int
	Status  string
	Output  map[string]any
	Error   string
}

// CreateWorkflowRun inserts a fresh run record in 'running' state.
func (d *DBStore) CreateWorkflowRun(ctx context.Context, id, defID string, version int, input map[string]any, sessionID, owner string) error {
	b, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("marshal workflow input: %w", err)
	}
	_, err = d.db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO workflow_runs (id, def_id, version, input_json, status, session_id, owner, started_at)
		 VALUES (%s, %s, %s, %s, 'running', %s, %s, %s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)),
		id, defID, version, string(b), sessionID, owner, nowRFC3339())
	return err
}

// FinalizeWorkflowRun sets the run's terminal status + error + finish time.
func (d *DBStore) FinalizeWorkflowRun(ctx context.Context, id, status, errMsg string) error {
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(
		`UPDATE workflow_runs SET status = %s, error = %s, finished_at = %s WHERE id = %s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
		status, errMsg, nowRFC3339(), id)
	return err
}

// AppendWorkflowNodeOutput appends one attempt for a node in a run. It never
// overwrites a prior attempt — resume (spec decision 15) adds a new row with a
// higher attempt number. Tracer bullet always uses attempt = 1.
func (d *DBStore) AppendWorkflowNodeOutput(ctx context.Context, runID, nodeID string, attempt int, status string, output map[string]any, errMsg string) error {
	b, err := json.Marshal(output)
	if err != nil {
		return fmt.Errorf("marshal workflow node output: %w", err)
	}
	_, err = d.db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO workflow_node_outputs (run_id, node_id, attempt, status, output_json, error, created_at)
		 VALUES (%s, %s, %s, %s, %s, %s, %s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)),
		runID, nodeID, attempt, status, string(b), errMsg, nowRFC3339())
	return err
}

// ListWorkflowNodeOutputs returns every persisted node attempt for a run, in
// (node, attempt) order. Callers that want one row per node pick the highest
// attempt; tracer bullet writes a single attempt so every node appears once.
func (d *DBStore) ListWorkflowNodeOutputs(ctx context.Context, runID string) ([]WorkflowNodeOutputRow, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT node_id, attempt, status, output_json, error
		 FROM workflow_node_outputs WHERE run_id = %s
		 ORDER BY node_id ASC, attempt ASC`, d.ph(1)), runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkflowNodeOutputRow
	for rows.Next() {
		var r WorkflowNodeOutputRow
		var outputJSON string
		if err := rows.Scan(&r.NodeID, &r.Attempt, &r.Status, &outputJSON, &r.Error); err != nil {
			return nil, err
		}
		if outputJSON != "" {
			_ = json.Unmarshal([]byte(outputJSON), &r.Output)
		}
		if r.Output == nil {
			r.Output = map[string]any{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkRunRunning flips a run back to "running" and clears its finish time,
// used at resume start (spec decision 15: failed→running while the resume is
// in flight).
func (d *DBStore) MarkRunRunning(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(
		`UPDATE workflow_runs SET status = 'running', finished_at = NULL WHERE id = %s`,
		d.ph(1)), id)
	return err
}

// GetWorkflowRun returns the run-level record, or ErrNotFound if it doesn't exist.
func (d *DBStore) GetWorkflowRun(ctx context.Context, id string) (status, errMsg string, err error) {
	err = d.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT status, error FROM workflow_runs WHERE id = %s`, d.ph(1)), id).Scan(&status, &errMsg)
	if err == sql.ErrNoRows {
		return "", "", ErrNotFound
	}
	return status, errMsg, err
}

// PruneWorkflowRuns deletes finished runs past their retention window, cascading
// to workflow_node_outputs (spec decision 11): succeeded runs older than
// successBefore, and runs in a failure terminal state (failed /
// needs_intervention) older than failedBefore. A zero cutoff for either state
// disables pruning for that state. 'running' runs (finished_at IS NULL) are
// never matched. Bounded batches keep a large backlog from holding the write
// lock in one transaction. Returns the total runs deleted. Deliberately not on
// the store.Store interface — the retention sweep reaches it via a *DBStore
// type assertion, so no-op test stores don't stub it.
func (d *DBStore) PruneWorkflowRuns(ctx context.Context, successBefore, failedBefore time.Time, batchSize int) (int64, error) {
	if batchSize <= 0 {
		batchSize = 1000
	}
	var total int64
	for {
		ids, err := d.selectExpiredRunIDs(ctx, successBefore, failedBefore, batchSize)
		if err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}
		if err := d.deleteRunsByID(ctx, ids); err != nil {
			return total, err
		}
		total += int64(len(ids))
		if len(ids) < batchSize {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
	}
}

// DeleteWorkflowRun removes one run (by id) and its node outputs — the manual
// "delete this run" entry point. No-op (returns nil) if the run doesn't exist.
func (d *DBStore) DeleteWorkflowRun(ctx context.Context, runID string) error {
	return d.deleteRunsByID(ctx, []string{runID})
}

// DeleteWorkflowRunsBy removes runs matching the given filter (any of status /
// olderThan / owner may be zero-value to skip that clause), cascading to node
// outputs, in bounded batches. olderThan matches only finished runs. A fully
// empty filter is a no-op (refuses to delete everything). Returns the total
// runs deleted.
func (d *DBStore) DeleteWorkflowRunsBy(ctx context.Context, status string, olderThan time.Time, owner string, batchSize int) (int64, error) {
	if batchSize <= 0 {
		batchSize = 1000
	}
	var total int64
	for {
		ids, err := d.selectRunIDsByFilter(ctx, status, olderThan, owner, batchSize)
		if err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}
		if err := d.deleteRunsByID(ctx, ids); err != nil {
			return total, err
		}
		total += int64(len(ids))
		if len(ids) < batchSize {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
	}
}

// selectExpiredRunIDs returns up to limit run ids whose finished_at is before
// the applicable retention cutoff. finished_at is RFC3339 (UTC); cutoffs are
// formatted the same way so the string compare matches how rows are stored.
// NULL finished_at (running) never compares less than a cutoff, so live runs
// are excluded.
func (d *DBStore) selectExpiredRunIDs(ctx context.Context, successBefore, failedBefore time.Time, limit int) ([]string, error) {
	var clauses []string
	var args []any
	if !successBefore.IsZero() {
		clauses = append(clauses, fmt.Sprintf("(status = 'succeeded' AND finished_at < %s)", d.ph(len(args)+1)))
		args = append(args, successBefore.UTC().Format(time.RFC3339))
	}
	if !failedBefore.IsZero() {
		clauses = append(clauses, fmt.Sprintf("(status IN ('failed','needs_intervention') AND finished_at < %s)", d.ph(len(args)+1)))
		args = append(args, failedBefore.UTC().Format(time.RFC3339))
	}
	if len(clauses) == 0 {
		return nil, nil
	}
	q := fmt.Sprintf(`SELECT id FROM workflow_runs WHERE %s ORDER BY id LIMIT %s`,
		strings.Join(clauses, " OR "), d.ph(len(args)+1))
	args = append(args, limit)
	return d.queryRunIDs(ctx, q, args...)
}

// selectRunIDsByFilter returns up to limit run ids matching the given manual
// cleanup filter. Returns nil for an empty filter (no clause), so the caller
// can't accidentally wipe the table.
func (d *DBStore) selectRunIDsByFilter(ctx context.Context, status string, olderThan time.Time, owner string, limit int) ([]string, error) {
	var clauses []string
	var args []any
	if status != "" {
		clauses = append(clauses, fmt.Sprintf("status = %s", d.ph(len(args)+1)))
		args = append(args, status)
	}
	if !olderThan.IsZero() {
		clauses = append(clauses, fmt.Sprintf("finished_at < %s", d.ph(len(args)+1)))
		args = append(args, olderThan.UTC().Format(time.RFC3339))
	}
	if owner != "" {
		clauses = append(clauses, fmt.Sprintf("owner = %s", d.ph(len(args)+1)))
		args = append(args, owner)
	}
	if len(clauses) == 0 {
		return nil, nil
	}
	q := fmt.Sprintf(`SELECT id FROM workflow_runs WHERE %s ORDER BY id LIMIT %s`,
		strings.Join(clauses, " AND "), d.ph(len(args)+1))
	args = append(args, limit)
	return d.queryRunIDs(ctx, q, args...)
}

func (d *DBStore) queryRunIDs(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// deleteRunsByID removes the named runs and their node outputs in one
// transaction so a partial delete can't orphan either side.
func (d *DBStore) deleteRunsByID(ctx context.Context, ids []string) error {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = d.ph(i + 1)
		args[i] = id
	}
	inList := strings.Join(placeholders, ", ")
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM workflow_node_outputs WHERE run_id IN (%s)`, inList), args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM workflow_runs WHERE id IN (%s)`, inList), args...); err != nil {
		return err
	}
	return tx.Commit()
}

// WorkflowRunRow is the run-level record for history listings / detail views.
type WorkflowRunRow struct {
	ID         string
	DefID      string
	Version    int
	Status     string
	SessionID  string
	Owner      string
	Error      string
	StartedAt  string
	FinishedAt string
}

// ListWorkflowRuns lists runs for one workflow def, most-recent-first, capped
// at limit (limit<=0 → 50). Backs the run-history UI (ticket 08).
func (d *DBStore) ListWorkflowRuns(ctx context.Context, defID string, limit int) ([]WorkflowRunRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT id, def_id, version, status, session_id, owner, error, started_at, finished_at
		 FROM workflow_runs WHERE def_id = %s
		 ORDER BY started_at DESC LIMIT %s`, d.ph(1), d.ph(2)), defID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkflowRunRow
	for rows.Next() {
		var r WorkflowRunRow
		if err := rows.Scan(&r.ID, &r.DefID, &r.Version, &r.Status, &r.SessionID, &r.Owner, &r.Error, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetWorkflowRunRow returns the full run-level record, or ErrNotFound.
func (d *DBStore) GetWorkflowRunRow(ctx context.Context, runID string) (WorkflowRunRow, error) {
	var r WorkflowRunRow
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT id, def_id, version, status, session_id, owner, error, started_at, finished_at
		 FROM workflow_runs WHERE id = %s`, d.ph(1)), runID).Scan(
		&r.ID, &r.DefID, &r.Version, &r.Status, &r.SessionID, &r.Owner, &r.Error, &r.StartedAt, &r.FinishedAt)
	if err == sql.ErrNoRows {
		return r, ErrNotFound
	}
	return r, err
}
