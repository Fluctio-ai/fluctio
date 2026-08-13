package workflow_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/workflow"
)

// AC 1, 2, 4(idempotent), 5 — a 3-node workflow fails at n3; resuming re-runs
// ONLY n3 (n1/n2 are not re-invoked), appends a second n3 attempt, and keeps
// the same run_id. The runner reloads the resume snapshot from the DB itself
// (decision 10) — the test only hands it the run_id.
func TestRunner_Resume(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - {name: n1, kind: tool, tool: t1, side_effect: idempotent}
  - {name: n2, kind: tool, tool: t2, side_effect: idempotent}
  - {name: n3, kind: tool, tool: t3, side_effect: idempotent}
edges:
  - {from: n1, to: n2}
  - {from: n2, to: n3}
`)
	tools := &fakeTools{
		out: map[string]string{"t1": `{"a":1}`, "t2": `{"b":2}`, "t3": `{"c":3}`},
		err: map[string]error{"t3": errors.New("boom")},
	}
	st := newTestStore(t)
	r := workflow.NewRunner(&fakeLLM{}, tools, st)

	// First run — n3 fails, n1/n2 succeed.
	res1, err := r.Run(context.Background(), def, map[string]any{})
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if res1.Status != workflow.StatusFailed {
		t.Fatalf("run1 status %s, want failed", res1.Status)
	}
	if res1.Snapshot["n1"].Status != workflow.StatusSucceeded || res1.Snapshot["n3"].Status != workflow.StatusFailed {
		t.Fatalf("run1 snapshot wrong: %+v", res1.Snapshot)
	}
	runID := res1.RunID
	if calls(tools, "t1") != 1 || calls(tools, "t2") != 1 || calls(tools, "t3") != 1 {
		t.Fatalf("after run1 calls t1=%d t2=%d t3=%d, want each 1", calls(tools, "t1"), calls(tools, "t2"), calls(tools, "t3"))
	}

	// Fix t3, then resume.
	delete(tools.err, "t3")
	res2, err := r.Run(context.Background(), def, map[string]any{}, workflow.WithResume(runID))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res2.Status != workflow.StatusSucceeded {
		t.Errorf("resume status %s, want succeeded", res2.Status)
	}
	// AC5 — run_id unchanged.
	if res2.RunID != runID {
		t.Errorf("resume run_id %q != original %q", res2.RunID, runID)
	}
	// AC1 — n1/n2 NOT re-invoked; n3 re-invoked once.
	if calls(tools, "t1") != 1 || calls(tools, "t2") != 1 {
		t.Errorf("resume re-ran completed nodes: t1=%d t2=%d, want 1 each", calls(tools, "t1"), calls(tools, "t2"))
	}
	if calls(tools, "t3") != 2 {
		t.Errorf("resume t3 calls = %d, want 2 (original + rerun)", calls(tools, "t3"))
	}
	// AC2 — n3 has two attempts with the right values: attempt 1 failed
	// (failure scene preserved), attempt 2 succeeded (the new attempt).
	rows, err := st.ListWorkflowNodeOutputs(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	byAttempt := map[int]string{}
	for _, rw := range rows {
		if rw.NodeID == "n3" {
			byAttempt[rw.Attempt] = rw.Status
		}
	}
	if len(byAttempt) != 2 {
		t.Fatalf("n3 persisted attempts = %d, want 2", len(byAttempt))
	}
	if byAttempt[1] != string(workflow.StatusFailed) {
		t.Errorf("n3 attempt 1 = %s, want failed (failure scene preserved)", byAttempt[1])
	}
	if byAttempt[2] != string(workflow.StatusSucceeded) {
		t.Errorf("n3 attempt 2 = %s, want succeeded (new attempt)", byAttempt[2])
	}
}

// AC 3 — a non-idempotent node that failed must NOT be auto-rerun on resume;
// the run is marked needs-intervention instead. (AC4's pure/idempotent side is
// the previous test; pure is the default and falls through the same "rerun"
// branch since only non-idempotent is refused.)
func TestRunner_Resume_NonIdempotent(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - {name: n1, kind: tool, tool: t1, side_effect: idempotent}
  - {name: n2, kind: tool, tool: t2, side_effect: non-idempotent}
edges:
  - {from: n1, to: n2}
`)
	tools := &fakeTools{
		out: map[string]string{"t1": `{}`, "t2": `{}`},
		err: map[string]error{"t2": errors.New("boom")},
	}
	st := newTestStore(t)
	r := workflow.NewRunner(&fakeLLM{}, tools, st)

	res1, err := r.Run(context.Background(), def, nil)
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if res1.Status != workflow.StatusFailed {
		t.Fatalf("run1 status %s, want failed at n2", res1.Status)
	}
	runID := res1.RunID

	// Even with t2 "fixed", resume must refuse to rerun the non-idempotent n2.
	delete(tools.err, "t2")
	res2, err := r.Run(context.Background(), def, nil, workflow.WithResume(runID))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res2.Status != workflow.StatusNeedsIntervention {
		t.Errorf("resume status %s, want needs_intervention", res2.Status)
	}
	if calls(tools, "t2") != 1 {
		t.Errorf("non-idempotent n2 re-invoked %d times, want 1 (no auto-rerun)", calls(tools, "t2"))
	}
	if res2.Error == nil || !strings.Contains(res2.Error.Message, "non-idempotent") {
		t.Errorf("error %+v should mention non-idempotent", res2.Error)
	}
}

func calls(f *fakeTools, name string) int { return len(f.got[name]) }
