package workflow_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/workflow"
)

// formYAML is the M6 shape: a form node parks the run; a downstream tool node
// reads both the form values (${collect.email}) and the original run input
// (${input.topic}) — the latter only survives resume because the runner
// replays input from the run row.
const formYAML = `
version: 1
input:
  schema:
    type: object
    properties:
      topic: {type: string}
nodes:
  - name: collect
    kind: form
    input:
      properties:
        email: {type: string}
        age: {type: integer}
      required: [email]
  - name: after
    kind: tool
    tool: t1
    input:
      e: "${collect.email}"
      topic: "${input.topic}"
edges:
  - {from: collect, to: after}
`

// M6 AC — a manual run hitting a form node parks on it: status=waiting, the
// result carries the pending form (node + schema), and the run row records
// pending_form_node for clients that reopen the run later.
func TestRunner_FormWaits(t *testing.T) {
	def := mustParse(t, formYAML)
	tools := &fakeTools{out: map[string]string{"t1": `{"ok":1}`}}
	st := newTestStore(t)
	r := workflow.NewRunner(&fakeLLM{}, tools, st)

	res, err := r.Run(context.Background(), def, map[string]any{"topic": "cats"}, workflow.WithOwner("u1"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != workflow.StatusWaiting {
		t.Fatalf("status %s, want waiting", res.Status)
	}
	if res.PendingForm == nil || res.PendingForm.Node != "collect" {
		t.Fatalf("pending form %+v, want node collect", res.PendingForm)
	}
	if props, _ := res.PendingForm.Schema["properties"].(map[string]any); len(props) != 2 {
		t.Fatalf("pending form schema properties = %v, want 2 fields", res.PendingForm.Schema)
	}
	if calls(tools, "t1") != 0 {
		t.Fatalf("downstream tool ran before the form was answered")
	}
	row, err := st.GetWorkflowRunRow(context.Background(), res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != string(workflow.StatusWaiting) || row.PendingFormNode != "collect" {
		t.Fatalf("run row status=%s pending=%q, want waiting/collect", row.Status, row.PendingFormNode)
	}
	if row.PendingFormSchema == "" {
		t.Fatalf("run row pending_form_schema empty")
	}
	// The form node itself is persisted as a waiting attempt (attempt 1).
	snap := res.Snapshot["collect"]
	if snap.Status != workflow.StatusWaiting || snap.Attempt != 1 {
		t.Fatalf("form node snapshot %+v, want waiting attempt 1", snap)
	}
}

// M6 AC — resume with values: they are validated, become the form node's
// output (downstream ${collect.email} resolves), the original run input is
// replayed from the run row (${input.topic} still resolves), and the run
// finishes succeeded with the same run_id.
func TestRunner_FormResume(t *testing.T) {
	def := mustParse(t, formYAML)
	tools := &fakeTools{out: map[string]string{"t1": `{"ok":1}`}}
	st := newTestStore(t)
	r := workflow.NewRunner(&fakeLLM{}, tools, st)

	res1, err := r.Run(context.Background(), def, map[string]any{"topic": "cats"}, workflow.WithOwner("u1"))
	if err != nil || res1.Status != workflow.StatusWaiting {
		t.Fatalf("run1: err=%v status=%s, want waiting", err, res1.Status)
	}

	res2, err := r.Run(context.Background(), def, nil,
		workflow.WithResume(res1.RunID),
		workflow.WithFormValues("collect", map[string]any{"email": "a@b.c", "age": 30}),
		workflow.WithOwner("u1"))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res2.Status != workflow.StatusSucceeded {
		t.Fatalf("resume status %s, want succeeded (err=%+v)", res2.Status, res2.Error)
	}
	if res2.RunID != res1.RunID {
		t.Fatalf("resume run_id %q != %q", res2.RunID, res1.RunID)
	}
	if calls(tools, "t1") != 1 {
		t.Fatalf("t1 calls = %d, want 1", calls(tools, "t1"))
	}
	got := tools.got["t1"][0]
	if got["e"] != "a@b.c" || got["topic"] != "cats" {
		t.Fatalf("t1 args = %v, want form value + replayed input", got)
	}
	// The waiting row is superseded by a succeeded attempt 2.
	snap := res2.Snapshot["collect"]
	if snap.Status != workflow.StatusSucceeded || snap.Attempt != 2 {
		t.Fatalf("form node snapshot %+v, want succeeded attempt 2", snap)
	}
	row, err := st.GetWorkflowRunRow(context.Background(), res2.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != string(workflow.StatusSucceeded) || row.PendingFormNode != "" {
		t.Fatalf("run row status=%s pending=%q, want succeeded/empty", row.Status, row.PendingFormNode)
	}
}

// M6 AC — resume with values violating the schema (missing required field)
// fails the run at the form node with a locatable message; the run can be
// resumed again with valid values.
func TestRunner_FormResume_MissingRequired(t *testing.T) {
	def := mustParse(t, formYAML)
	tools := &fakeTools{out: map[string]string{"t1": `{"ok":1}`}}
	st := newTestStore(t)
	r := workflow.NewRunner(&fakeLLM{}, tools, st)

	res1, err := r.Run(context.Background(), def, nil, workflow.WithOwner("u1"))
	if err != nil || res1.Status != workflow.StatusWaiting {
		t.Fatalf("run1: err=%v status=%s, want waiting", err, res1.Status)
	}
	res2, err := r.Run(context.Background(), def, nil,
		workflow.WithResume(res1.RunID),
		workflow.WithFormValues("collect", map[string]any{"age": 30}),
		workflow.WithOwner("u1"))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res2.Status != workflow.StatusFailed {
		t.Fatalf("resume status %s, want failed", res2.Status)
	}
	if res2.Error == nil || !strings.Contains(res2.Error.Message, "email") {
		t.Fatalf("error %+v should name the missing field", res2.Error)
	}
	if calls(tools, "t1") != 0 {
		t.Fatalf("downstream ran despite invalid form values")
	}
	// A corrected resume still works from the failed state.
	res3, err := r.Run(context.Background(), def, nil,
		workflow.WithResume(res1.RunID),
		workflow.WithFormValues("collect", map[string]any{"email": "x@y.z"}),
		workflow.WithOwner("u1"))
	if err != nil || res3.Status != workflow.StatusSucceeded {
		t.Fatalf("retry resume: err=%v status=%s, want succeeded", err, res3.Status)
	}
}

// M6 AC — trigger-source gate: a cron-style run (owner="system") and an
// LLM-triggered run (session != "") cannot wait on a form — the run fails
// with a clear message instead of parking.
func TestRunner_FormTriggerGate(t *testing.T) {
	def := mustParse(t, formYAML)
	tools := &fakeTools{out: map[string]string{"t1": `{"ok":1}`}}
	st := newTestStore(t)
	r := workflow.NewRunner(&fakeLLM{}, tools, st)

	res, err := r.Run(context.Background(), def, nil, workflow.WithOwner("system"))
	if err != nil {
		t.Fatalf("cron run: %v", err)
	}
	if res.Status != workflow.StatusFailed || res.Error == nil ||
		!strings.Contains(res.Error.Message, "manual (web) trigger") {
		t.Fatalf("cron run status=%s err=%+v, want failed with trigger message", res.Status, res.Error)
	}

	st2 := newTestStore(t)
	r2 := workflow.NewRunner(&fakeLLM{}, tools, st2)
	res2, err := r2.Run(context.Background(), def, nil, workflow.WithOwner("u1"), workflow.WithSession("sess1"))
	if err != nil {
		t.Fatalf("llm run: %v", err)
	}
	if res2.Status != workflow.StatusFailed || res2.Error == nil ||
		!strings.Contains(res2.Error.Message, "manual (web) trigger") {
		t.Fatalf("llm run status=%s err=%+v, want failed with trigger message", res2.Status, res2.Error)
	}
}

// M6 AC — Validate rejects a form node without a field schema: a waiting form
// that renders nothing is an unanswerable prompt, caught at design time.
func TestValidate_FormRequiresSchema(t *testing.T) {
	def := mustParse(t, `
version: 1
nodes:
  - {name: collect, kind: form}
  - {name: after, kind: condition}
edges:
  - {from: collect, to: after}
`)
	if verr := workflow.Validate(def, nil); verr == nil || !strings.Contains(verr.Error(), "properties") {
		t.Fatalf("validate err = %v, want form schema requirement", verr)
	}
}
