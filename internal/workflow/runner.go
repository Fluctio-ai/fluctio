package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// LLMCaller is the leaf capability an llm node drives: a bare provider call
// returning the model's raw content. The runner parses that content against the
// node's output schema. ProviderLLMCaller (adapters.go) wraps provider.Provider.
type LLMCaller interface {
	Call(ctx context.Context, prompt string) (string, error)
}

// ToolCaller drives a tool node: invoke one registered tool by name. The
// runner parses the raw return into the node's output map. RegistryToolCaller
// (adapters.go) wraps tools.Registry.Execute.
type ToolCaller interface {
	Call(ctx context.Context, name string, args map[string]any) (string, error)
}

// CodeCaller drives a code node: run a script in the given language inside a
// sandbox and return its raw stdout, which the runner parses against the
// node's output schema (spec decision 3). SandboxCodeCaller (adapters.go)
// wraps sandbox.Executor. A nil CodeCaller (the default) makes any code node
// fail at runtime with a clear "no code caller" error.
type CodeCaller interface {
	Run(ctx context.Context, language, code string) (string, error)
}

// RunStore is the persistence seam. *store.DBStore implements it; the runner
// holds the interface so its graph/contract logic is testable without a DB.
type RunStore interface {
	CreateWorkflowRun(ctx context.Context, id, defID string, version int, input map[string]any, sessionID, owner string) error
	MarkRunRunning(ctx context.Context, id string) error
	FinalizeWorkflowRun(ctx context.Context, id, status, errMsg string) error
	AppendWorkflowNodeOutput(ctx context.Context, runID, nodeID string, attempt int, status string, output map[string]any, errMsg string) error
	ListWorkflowNodeOutputs(ctx context.Context, runID string) ([]store.WorkflowNodeOutputRow, error)
}

// Runner executes a Definition's graph against input and returns an
// ExecutionResult. It owns no state between runs (spec decision 12) — every
// run is fully isolated.
type Runner struct {
	llm   LLMCaller
	tool  ToolCaller
	code  CodeCaller
	store RunStore
	newID func() string
}

// RunnerOption configures a Runner at construction. The only option today is
// WithCodeCaller; llm/tool/store stay positional because every runner needs
// them, so existing three-arg callers keep working (code defaults to nil).
type RunnerOption func(*Runner)

// WithCodeCaller wires the sandbox code caller (spec decision 3). Without it a
// code node fails at runtime with a clear "no code caller" error; tool/llm-only
// workflows need not set it.
func WithCodeCaller(c CodeCaller) RunnerOption {
	return func(r *Runner) { r.code = c }
}

// NewRunner wires the leaf callers + persistence. Tests inject fakes here;
// production callers use ProviderLLMCaller / RegistryToolCaller /
// SandboxCodeCaller from adapters.go. code defaults to nil when no option is
// passed, so a tool/llm-only workflow runs without a sandbox.
func NewRunner(llm LLMCaller, tool ToolCaller, rs RunStore, opts ...RunnerOption) *Runner {
	r := &Runner{llm: llm, tool: tool, store: rs, newID: newRunID}
	for _, o := range opts {
		o(r)
	}
	return r
}

func newRunID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "wf_" + hex.EncodeToString(b[:])
}

// RunOption configures a Run call. The only option today is WithResume.
type RunOption func(*runConfig)

type runConfig struct {
	runID   string             // original run id when resuming; "" for a fresh run
	resume  map[string]NodeOutput // populated by Run from the DB (decision 10)
	owner   string             // run owner (spec decision 14); "" = system
	session string             // agent session an LLM-triggered run hangs off; "" = none
}

// WithResume resumes a previously-failed run in place. The runner reuses the
// run_id, reloads the authoritative snapshot from workflow_node_outputs
// (decision 10 — it does not trust a caller-supplied copy), skips succeeded
// nodes, re-runs failed ones at attempt+1, and refuses to auto-rerun a failed
// non-idempotent node (spec decision 2).
func WithResume(runID string) RunOption {
	return func(c *runConfig) { c.runID = runID }
}

// WithOwner / WithSession attach the run's ownership (spec decision 14): owner
// is the calling user (LLM/manual triggers) or "" for system (cron); session
// is the agent session an LLM-triggered run hangs off, or "" for none.
func WithOwner(owner string) RunOption {
	return func(c *runConfig) { c.owner = owner }
}

func WithSession(session string) RunOption {
	return func(c *runConfig) { c.session = session }
}

// refScope bundles the two reference namespaces a node resolves against — the
// run input and the completed nodes' outputs — so the resolve helpers carry one
// value instead of an (input, outputs) pair everywhere (spec decision 4).
type refScope struct {
	input   map[string]any
	outputs map[string]map[string]any
}

func (s refScope) lookup(expr string) (any, bool) {
	parts := strings.Split(expr, ".")
	if len(parts) == 0 {
		return nil, false
	}
	var root map[string]any
	if parts[0] == "input" {
		root = s.input
	} else {
		root = s.outputs[parts[0]]
	}
	if root == nil {
		return nil, false
	}
	return dig(root, parts[1:])
}

// Run executes def starting at the single entry node and follows outgoing
// edges (spec decision 3): plain edges walk a linear chain; deterministic
// `when` expressions pick the first true branch; llm_route edges ask the LLM
// to choose (falling back to `default`, or failing the node if nothing matches
// — "LLM 路由无 default 则该节点失败" is a runtime failure). A node failure
// stops the run with status=failed + the failing node and a snapshot of
// everything attempted (decision 5). The snapshot is assembled from
// workflow_node_outputs (decision 10); its run_id is what WithResume takes.
//
// WithResume(runID) reloads the snapshot from the DB (decision 10), flips the
// run to "running" (decision 15), skips succeeded nodes (no re-invocation),
// re-runs failed nodes at attempt+1 (append-only, decision 15), and refuses
// to auto-rerun a failed non-idempotent node (decision 2).
//
// Resume caveat: edge selection re-runs even for skipped (succeeded) nodes.
// Deterministic branches reproduce from the cached output; llm_route re-asks
// the LLM and may diverge from the original path — accepted for the tracer
// bullet (decision 12's "stateless reproducible" can't hold when the LLM
// router is in the loop).
func (r *Runner) Run(ctx context.Context, def *Definition, input map[string]any, opts ...RunOption) (*ExecutionResult, error) {
	if input == nil {
		input = map[string]any{}
	}
	if err := Validate(def, input); err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}

	var cfg runConfig
	for _, o := range opts {
		o(&cfg)
	}

	runID := cfg.runID
	if runID != "" {
		if err := r.store.MarkRunRunning(context.Background(), runID); err != nil {
			return nil, fmt.Errorf("mark run running: %w", err)
		}
		resumeSnap, err := r.loadSnapshot(context.Background(), runID, def)
		if err != nil {
			return nil, fmt.Errorf("load resume snapshot: %w", err)
		}
		cfg.resume = resumeSnap
	} else {
		runID = r.newID()
		if err := r.store.CreateWorkflowRun(context.Background(), runID, def.ID, def.Version, input, cfg.session, cfg.owner); err != nil {
			return nil, fmt.Errorf("create workflow run: %w", err)
		}
	}

	sc := refScope{input: input, outputs: map[string]map[string]any{}}
	for name, no := range cfg.resume {
		if no.Status == StatusSucceeded {
			sc.outputs[name] = no.Output
		}
	}

	// Walk the graph from the single entry node (Validate guarantees one).
	current := entryNode(def)
	var lastOutput map[string]any
	for current != "" {
		// Honor a canceled ctx (cancel_previous, spec decision 13) at the next
		// node boundary — a mid-run cancel stops here, leaving already-completed
		// nodes in the snapshot.
		if err := ctx.Err(); err != nil {
			return r.endRun(runID, def, current, StatusFailed, "canceled: "+err.Error())
		}
		name := current
		if prior, seen := cfg.resume[name]; seen && prior.Status == StatusSucceeded {
			lastOutput = prior.Output
		} else {
			out, status, msg, perr := r.executeStep(ctx, name, sc, cfg.resume, runID, def)
			if perr != nil {
				return nil, perr
			}
			if status != StatusSucceeded {
				return r.endRun(runID, def, name, status, msg)
			}
			sc.outputs[name] = out
			lastOutput = out
		}

		edges := outEdges(def, name)
		if len(edges) == 0 {
			break // terminal node
		}
		next, selErr := r.selectEdge(ctx, edges, sc)
		if selErr != nil {
			// selErr already says "no matching edge from X" (genuine AC4
			// no-match) or "edge X→Y: <expr error>" — don't re-prefix and
			// conflate the two.
			return r.endRun(runID, def, name, StatusFailed, selErr.Error())
		}
		current = next
	}

	if ferr := r.store.FinalizeWorkflowRun(context.Background(), runID, string(StatusSucceeded), ""); ferr != nil {
		return nil, fmt.Errorf("finalize run: %w", ferr)
	}
	snap, err := r.loadSnapshot(context.Background(), runID, def)
	if err != nil {
		return nil, fmt.Errorf("load snapshot: %w", err)
	}
	return &ExecutionResult{
		RunID:    runID,
		Status:   StatusSucceeded,
		Result:   resolveWorkflowOutput(def, sc, lastOutput),
		Snapshot: snap,
	}, nil
}

// resolveWorkflowOutput builds the workflow-level Result (spec decision 4 data
// flow, extended by the output-map feature). When the definition declares an
// output map, each value is resolved as a reference (${input.*} / ${node.*});
// a value that is exactly ${ref} keeps its native type, inline refs are
// substituted as text. With no output map the last node's output is the
// result — the pre-output-map behavior, preserved for existing workflows.
func resolveWorkflowOutput(def *Definition, sc refScope, lastOutput map[string]any) map[string]any {
	if len(def.Output) == 0 {
		return lastOutput
	}
	out := make(map[string]any, len(def.Output))
	for k, v := range def.Output {
		out[k] = resolveValue(v, sc)
	}
	return out
}

// executeStep runs one node's leaf action and persists the outcome, applying
// the resume rules (skip is the caller's job; refuse non-idempotent failures;
// re-run at attempt+1). It returns the parsed output plus a terminal
// status+message when the step itself ends the run, or a go error on a
// persistence failure.
func (r *Runner) executeStep(ctx context.Context, name string, sc refScope, resume map[string]NodeOutput, runID string, def *Definition) (map[string]any, Status, string, error) {
	node := nodeByName(def, name)
	prior, seen := resume[name]
	if seen && prior.Status == StatusFailed && node.SideEffect == SideEffectNonIdempotent {
		return nil, StatusNeedsIntervention, "non-idempotent node failed; manual intervention required to resume", nil
	}
	attempt := 1
	if seen {
		attempt = prior.Attempt + 1
	}
	out, execErr := r.execNode(ctx, node, sc)
	if execErr != nil {
		msg := execErr.Error()
		if perr := r.store.AppendWorkflowNodeOutput(context.Background(), runID, name, attempt, string(StatusFailed), nil, msg); perr != nil {
			return nil, "", "", perr
		}
		return nil, StatusFailed, msg, nil
	}
	if perr := r.store.AppendWorkflowNodeOutput(context.Background(), runID, name, attempt, string(StatusSucceeded), out, ""); perr != nil {
		return nil, "", "", perr
	}
	return out, StatusSucceeded, "", nil
}

// endRun finalizes a failing run and assembles the ExecutionResult. Shared by
// the exec-failure and non-idempotent-refusal paths; the caller persists any
// new attempt row before calling (the refusal path persists none, since the
// node isn't re-executed).
func (r *Runner) endRun(runID string, def *Definition, node string, status Status, msg string) (*ExecutionResult, error) {
	if ferr := r.store.FinalizeWorkflowRun(context.Background(), runID, string(status), fmt.Sprintf("node %s: %s", node, msg)); ferr != nil {
		return nil, fmt.Errorf("finalize run: %w", ferr)
	}
	snap, err := r.loadSnapshot(context.Background(), runID, def)
	if err != nil {
		return nil, fmt.Errorf("load snapshot: %w", err)
	}
	return &ExecutionResult{
		RunID:    runID,
		Status:   status,
		Error:    &NodeError{Node: node, Message: msg},
		Snapshot: snap,
	}, nil
}

// loadSnapshot assembles the completed-nodes snapshot straight from the
// persisted node-output rows (spec decision 10). Kind is the one field the
// table doesn't store, so it is re-read from the definition. When a node has
// multiple attempts (resume, decision 15), the latest attempt wins.
func (r *Runner) loadSnapshot(ctx context.Context, runID string, def *Definition) (map[string]NodeOutput, error) {
	rows, err := r.store.ListWorkflowNodeOutputs(ctx, runID)
	if err != nil {
		return nil, err
	}
	latest := make(map[string]store.WorkflowNodeOutputRow, len(rows))
	for _, rw := range rows {
		if cur, ok := latest[rw.NodeID]; !ok || rw.Attempt >= cur.Attempt {
			latest[rw.NodeID] = rw
		}
	}
	snap := make(map[string]NodeOutput, len(latest))
	for name, rw := range latest {
		snap[name] = NodeOutput{
			Name:    name,
			Kind:    nodeByName(def, name).Kind,
			Status:  Status(rw.Status),
			Output:  rw.Output,
			Error:   rw.Error,
			Attempt: rw.Attempt,
		}
	}
	return snap, nil
}

// execNode resolves references, dispatches by kind, and parses the raw leaf
// output into the node's output map.
func (r *Runner) execNode(ctx context.Context, node Node, sc refScope) (map[string]any, error) {
	switch node.Kind {
	case KindTool:
		args := resolveInput(node.Input, sc)
		raw, err := r.tool.Call(ctx, node.Tool, args)
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", node.Tool, err)
		}
		return parseOutput(raw), nil
	case KindLLM:
		prompt := resolveRefs(node.Prompt, sc)
		raw, err := r.llm.Call(ctx, prompt)
		if err != nil {
			return nil, fmt.Errorf("llm: %w", err)
		}
		return parseOutput(raw), nil
	case KindCode:
		if r.code == nil {
			return nil, fmt.Errorf("code: no sandbox code caller wired")
		}
		lang := node.Language
		if lang == "" {
			lang = "python"
		}
		raw, err := r.code.Run(ctx, lang, resolveRefs(node.Code, sc))
		if err != nil {
			return nil, fmt.Errorf("code: %w", err)
		}
		return parseOutput(raw), nil
	default:
		return nil, fmt.Errorf("unknown node kind %q", node.Kind)
	}
}

// --- reference resolution (spec decision 4) ---
//
// Two forms:
//   - inline: ${ref} inside a longer string → substituted as text.
//   - whole:  a value that is exactly ${ref} → replaced by the native typed
//     value, so downstream nodes get numbers/objects/arrays, not their string
//     rendering.

var (
	refPattern = regexp.MustCompile(`\$\{([^}]+)\}`)
	exactRef   = regexp.MustCompile(`^\$\{([^}]+)\}$`)
)

func resolveRefs(s string, sc refScope) string {
	return refPattern.ReplaceAllStringFunc(s, func(tok string) string {
		expr := tok[2 : len(tok)-1] // strip ${ }
		v, ok := sc.lookup(expr)
		if !ok {
			return tok // leave unresolved refs intact — Seam B validates them
		}
		return stringify(v)
	})
}

func resolveInput(in map[string]any, sc refScope) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = resolveValue(v, sc)
	}
	return out
}

func resolveValue(v any, sc refScope) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	if m := exactRef.FindStringSubmatch(s); m != nil {
		if val, ok := sc.lookup(m[1]); ok {
			return val
		}
	}
	return resolveRefs(s, sc)
}

// dig descends a dotted path through nested maps.
func dig(m map[string]any, path []string) (any, bool) {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprintf("%v", x)
		}
		return string(b)
	}
}

// parseOutput decodes a leaf's raw string into a map. A JSON object maps
// directly; anything else is wrapped as {"result": <raw>} so a downstream
// ${node.result} reference always resolves.
func parseOutput(raw string) map[string]any {
	trim := strings.TrimSpace(raw)
	if trim != "" {
		var m map[string]any
		if err := json.Unmarshal([]byte(trim), &m); err == nil {
			return m
		}
	}
	return map[string]any{"result": raw}
}

// --- graph utilities ---

// topoOrder returns node names in execution order via Kahn's algorithm, seeded
// in declaration order for determinism. The tracer bullet only needs linear
// chains, but the general DAG walk costs nothing extra and stays honest for
// ticket 04's branches.
func topoOrder(def *Definition) ([]string, error) {
	names := make([]string, 0, len(def.Nodes))
	nodes := make(map[string]Node, len(def.Nodes))
	indeg := make(map[string]int, len(def.Nodes))
	next := make(map[string][]string, len(def.Nodes))
	for _, n := range def.Nodes {
		if _, dup := nodes[n.Name]; dup {
			return nil, fmt.Errorf("duplicate node %q", n.Name)
		}
		nodes[n.Name] = n
		indeg[n.Name] = 0
		names = append(names, n.Name)
	}
	for _, e := range def.Edges {
		if _, ok := nodes[e.From]; !ok {
			return nil, fmt.Errorf("edge from unknown node %q", e.From)
		}
		if _, ok := nodes[e.To]; !ok {
			return nil, fmt.Errorf("edge to unknown node %q", e.To)
		}
		next[e.From] = append(next[e.From], e.To)
		indeg[e.To]++
	}

	var ready []string
	for _, n := range names {
		if indeg[n] == 0 {
			ready = append(ready, n)
		}
	}
	order := make([]string, 0, len(names))
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		order = append(order, n)
		for _, m := range next[n] {
			indeg[m]--
			if indeg[m] == 0 {
				ready = append(ready, m)
			}
		}
	}
	if len(order) != len(names) {
		return nil, fmt.Errorf("workflow graph has a cycle")
	}
	return order, nil
}

func nodeByName(def *Definition, name string) Node {
	for _, n := range def.Nodes {
		if n.Name == name {
			return n
		}
	}
	return Node{Name: name}
}

// --- branch execution (spec decision 3, ticket 04) ---

// entryNode returns the single in-degree-0 node (Validate guarantees one).
func entryNode(def *Definition) string {
	indeg := indegree(def)
	for _, n := range def.Nodes {
		if indeg[n.Name] == 0 {
			return n.Name
		}
	}
	return ""
}

// outEdges returns def's edges leaving node, in declaration order.
func outEdges(def *Definition, node string) []Edge {
	var out []Edge
	for _, e := range def.Edges {
		if e.From == node {
			out = append(out, e)
		}
	}
	return out
}

// selectEdge picks the next node from a node's outgoing edges: a plain edge
// walks on; otherwise the first true deterministic `when` wins; otherwise an
// llm_route LLM pick; otherwise default; otherwise no match (runtime failure).
func (r *Runner) selectEdge(ctx context.Context, edges []Edge, sc refScope) (string, error) {
	var plains, deterministics, llmRoutes []Edge
	var defaultEdge *Edge
	for i := range edges {
		switch e := edges[i]; e.When {
		case "":
			plains = append(plains, e)
		case WhenDefault:
			defaultEdge = &edges[i]
		case WhenLLMRoute:
			llmRoutes = append(llmRoutes, e)
		default:
			deterministics = append(deterministics, e)
		}
	}
	if len(plains) > 0 {
		return plains[0].To, nil
	}
	for _, e := range deterministics {
		ok, err := evalExpr(e.When, sc)
		if err != nil {
			return "", fmt.Errorf("edge %s→%s: %w", e.From, e.To, err)
		}
		if ok {
			return e.To, nil
		}
	}
	if len(llmRoutes) > 0 {
		if chosen, ok := r.llmRoute(ctx, llmRoutes); ok {
			return chosen, nil
		}
		// non-candidate reply → fall through to default (or no-match failure)
	}
	if defaultEdge != nil {
		return defaultEdge.To, nil
	}
	return "", fmt.Errorf("no matching edge from %q", edges[0].From)
}

// llmRoute asks the LLM to choose one candidate edge by node name. Returns
// ok=false for a non-candidate reply (caller falls through to default).
func (r *Runner) llmRoute(ctx context.Context, candidates []Edge) (string, bool) {
	var b strings.Builder
	b.WriteString("Pick exactly one option. Reply with only its node name.\n")
	for _, e := range candidates {
		desc := e.Description
		if desc == "" {
			desc = e.To
		}
		fmt.Fprintf(&b, "- %s: %s\n", e.To, desc)
	}
	content, err := r.llm.Call(ctx, b.String())
	if err != nil {
		return "", false
	}
	choice := strings.TrimSpace(content)
	var env struct {
		Choice string `json:"choice"`
	}
	if json.Unmarshal([]byte(choice), &env) == nil && env.Choice != "" {
		choice = env.Choice
	}
	for _, e := range candidates {
		if e.To == choice {
			return e.To, true
		}
	}
	return "", false
}

// evalExpr evaluates a deterministic when expression "${ref} OP literal".
var exprPattern = regexp.MustCompile(`^\$\{([^}]+)\}\s*(>=|<=|==|!=|>|<)\s*(.+)$`)

func evalExpr(expr string, sc refScope) (bool, error) {
	expr = strings.TrimSpace(expr)
	// Combine multiple conditions with && (and) or || (or). The edge language
	// is single-level: no parentheses, and a literal must not contain the
	// operator string (use separate edges + default for richer logic — spec
	// decision 3). && binds tighter than ||, so "a && b || c" reads as
	// "(a && b) || c".
	if parts := splitTop(expr, "&&"); len(parts) > 1 {
		for _, p := range parts {
			ok, err := evalExpr(p, sc)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	}
	if parts := splitTop(expr, "||"); len(parts) > 1 {
		for _, p := range parts {
			ok, err := evalExpr(p, sc)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}
	m := exprPattern.FindStringSubmatch(expr)
	if m == nil {
		return false, fmt.Errorf("bad when expression %q", expr)
	}
	ref, op, literal := m[1], m[2], strings.TrimSpace(m[3])
	val, ok := sc.lookup(ref)
	if !ok {
		return false, fmt.Errorf("unresolved reference ${%s}", ref)
	}
	return compare(val, op, literal)
}

// splitTop splits expr on a combine operator (&& or ||), trimming each part.
// It does not understand parentheses or quoted operators — the edge language
// is deliberately single-level (see the evalExpr note).
func splitTop(expr, op string) []string {
	parts := strings.Split(expr, op)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// compare applies op to val and a literal. Numbers compare numerically; == / !=
// also work on strings.
func compare(val any, op, literal string) (bool, error) {
	vf, verr := toFloat(val)
	lf, lerr := toFloat(literal)
	if verr == nil && lerr == nil {
		switch op {
		case ">":
			return vf > lf, nil
		case "<":
			return vf < lf, nil
		case ">=":
			return vf >= lf, nil
		case "<=":
			return vf <= lf, nil
		case "==":
			return vf == lf, nil
		case "!=":
			return vf != lf, nil
		}
		return false, fmt.Errorf("unknown operator %q", op)
	}
	switch op {
	case "==":
		return fmt.Sprint(val) == literal, nil
	case "!=":
		return fmt.Sprint(val) != literal, nil
	}
	return false, fmt.Errorf("operator %q needs numbers, got %T and %q", op, val, literal)
}

func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case string:
		return strconv.ParseFloat(x, 64)
	}
	return 0, fmt.Errorf("not a number: %T", v)
}
