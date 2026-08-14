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

// A reply node (M3 domain node) emits a templated response: Prompt is the reply
// body with ${input.*} / ${node.*} references resolved inline, and its {text}
// output becomes the workflow's result when it's the terminal node. No leaf is
// invoked — reply is pure templating, so the LLM caller stays untouched.
func TestRunner_ReplyNode(t *testing.T) {
	const yamlDef = `
version: 1
input:
  schema:
    type: object
    properties:
      user: {type: string}
nodes:
  - name: fetch
    kind: tool
    tool: get_data
    input:
      user: ${input.user}
  - name: answer
    kind: reply
    prompt: "Hi ${fetch.name}, you asked about ${input.user}."
edges:
  - {from: fetch, to: answer}
`
	def, err := workflow.Parse("reply", []byte(yamlDef))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	llm := &fakeLLM{}
	tools := &fakeTools{out: map[string]string{"get_data": `{"name":"Alice"}`}}
	st := newTestStore(t)
	r := workflow.NewRunner(llm, tools, st)

	res, err := r.Run(context.Background(), def, map[string]any{"user": "cats"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != workflow.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", res.Status)
	}
	// reply's {text} output is the terminal node's output → Result (no output map).
	if got := res.Result["text"]; got != "Hi Alice, you asked about cats." {
		t.Errorf("result.text = %#v, want the resolved reply template", got)
	}
	// the reply node never calls the LLM leaf (pure templating).
	if len(llm.got) != 0 {
		t.Errorf("llm was called %d times; reply must not invoke the llm leaf", len(llm.got))
	}
}

// A question_rewrite node (M3 domain node) asks the LLM to reformulate a query
// for retrieval and canonicalizes the reply to a {query} field. Downstream
// nodes reference ${rewrite.query} — here a reply node echoes it back, proving
// the rewrite output flows through the reference machinery.
func TestRunner_QuestionRewriteNode(t *testing.T) {
	const yamlDef = `
version: 1
input:
  schema:
    type: object
    properties:
      q: {type: string}
nodes:
  - name: rewrite
    kind: question_rewrite
    prompt: "Rewrite as a concise search query: ${input.q}"
  - name: answer
    kind: reply
    prompt: "Searching the KB for: ${rewrite.query}"
edges:
  - {from: rewrite, to: answer}
`
	def, err := workflow.Parse("qr", []byte(yamlDef))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	llm := &fakeLLM{resp: "feline behavior"}
	st := newTestStore(t)
	r := workflow.NewRunner(llm, &fakeTools{}, st)

	res, err := r.Run(context.Background(), def, map[string]any{"q": "cats"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != workflow.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", res.Status)
	}
	// the bare-text LLM reply was canonicalized to {query}.
	if got := res.Snapshot["rewrite"].Output["query"]; got != "feline behavior" {
		t.Errorf("rewrite.query = %#v, want \"feline behavior\" (canonicalized from bare-text reply)", got)
	}
	// ${rewrite.query} flowed into the reply node.
	if got := res.Result["text"]; got != "Searching the KB for: feline behavior" {
		t.Errorf("result.text = %#v, want the reply to carry the rewritten query", got)
	}
}

// fakeHTTP is the Seam A fake for HTTPCaller — it records the resolved request
// line and returns a canned status + body the runner parses into {status,body}.
type fakeHTTP struct {
	got    []httpCall
	status int
	body   string
	err    error
}

type httpCall struct {
	method, url string
	headers     map[string]string
	body        string
}

func (f *fakeHTTP) Do(_ context.Context, method, url string, headers map[string]string, body string) (int, string, error) {
	f.got = append(f.got, httpCall{method: method, url: url, headers: headers, body: body})
	if f.err != nil {
		return 0, "", f.err
	}
	status := f.status
	if status == 0 {
		status = 200
	}
	return status, f.body, nil
}

// An http node (M3 domain node) issues one outbound request and exposes
// {status, body}. ${...} references in its Input (url/headers/body) resolve
// before the call; the body is parsed the same way tool/llm returns are, so a
// JSON response yields addressable fields (${fetch.body.field}).
func TestRunner_HTTPNode(t *testing.T) {
	const yamlDef = `
version: 1
input:
  schema:
    type: object
    properties:
      q: {type: string}
nodes:
  - name: fetch
    kind: http
    input:
      method: GET
      url: "https://api.example.com/search?q=${input.q}"
      headers:
        Authorization: "Bearer secret"
  - name: answer
    kind: reply
    prompt: "Got ${fetch.body.result} (HTTP ${fetch.status})"
edges:
  - {from: fetch, to: answer}
`
	def, err := workflow.Parse("http", []byte(yamlDef))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	hc := &fakeHTTP{body: `{"result":"ok"}`}
	st := newTestStore(t)
	r := workflow.NewRunner(&fakeLLM{}, &fakeTools{}, st, workflow.WithHTTPCaller(hc))

	res, err := r.Run(context.Background(), def, map[string]any{"q": "cats"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != workflow.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", res.Status)
	}
	// ${input.q} resolved into the url; method defaulted to GET; header passed.
	if len(hc.got) != 1 {
		t.Fatalf("http calls = %d, want 1", len(hc.got))
	}
	if !strings.Contains(hc.got[0].url, "q=cats") {
		t.Errorf("url = %q, want ${input.q} resolved to q=cats", hc.got[0].url)
	}
	if hc.got[0].method != "GET" {
		t.Errorf("method = %q, want GET", hc.got[0].method)
	}
	if hc.got[0].headers["Authorization"] != "Bearer secret" {
		t.Errorf("headers = %#v, want Authorization passed through", hc.got[0].headers)
	}
	// {status, body} output; JSON body parsed; both flow into the reply.
	// status round-trips through the DB snapshot as float64 (JSON number).
	if got := res.Snapshot["fetch"].Output["status"]; got != float64(200) {
		t.Errorf("fetch.status = %v, want 200", got)
	}
	if got := res.Result["text"]; got != "Got ok (HTTP 200)" {
		t.Errorf("result.text = %#v, want body+status resolved into the reply", got)
	}
}

// A kb_search node (M3 domain node) wraps the builtin knowledgebase_search
// tool: Input is resolved and forwarded, and the tool's raw text return is
// wrapped as {result}. It behaves like a tool node with the name fixed, so the
// editor can offer a dedicated "search the knowledge base" node.
func TestRunner_KBSearchNode(t *testing.T) {
	const yamlDef = `
version: 1
input:
  schema:
    type: object
    properties:
      q: {type: string}
nodes:
  - name: search
    kind: kb_search
    input:
      query: ${input.q}
      limit: 3
  - name: answer
    kind: reply
    prompt: "Top result: ${search.result}"
edges:
  - {from: search, to: answer}
`
	def, err := workflow.Parse("kb", []byte(yamlDef))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	tools := &fakeTools{out: map[string]string{"knowledgebase_search": "Found: cats love naps"}}
	st := newTestStore(t)
	r := workflow.NewRunner(&fakeLLM{}, tools, st)

	res, err := r.Run(context.Background(), def, map[string]any{"q": "cats"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != workflow.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", res.Status)
	}
	// knowledgebase_search called with the resolved query + limit forwarded.
	calls, ok := tools.got["knowledgebase_search"]
	if !ok || len(calls) == 0 {
		t.Fatalf("kb_search not called: %#v", tools.got)
	}
	if calls[0]["query"] != "cats" {
		t.Errorf("query = %#v, want cats resolved from ${input.q}", calls[0]["query"])
	}
	if calls[0]["limit"] != 3 {
		t.Errorf("limit = %#v, want 3 forwarded", calls[0]["limit"])
	}
	// tool's raw text return wrapped as {result}, flowed into the reply.
	if got := res.Result["text"]; got != "Top result: Found: cats love naps" {
		t.Errorf("result.text = %#v, want the kb result flowed into the reply", got)
	}
}

// WithEventSink (M4) lets a caller observe node-level progress: the runner
// emits NodeStart / NodeComplete per node in execution order, then a terminal
// Done with the resolved result. Without a sink, Run is unchanged.
func TestRunner_EventStream(t *testing.T) {
	def, err := workflow.Parse("hello", []byte(linearYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var events []workflow.RunEvent
	llm := &fakeLLM{resp: `{"result":"cats are great"}`}
	tools := &fakeTools{out: map[string]string{"get_data": `{"summary":"hello world"}`}}
	st := newTestStore(t)
	r := workflow.NewRunner(llm, tools, st)

	_, err = r.Run(context.Background(), def, map[string]any{"topic": "cats"}, workflow.WithEventSink(func(e workflow.RunEvent) {
		events = append(events, e)
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// start/complete per node, then a terminal done.
	wantTypes := []string{workflow.EventNodeStart, workflow.EventNodeComplete, workflow.EventNodeStart, workflow.EventNodeComplete, workflow.EventDone}
	if len(events) != len(wantTypes) {
		t.Fatalf("events = %d, want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, w := range wantTypes {
		if events[i].Type != w {
			t.Errorf("events[%d].type = %q, want %q", i, events[i].Type, w)
		}
	}
	// Nodes announced in execution (declaration) order.
	if events[0].Node != "fetch" || events[2].Node != "summarize" {
		t.Errorf("node order = %q…%q, want fetch then summarize", events[0].Node, events[2].Node)
	}
	// node_complete carries the parsed output; done carries the resolved result.
	if events[1].Output["summary"] != "hello world" {
		t.Errorf("fetch complete output = %#v, want summary=hello world", events[1].Output)
	}
	if events[4].Output["result"] != "cats are great" {
		t.Errorf("done output = %#v, want result=cats are great", events[4].Output)
	}
}

// A set node (M5) writes resolved values into the run's writable variable
// space; downstream nodes read them via ${var.name}. Values resolve ${...}
// refs (input + earlier vars) before being written. set has no leaf — it is a
// pure side effect on the variable space.
func TestRunner_SetNode(t *testing.T) {
	const yamlDef = `
version: 1
input:
  schema:
    type: object
    properties:
      n: {type: integer}
nodes:
  - name: init
    kind: set
    input:
      counter: 0
      label: "run-${input.n}"
  - name: answer
    kind: reply
    prompt: "counter=${var.counter} label=${var.label}"
edges:
  - {from: init, to: answer}
`
	def, err := workflow.Parse("set", []byte(yamlDef))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st := newTestStore(t)
	r := workflow.NewRunner(&fakeLLM{}, &fakeTools{}, st)

	res, err := r.Run(context.Background(), def, map[string]any{"n": 7})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != workflow.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", res.Status)
	}
	// init wrote counter=0 + label="run-7" (input ref resolved); answer read
	// both back via ${var.*}.
	if got := res.Result["text"]; got != "counter=0 label=run-7" {
		t.Errorf("result.text = %#v, want counter=0 label=run-7", got)
	}
}

// A condition node (M1b, previously a front-end-only half-feature) is a
// pass-through branch point: it runs no leaf, emits {}, and its outgoing
// edges' `when` pick the branch. Here a high/low split on ${input.score}.
func TestRunner_ConditionNode(t *testing.T) {
	const yamlDef = `
version: 1
input:
  schema:
    type: object
    properties:
      score: {type: number}
nodes:
  - name: gate
    kind: condition
  - name: high
    kind: reply
    prompt: "high"
  - name: low
    kind: reply
    prompt: "low"
edges:
  - {from: gate, to: high, when: "${input.score} > 0.5"}
  - {from: gate, to: low, when: "${input.score} <= 0.5"}
`
	def, err := workflow.Parse("cond", []byte(yamlDef))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st := newTestStore(t)
	r := workflow.NewRunner(&fakeLLM{}, &fakeTools{}, st)

	// score 0.9 → the > 0.5 edge fires → high.
	res, err := r.Run(context.Background(), def, map[string]any{"score": 0.9})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != workflow.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", res.Status)
	}
	if got := res.Result["text"]; got != "high" {
		t.Errorf("score 0.9 result = %#v, want high", got)
	}

	// score 0.3 → the <= 0.5 edge fires → low.
	res2, err := r.Run(context.Background(), def, map[string]any{"score": 0.3})
	if err != nil {
		t.Fatalf("Run (low): %v", err)
	}
	if got := res2.Result["text"]; got != "low" {
		t.Errorf("score 0.3 result = %#v, want low", got)
	}
}
