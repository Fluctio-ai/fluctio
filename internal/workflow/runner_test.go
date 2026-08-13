package workflow_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/workflow"
)

// --- fakes for the leaf capabilities (provider / tool registry) ---
// Per spec "不在新 seam 内测试：节点内部行为（provider/工具/sandbox 自有边界）",
// Seam A drives execution through these fakes; the real adapters wrap the
// provider / registry but are exercised only through integration elsewhere.

type fakeLLM struct {
	resp string
	err  error
	got  []string // prompts received
}

func (f *fakeLLM) Call(_ context.Context, prompt string) (string, error) {
	f.got = append(f.got, prompt)
	if f.err != nil {
		return "", f.err
	}
	return f.resp, nil
}

type fakeTools struct {
	out map[string]string         // tool name → raw output string
	err map[string]error          // tool name → forced error
	got map[string][]map[string]any // tool name → received args per call
}

func (f *fakeTools) Call(_ context.Context, name string, args map[string]any) (string, error) {
	if f.got == nil {
		f.got = map[string][]map[string]any{}
	}
	f.got[name] = append(f.got[name], args)
	if e, ok := f.err[name]; ok {
		return "", e
	}
	return f.out[name], nil
}

func newTestStore(t *testing.T) *store.DBStore {
	t.Helper()
	s, err := store.NewDBStore("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = s.DB().Close() })
	return s
}

// linearYAML is the hello-world definition: a tool node feeds an LLM node.
const linearYAML = `
version: 1
input:
  schema:
    type: object
    properties:
      topic: {type: string}
nodes:
  - name: fetch
    kind: tool
    tool: get_data
    input:
      topic: ${input.topic}
  - name: summarize
    kind: llm
    prompt: "Summarize this: ${fetch.summary}"
    output:
      result: {type: string}
edges:
  - {from: fetch, to: summarize}
`

// AC 1, 2, 3, 4 — linear tool→LLM success path.
func TestRunner_LinearSuccess(t *testing.T) {
	def, err := workflow.Parse("hello", []byte(linearYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	llm := &fakeLLM{resp: `{"result":"cats are great"}`}
	tools := &fakeTools{out: map[string]string{"get_data": `{"summary":"hello world"}`}}
	st := newTestStore(t)
	r := workflow.NewRunner(llm, tools, st)

	res, err := r.Run(context.Background(), def, map[string]any{"topic": "cats"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// AC1 — status succeeded, result is the final node's output.
	if res.Status != workflow.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", res.Status)
	}
	if got := res.Result["result"]; got != "cats are great" {
		t.Errorf("result = %#v, want {result: \"cats are great\"}", res.Result)
	}

	// AC2 — ${fetch.summary} resolved into the LLM prompt.
	if len(llm.got) == 0 || !strings.Contains(llm.got[0], "hello world") {
		t.Errorf("llm prompt = %#v, want it to contain resolved %q", llm.got, "hello world")
	}
	// AC2 (input side) — ${input.topic} resolved into the tool node's args.
	if calls, ok := tools.got["get_data"]; !ok || len(calls) == 0 || calls[0]["topic"] != "cats" {
		t.Errorf("tool args = %#v, want topic=cats", tools.got)
	}

	// AC3 — one run row + one row per executed node persisted.
	runStatus, _, err := st.GetWorkflowRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if runStatus != string(workflow.StatusSucceeded) {
		t.Errorf("persisted run status = %q, want succeeded", runStatus)
	}
	rows, err := st.ListWorkflowNodeOutputs(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("ListWorkflowNodeOutputs: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("persisted node outputs = %d, want 2", len(rows))
	}

	// AC4 — snapshot carries each completed node's output.
	if len(res.Snapshot) != 2 {
		t.Fatalf("snapshot = %d nodes, want 2", len(res.Snapshot))
	}
	fetchOut, ok := res.Snapshot["fetch"]
	if !ok {
		t.Fatal("snapshot missing fetch")
	}
	if fetchOut.Status != workflow.StatusSucceeded || fetchOut.Output["summary"] != "hello world" {
		t.Errorf("snapshot[fetch] = %+v, want status=succeeded summary=hello world", fetchOut)
	}
	sumOut, ok := res.Snapshot["summarize"]
	if !ok {
		t.Fatal("snapshot missing summarize")
	}
	if sumOut.Output["result"] != "cats are great" {
		t.Errorf("snapshot[summarize].output = %+v, want result=cats are great", sumOut.Output)
	}
}

// AC 5 — a node failure surfaces status=failed with node id + original error,
// and the snapshot keeps the nodes that completed before the failure (plus the
// failure scene itself, per spec decision 5 — snapshot is resume + diagnostics
// input).
func TestRunner_NodeFailure(t *testing.T) {
	const yamlDef = `
version: 1
nodes:
  - name: ok
    kind: tool
    tool: good_tool
  - name: boom
    kind: tool
    tool: bad_tool
edges:
  - {from: ok, to: boom}
`
	def, err := workflow.Parse("fail", []byte(yamlDef))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	boomErr := errors.New("kaboom")
	tools := &fakeTools{
		out: map[string]string{"good_tool": `{"x": 42}`},
		err: map[string]error{"bad_tool": boomErr},
	}
	st := newTestStore(t)
	r := workflow.NewRunner(&fakeLLM{}, tools, st)

	res, err := r.Run(context.Background(), def, map[string]any{})
	if err != nil {
		t.Fatalf("Run returned a go error %v; node failures should land in ExecutionResult, not as a go err", err)
	}

	if res.Status != workflow.StatusFailed {
		t.Errorf("status = %q, want failed", res.Status)
	}
	if res.Error == nil {
		t.Fatal("ExecutionResult.Error is nil")
	}
	if res.Error.Node != "boom" {
		t.Errorf("error.node = %q, want boom", res.Error.Node)
	}
	if !strings.Contains(res.Error.Message, "kaboom") {
		t.Errorf("error.message = %q, want it to contain the original error %q", res.Error.Message, "kaboom")
	}

	// Pre-failure completed node is retained.
	okNode, ok := res.Snapshot["ok"]
	if !ok {
		t.Fatal("snapshot missing 'ok' (the pre-failure completed node)")
	}
	if okNode.Status != workflow.StatusSucceeded {
		t.Errorf("snapshot[ok].status = %q, want succeeded", okNode.Status)
	}
	if okNode.Output["x"] != float64(42) { // JSON number → float64
		t.Errorf("snapshot[ok].output.x = %v, want 42", okNode.Output["x"])
	}

	// The failing node is recorded as failed (resume / diagnostics input).
	boomNode, ok := res.Snapshot["boom"]
	if !ok {
		t.Fatal("snapshot should retain the failed node 'boom' for diagnosis")
	}
	if boomNode.Status != workflow.StatusFailed {
		t.Errorf("snapshot[boom].status = %q, want failed", boomNode.Status)
	}

	// The failed attempt is persisted too.
	rows, err := st.ListWorkflowNodeOutputs(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("ListWorkflowNodeOutputs: %v", err)
	}
	var sawFailedBoom bool
	for _, rw := range rows {
		if rw.NodeID == "boom" && rw.Status == string(workflow.StatusFailed) {
			sawFailedBoom = true
		}
	}
	if !sawFailedBoom {
		t.Errorf("persisted node outputs = %+v, want a failed boom row", rows)
	}
}
