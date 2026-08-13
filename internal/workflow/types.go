// Package workflow implements the FastClaw workflow subsystem — a fixed,
// pre-orchestrated multi-step pipeline driven by its own runner (spec
// decision 1, ADR 0001). This file holds the value types shared across the
// loader, runner, validation, and persistence seams.
package workflow

// Status is the state of a run or a node execution.
type Status string

const (
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

// NodeKind labels how a node executes. The tracer bullet ships tool + llm;
// code (sandbox) and branch nodes arrive in later tickets.
type NodeKind string

const (
	KindTool NodeKind = "tool"
	KindLLM  NodeKind = "llm"
)

// Definition is a versioned workflow. YAML is the single source of truth and
// the definition never enters the DB (spec decision 8). Optional fields carry
// `,omitempty` so parse → marshal → parse round-trips with nil/zero collapsing
// to the same shape (spec decision 8 round-trip hard constraint; ticket 02).
type Definition struct {
	ID      string    `yaml:"id,omitempty"`
	Version int       `yaml:"version"`
	Input   InputSpec `yaml:"input,omitempty"`
	Nodes   []Node    `yaml:"nodes"`
	Edges   []Edge    `yaml:"edges,omitempty"`
}

// InputSpec declares the entry contract. ${input.*} references resolve against
// the run input object (spec decision 6).
type InputSpec struct {
	Schema map[string]any `yaml:"schema,omitempty"`
}

// Node is one step. A tool node calls one registered tool; an LLM node makes a
// bare provider call. References in Input/Prompt resolve against ${input.*}
// and upstream ${node.*} outputs (spec decision 4). Output is the node's
// declared output schema — when absent, field references into this node are
// trusted (the leaf's raw return is parsed at runtime).
type Node struct {
	Name   string         `yaml:"name"`
	Kind   NodeKind       `yaml:"kind"`
	Tool   string         `yaml:"tool,omitempty"`
	Input  map[string]any `yaml:"input,omitempty"`
	Prompt string         `yaml:"prompt,omitempty"`
	Output map[string]any `yaml:"output,omitempty"`
}

// RouteKind is the branch language on an edge (spec decision 3). The tracer
// bullet only validates llm_route/default; deterministic-expression routing
// and branch execution arrive in ticket 04.
type RouteKind string

const (
	RoutePlain    RouteKind = ""         // plain linear edge
	RouteDefault  RouteKind = "default"  // LLM-router fallback
	RouteLLMRoute RouteKind = "llm_route" // LLM picks among siblings
)

// Edge is a from→to connection. Route is the branch language (spec decision
// 3). Ticket 02 only *validates* that llm_route edges have a sibling default;
// branch *execution* is 04.
type Edge struct {
	From  string    `yaml:"from"`
	To    string    `yaml:"to"`
	Route RouteKind `yaml:"route,omitempty"`
}

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
	Name   string         `json:"name"`
	Kind   NodeKind       `json:"kind"`
	Status Status         `json:"status"`
	Output map[string]any `json:"output,omitempty"`
	Error  string         `json:"error,omitempty"`
}
