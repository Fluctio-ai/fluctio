// Package workflow implements the FastClaw workflow subsystem — a fixed,
// pre-orchestrated multi-step pipeline driven by its own runner (spec
// decision 1, ADR 0001). This file holds the value types shared across the
// loader, runner, and persistence seams.
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

// Definition is a versioned workflow. It is consumed as-is by the runner;
// validation + round-trip live in Seam B (ticket 02). YAML is the single
// source of truth and the definition never enters the DB (spec decision 8).
type Definition struct {
	ID      string    `yaml:"id"`
	Version int       `yaml:"version"`
	Input   InputSpec `yaml:"input"`
	Nodes   []Node    `yaml:"nodes"`
	Edges   []Edge    `yaml:"edges"`
}

// InputSpec declares the entry contract. ${input.*} references resolve against
// the run input object (spec decision 6).
type InputSpec struct {
	Schema map[string]any `yaml:"schema"`
}

// Node is one step. A tool node calls one registered tool; an LLM node makes a
// bare provider call. References in Input/Prompt resolve against ${input.*}
// and upstream ${node.*} outputs (spec decision 4). Output is the node's
// declared output schema (used by Seam B validation; the runner parses the
// leaf's raw return against it).
type Node struct {
	Name   string         `yaml:"name"`
	Kind   NodeKind       `yaml:"kind"`
	Tool   string         `yaml:"tool"`
	Input  map[string]any `yaml:"input"`
	Prompt string         `yaml:"prompt"`
	Output map[string]any `yaml:"output"`
}

// Edge is a linear from→to. The tracer bullet only exercises linear chains;
// the struct carries future branch conditions (ticket 04) without reshaping.
type Edge struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
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
