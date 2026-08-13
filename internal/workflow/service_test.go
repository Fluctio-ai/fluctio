package workflow_test

import (
	"context"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/workflow"
)

// AC 1 core — the per-agent ACL: a workflow is visible to its whitelisted
// agents only; with no whitelist it is default-private (visible to no one).
func TestService_VisibleTo(t *testing.T) {
	svc := workflow.NewService(nil, nil)
	whitelisted := &workflow.Definition{Agents: []string{"a1", "a2"}}
	if !svc.VisibleTo(whitelisted, "a1") {
		t.Error("agent in whitelist should be visible")
	}
	if svc.VisibleTo(whitelisted, "a3") {
		t.Error("agent not in whitelist should be invisible")
	}
	private := &workflow.Definition{} // no agents → default-private
	if svc.VisibleTo(private, "anyone") {
		t.Error("default-private workflow should be invisible to everyone")
	}
}

// Spec decision 7 — the tool schema is generated from the workflow's input
// schema; a schema-less workflow still gets a bare-object tool schema.
func TestService_ToolSchema(t *testing.T) {
	svc := workflow.NewService(nil, nil)
	withSchema := &workflow.Definition{Input: workflow.InputSpec{Schema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"x": map[string]any{"type": "string"}},
	}}}
	if got := svc.ToolSchema(withSchema); got["type"] != "object" {
		t.Errorf("tool schema = %#v, want input schema verbatim", got)
	}
	noSchema := &workflow.Definition{}
	if got := svc.ToolSchema(noSchema); got["type"] != "object" {
		t.Errorf("default tool schema = %#v, want {type:object}", got)
	}
}

// AC 2/3/4 — RunWorkflow executes via the runner and the run's ownership
// (owner/session, spec decision 14) lands in workflow_runs. AC 5 (schema
// rejection) is exercised through runner.Run's Validate precondition, already
// covered by validate_test.go; an unknown id errors.
func TestService_RunWorkflow(t *testing.T) {
	def := mustParse(t, linearYAML)
	defs := map[string]*workflow.Definition{def.ID: def}

	st := newTestStore(t)
	tools := &fakeTools{out: map[string]string{"get_data": `{"summary":"s"}`}}
	runner := workflow.NewRunner(&fakeLLM{resp: `{"r":"ok"}`}, tools, st)
	svc := workflow.NewService(defs, runner)

	res, err := svc.RunWorkflow(context.Background(), def.ID, map[string]any{"topic": "cats"}, "user-1", "sess-1")
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}
	if res.Status != workflow.StatusSucceeded {
		t.Fatalf("status %s, want succeeded", res.Status)
	}

	// ownership landed in workflow_runs
	var owner, session string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT owner, session_id FROM workflow_runs WHERE id = ?`, res.RunID).Scan(&owner, &session); err != nil {
		t.Fatalf("query run: %v", err)
	}
	if owner != "user-1" || session != "sess-1" {
		t.Errorf("run owner=%q session=%q, want user-1/sess-1", owner, session)
	}

	if _, err := svc.RunWorkflow(context.Background(), "nope", nil, "", ""); err == nil {
		t.Error("expected error for unknown workflow id")
	}
}
