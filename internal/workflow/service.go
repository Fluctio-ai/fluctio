package workflow

import (
	"context"
	"fmt"
)

// Service is the integration surface between the workflow subsystem and one
// agent (spec decision 7, ownership model): it holds the workflows loaded from
// that agent's own directory and runs one on demand. Visibility is by ownership
// — every definition here belongs to this agent — so there is no cross-agent
// ACL list. The per-run leaf capabilities (LLM + tool callers) are injected at
// RunWorkflow time so the run uses the agent's current provider/registry
// (hot-reload-safe).
//
// Agent boot builds one Service per agent (LoadDir over the agent's workflows
// directory), registers a tool per definition, and the manual-trigger API +
// agent-loop LLM trigger sit on top of it. This file is the testable core.
type Service struct {
	defs        map[string]*Definition
	store       RunStore
	concurrency *ConcurrencyManager
}

// NewService builds a Service over the given definitions + persistence seam.
// defs is keyed by definition id (LoadDir / LoadFile's output shape). store may
// be nil only when the caller never invokes RunWorkflow (e.g. schema-only use).
func NewService(defs map[string]*Definition, store RunStore) *Service {
	if defs == nil {
		defs = map[string]*Definition{}
	}
	return &Service{defs: defs, store: store, concurrency: NewConcurrencyManager()}
}

// Definition returns the named workflow, if present.
func (s *Service) Definition(id string) (*Definition, bool) {
	d, ok := s.defs[id]
	return d, ok
}

// Definitions returns every loaded workflow, keyed by id. Agent boot iterates
// this to register a tool per workflow.
func (s *Service) Definitions() map[string]*Definition {
	return s.defs
}

// ToolSchema returns the JSON-schema for the workflow tool's parameters,
// generated from the workflow's input schema (spec decision 7 — "tool schema
// 由 input schema 生成"). With no declared schema it defaults to a bare object
// so the tool is still callable with any input map.
func (s *Service) ToolSchema(def *Definition) map[string]any {
	if len(def.Input.Schema) > 0 {
		return def.Input.Schema
	}
	return map[string]any{"type": "object"}
}

// RunWorkflow executes the named workflow under the given owner/session (spec
// decision 14), using the supplied leaf callers. The runner is built fresh per
// call (spec decision 12 — stateless, isolated runs), reading the agent's
// current provider/registry through llm/tool, so a provider hot-reload takes
// effect on the next call without rebuilding the Service. code is the sandbox
// caller for code nodes; pass nil when no workflow uses them. Validation
// (ticket 02) runs inside Run, so a schema-violating input is rejected here.
func (s *Service) RunWorkflow(ctx context.Context, id string, input map[string]any, owner, session string, llm LLMCaller, tool ToolCaller, code CodeCaller) (*ExecutionResult, error) {
	return s.RunWorkflowStream(ctx, id, input, owner, session, llm, tool, code, nil)
}

// RunWorkflowStream is RunWorkflow with a progress-event sink (M4 node-level
// streaming): the runner emits NodeStart / NodeComplete / Done events as it
// walks the graph. A nil sink behaves exactly like RunWorkflow. The sink is
// called synchronously from the runner goroutine, so a streaming caller must
// forward events without blocking (buffer + flush) to avoid stalling the run.
func (s *Service) RunWorkflowStream(ctx context.Context, id string, input map[string]any, owner, session string, llm LLMCaller, tool ToolCaller, code CodeCaller, sink func(RunEvent)) (*ExecutionResult, error) {
	def, ok := s.defs[id]
	if !ok {
		return nil, fmt.Errorf("unknown workflow %q", id)
	}
	// Apply the workflow's concurrency policy (spec decision 13): serial may
	// block until a prior run finishes; cancel_previous may cancel one. allow
	// is a no-op. release runs after the run completes.
	ctx, release := s.concurrency.Acquire(ctx, id, def.Concurrency)
	defer release()
	opts := []RunOption{WithOwner(owner), WithSession(session)}
	if sink != nil {
		opts = append(opts, WithEventSink(sink))
	}
	return NewRunner(llm, tool, s.store, WithCodeCaller(code), WithHTTPCaller(NetHTTPCaller{})).Run(ctx, def, input, opts...)
}
