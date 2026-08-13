package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
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

// RunStore is the persistence seam. *store.DBStore implements it; the runner
// holds the interface so its graph/contract logic is testable without a DB.
type RunStore interface {
	CreateWorkflowRun(ctx context.Context, id, defID string, version int, input map[string]any, sessionID, owner string) error
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
	store RunStore
	newID func() string
}

// NewRunner wires the leaf callers + persistence. Tests inject fakes here;
// production callers use ProviderLLMCaller / RegistryToolCaller from adapters.go.
func NewRunner(llm LLMCaller, tool ToolCaller, rs RunStore) *Runner {
	return &Runner{llm: llm, tool: tool, store: rs, newID: newRunID}
}

func newRunID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "wf_" + hex.EncodeToString(b[:])
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

// Run executes def's nodes in topological order against input. A node failure
// stops the run: the result carries status=failed, the failing node + original
// error, and a snapshot of every node attempted this run (spec decision 5). The
// snapshot is assembled from the persisted node-output rows, not held in memory
// across the run (spec decision 10 — no redundant copy).
func (r *Runner) Run(ctx context.Context, def *Definition, input map[string]any) (*ExecutionResult, error) {
	if input == nil {
		input = map[string]any{}
	}
	order, err := topoOrder(def)
	if err != nil {
		return nil, err
	}

	runID := r.newID()
	if err := r.store.CreateWorkflowRun(ctx, runID, def.ID, def.Version, input, "", ""); err != nil {
		return nil, fmt.Errorf("create workflow run: %w", err)
	}

	sc := refScope{input: input, outputs: map[string]map[string]any{}}

	for _, name := range order {
		node := nodeByName(def, name)
		out, execErr := r.execNode(ctx, node, sc)

		if execErr != nil {
			msg := execErr.Error()
			if perr := r.store.AppendWorkflowNodeOutput(ctx, runID, name, 1, string(StatusFailed), nil, msg); perr != nil {
				return nil, fmt.Errorf("persist failed node %s: %w", name, perr)
			}
			failMsg := fmt.Sprintf("node %s: %s", name, msg)
			if ferr := r.store.FinalizeWorkflowRun(ctx, runID, string(StatusFailed), failMsg); ferr != nil {
				return nil, fmt.Errorf("finalize failed run: %w", ferr)
			}
			snap, lerr := r.loadSnapshot(ctx, runID, def)
			if lerr != nil {
				return nil, fmt.Errorf("load snapshot: %w", lerr)
			}
			return &ExecutionResult{
				RunID:    runID,
				Status:   StatusFailed,
				Error:    &NodeError{Node: name, Message: msg},
				Snapshot: snap,
			}, nil
		}

		if perr := r.store.AppendWorkflowNodeOutput(ctx, runID, name, 1, string(StatusSucceeded), out, ""); perr != nil {
			return nil, fmt.Errorf("persist node %s output: %w", name, perr)
		}
		sc.outputs[name] = out
	}

	if ferr := r.store.FinalizeWorkflowRun(ctx, runID, string(StatusSucceeded), ""); ferr != nil {
		return nil, fmt.Errorf("finalize run: %w", ferr)
	}
	snap, err := r.loadSnapshot(ctx, runID, def)
	if err != nil {
		return nil, fmt.Errorf("load snapshot: %w", err)
	}
	var result map[string]any
	if len(order) > 0 {
		result = sc.outputs[order[len(order)-1]]
	}
	return &ExecutionResult{
		RunID:    runID,
		Status:   StatusSucceeded,
		Result:   result,
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
			Name:   name,
			Kind:   nodeByName(def, name).Kind,
			Status: Status(rw.Status),
			Output: rw.Output,
			Error:  rw.Error,
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
