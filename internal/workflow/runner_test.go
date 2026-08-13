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

// fakeCode is the Seam A fake for CodeCaller — it records the interpolated
// script + language and returns a canned raw output the runner parses.
type fakeCode struct {
	out string
	err error
	got []codeCall
}

type codeCall struct {
	lang string
	code string
}

func (f *fakeCode) Run(_ context.Context, language, code string) (string, error) {
	f.got = append(f.got, codeCall{lang: language, code: code})
	if f.err != nil {
		return "", f.err
	}
	return f.out, nil
}

// A code node runs its script in the sandbox (here faked), with ${input.*}
// references interpolated into the code body before the call. Its raw stdout is
// parsed the same way a tool/llm node's return is — JSON object → output map.
func TestRunner_CodeNode(t *testing.T) {
	const yamlDef = `
version: 1
input:
  schema:
    type: object
    properties:
      n: {type: integer}
nodes:
  - name: compute
    kind: code
    lang: python
    code: |
      n = ${input.n}
      print(json.dumps({"doubled": n * 2}))
`
	def, err := workflow.Parse("code", []byte(yamlDef))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	code := &fakeCode{out: `{"doubled": 84}`}
	st := newTestStore(t)
	r := workflow.NewRunner(&fakeLLM{}, &fakeTools{}, st, workflow.WithCodeCaller(code))

	res, err := r.Run(context.Background(), def, map[string]any{"n": 42})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != workflow.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", res.Status)
	}
	// ${input.n} was interpolated into the code body before the call.
	if len(code.got) != 1 {
		t.Fatalf("code calls = %d, want 1", len(code.got))
	}
	if code.got[0].lang != "python" {
		t.Errorf("lang = %q, want python", code.got[0].lang)
	}
	if !strings.Contains(code.got[0].code, "n = 42") {
		t.Errorf("code body = %q, want ${input.n} resolved to 'n = 42'", code.got[0].code)
	}
	// stdout JSON parsed into the node output → result (no output map → last node).
	if res.Result["doubled"] != float64(84) {
		t.Errorf("result = %#v, want doubled=84", res.Result)
	}
}

// A code node with no CodeCaller wired fails with a clear message at runtime
// (the default code=nil path) rather than panicking.
func TestRunner_CodeNodeNoCaller(t *testing.T) {
	const yamlDef = `
version: 1
nodes:
  - name: compute
    kind: code
    code: "print(1)"
`
	def, err := workflow.Parse("codenocall", []byte(yamlDef))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st := newTestStore(t)
	r := workflow.NewRunner(&fakeLLM{}, &fakeTools{}, st) // no WithCodeCaller

	res, err := r.Run(context.Background(), def, map[string]any{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != workflow.StatusFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if res.Error == nil || !strings.Contains(res.Error.Message, "code caller") {
		t.Errorf("error = %+v, want a \"code caller\" message", res.Error)
	}
}

// A workflow-level output map reshapes the Result: each value is resolved as a
// reference (${node.field} / ${input.field}). A whole-${ref} value keeps its
// native type; an inline ref is substituted as text. Without the map, the last
// node's raw output would be the Result — here that leak is asserted against.
func TestRunner_WorkflowOutputMap(t *testing.T) {
	const yamlDef = `
version: 1
nodes:
  - name: fetch
    kind: tool
    tool: get_data
output:
  fetched: ${fetch.result}
  mixed: "raw ${fetch.result} tail"
`
	def, err := workflow.Parse("out", []byte(yamlDef))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	tools := &fakeTools{out: map[string]string{"get_data": `{"result":"hello"}`}}
	st := newTestStore(t)
	r := workflow.NewRunner(&fakeLLM{}, tools, st)

	res, err := r.Run(context.Background(), def, map[string]any{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != workflow.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", res.Status)
	}
	// whole-ref keeps the native string type.
	if res.Result["fetched"] != "hello" {
		t.Errorf("fetched = %#v, want \"hello\"", res.Result["fetched"])
	}
	// inline ref substituted as text.
	if res.Result["mixed"] != "raw hello tail" {
		t.Errorf("mixed = %#v, want \"raw hello tail\"", res.Result["mixed"])
	}
	// a key absent from the output map is NOT leaked from the last node.
	if _, leaked := res.Result["result"]; leaked {
		t.Errorf("Result leaked the last-node key %q (output map should fully own Result)", "result")
	}
}
