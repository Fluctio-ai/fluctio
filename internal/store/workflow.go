package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
