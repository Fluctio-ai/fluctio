package workflow_test

import (
	"context"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/workflow"
)

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

// AC 2/3/4 — RunWorkflow executes via a fresh runner built from the injected
// leaf callers, and the run's ownership (owner/session, spec decision 14) lands
// in workflow_runs. AC 5 (schema rejection) is exercised through runner.Run's
// Validate precondition, already covered by validate_test.go; an unknown id
// errors before any runner is built (llm/tool unused).
func TestService_RunWorkflow(t *testing.T) {
	def := mustParse(t, linearYAML)
	defs := map[string]*workflow.Definition{def.ID: def}

	st := newTestStore(t)
	tools := &fakeTools{out: map[string]string{"get_data": `{"summary":"s"}`}}
	llm := &fakeLLM{resp: `{"r":"ok"}`}
	svc := workflow.NewService(defs, st)

	res, err := svc.RunWorkflow(context.Background(), def.ID, map[string]any{"topic": "cats"}, "user-1", "sess-1", llm, tools, nil)
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

	if _, err := svc.RunWorkflow(context.Background(), "nope", nil, "", "", nil, nil, nil); err == nil {
		t.Error("expected error for unknown workflow id")
	}
}
