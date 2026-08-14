// Package workflow implements the FastClaw workflow subsystem — a fixed,
// pre-orchestrated multi-step pipeline driven by its own runner (spec
// decision 1, ADR 0001). This file holds the value types shared across the
// loader, runner, validation, and persistence seams.
package workflow

// Status is the state of a run or a node execution.
type Status string

const (
	StatusRunning           Status = "running"
	StatusSucceeded         Status = "succeeded"
	StatusFailed            Status = "failed"
	StatusNeedsIntervention Status = "needs_intervention" // non-idempotent node failed; resume refused (spec decision 2)
)

// NodeKind labels how a node executes: a tool node calls one registered tool,
// an llm node makes a bare provider call, a code node runs a script in the
// sandbox (spec decision 3).
type NodeKind string

const (
	KindTool      NodeKind = "tool"
	KindLLM       NodeKind = "llm"
	KindCode      NodeKind = "code"
	KindCondition NodeKind = "condition"
)

// SideEffect declares a node's side-effect category (spec decision 2, ADR 0002).
// It governs resume: pure/idempotent nodes can be safely re-run after a
// failure; non-idempotent nodes that failed require manual intervention and
// are not auto-rerun. Nodes without an explicit declaration default to pure.
type SideEffect string

const (
	SideEffectPure         SideEffect = "pure"
	SideEffectIdempotent   SideEffect = "idempotent"
	SideEffectNonIdempotent SideEffect = "non-idempotent"
)

// Concurrency declares how overlapping triggers of the same workflow are
// coordinated (spec decision 13). The default (empty) is allow.
type Concurrency string

const (
	ConcurrencyAllow          Concurrency = ""                 // default: runs overlap freely
	ConcurrencySerial         Concurrency = "serial"           // one at a time, queued
	ConcurrencyCancelPrevious Concurrency = "cancel_previous" // a new trigger cancels the prior inflight run
)

// Definition is a versioned workflow. YAML is the single source of truth and
// the definition never enters the DB (spec decision 8). Optional fields carry
// `,omitempty` so parse → marshal → parse round-trips with nil/zero collapsing
// to the same shape (spec decision 8 round-trip hard constraint; ticket 02).
// Output is an optional workflow-level result map: its values are ${input.*}
// / ${node.*} references the runner resolves on a successful run to produce
// ExecutionResult.Result (without it, Result defaults to the last node's
// output).
type Definition struct {
	ID          string         `yaml:"id,omitempty"`
	Version     int            `yaml:"version"`
	Title       string         `yaml:"title,omitempty"`
	Description string         `yaml:"description,omitempty"`
	Input       InputSpec      `yaml:"input,omitempty"`
	Nodes       []Node         `yaml:"nodes"`
	Edges       []Edge         `yaml:"edges,omitempty"`
	Output      map[string]any `yaml:"output,omitempty"`
	Concurrency Concurrency    `yaml:"concurrency,omitempty"`
}

// InputSpec declares the entry contract. ${input.*} references resolve against
// the run input object (spec decision 6).
type InputSpec struct {
	Schema map[string]any `yaml:"schema,omitempty"`
}

// Node is one step. A tool node calls one registered tool; an LLM node makes a
// bare provider call; a code node runs Code (a script in Language, default
// "python") in the sandbox. References in Input/Prompt resolve against
// ${input.*} and upstream ${node.*} outputs (spec decision 4). Output is the
// node's declared output schema — when absent, field references into this node
// are trusted (the leaf's raw return is parsed at runtime).
type Node struct {
	Name       string         `yaml:"name"`
	Kind       NodeKind       `yaml:"kind"`
	Tool       string         `yaml:"tool,omitempty"`
	Input      map[string]any `yaml:"input,omitempty"`
	Prompt     string         `yaml:"prompt,omitempty"`
	Code       string         `yaml:"code,omitempty"`
	Language   string         `yaml:"lang,omitempty"`
	Output     map[string]any `yaml:"output,omitempty"`
	SideEffect SideEffect     `yaml:"side_effect,omitempty"`
}

// Edge is a from→to connection. When is the branch language (spec decision 3):
//   - ""           plain linear edge (always taken; the node has one such out-edge)
//   - "default"    fallback, taken when no other edge matched
//   - "llm_route"  the runner asks the LLM to pick among sibling llm_route edges
//   - any other    a deterministic expression on upstream outputs, e.g. "${score} > 0.8"
//
// Description is shown to the LLM when When == "llm_route" (ticket 04).
type Edge struct {
	From        string `yaml:"from"`
	To          string `yaml:"to"`
	When        string `yaml:"when,omitempty"`
	Description string `yaml:"desc,omitempty"`
}

// Edge.When sentinel values.
const (
	WhenDefault  = "default"
	WhenLLMRoute = "llm_route"
)

// ExecutionResult is the unified return for every run (spec decision 5): the
// terminal status, the final node's output on success, an error pointing at
// the failing node on failure, and a snapshot of every node executed this run.
// The snapshot is the input to resume + the diagnostics surface (ADR 0002).
type ExecutionResult struct {
	RunID    string                `json:"run_id"`
	Status   Status                `json:"status"`
	Result   map[string]any        `json:"result,omitempty"`
	Error    *NodeError            `json:"error,omitempty"`
	Snapshot map[string]NodeOutput `json:"completed_nodes_snapshot"`
}

// NodeError identifies which node failed and carries the original error text.
type NodeError struct {
	Node    string `json:"node"`
	Message string `json:"message"`
}

// NodeOutput is one node's outcome inside the snapshot.
type NodeOutput struct {
	Name    string         `json:"name"`
	Kind    NodeKind       `json:"kind"`
	Status  Status         `json:"status"`
	Output  map[string]any `json:"output,omitempty"`
	Error   string         `json:"error,omitempty"`
	Attempt int            `json:"attempt,omitempty"`
}
