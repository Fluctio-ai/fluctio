package workflow

import (
	"context"
	"fmt"
	"slices"
)

// Service is the integration surface between the workflow subsystem and the
// rest of the platform (spec decision 7): it holds the loaded definitions and
// the runner, answers the per-agent ACL (which workflows an agent can see as a
// tool), generates each workflow's tool schema from its input schema, and runs
// a workflow on demand. Agent boot, the manual-trigger API, and the agent-loop
// LLM trigger all sit on top of this — they're wired in ticket 05's integration
// step; this file is the testable core.
type Service struct {
	defs   map[string]*Definition
	runner *Runner
}

// NewService builds a Service over the given definitions + runner. defs is keyed
// by definition id (LoadDir / LoadFile's output shape).
func NewService(defs map[string]*Definition, runner *Runner) *Service {
	if defs == nil {
		defs = map[string]*Definition{}
	}
	return &Service{defs: defs, runner: runner}
}

// Definition returns the named workflow, if present.
func (s *Service) Definition(id string) (*Definition, bool) {
	d, ok := s.defs[id]
	return d, ok
}

// Definitions returns every loaded workflow, keyed by id. Agent boot iterates
// this to register a tool per visible workflow.
func (s *Service) Definitions() map[string]*Definition {
	return s.defs
}

// VisibleTo implements the per-agent ACL (spec decision 7): a workflow is
// visible only to agents in its Agents whitelist. A workflow with no whitelist
// is default-private (visible to no one) — enrollment is explicit, so a
// half-configured workflow is never accidentally callable.
func (s *Service) VisibleTo(def *Definition, agentID string) bool {
	return slices.Contains(def.Agents, agentID)
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
// decision 14). It does NOT re-check the ACL — visibility is enforced by the
// caller (agent registration filters by VisibleTo; the manual API gates on the
// caller's identity) — this runs whatever it's handed. Validation (ticket 02)
// runs inside Run, so a schema-violating input is rejected here.
func (s *Service) RunWorkflow(ctx context.Context, id string, input map[string]any, owner, session string) (*ExecutionResult, error) {
	def, ok := s.defs[id]
	if !ok {
		return nil, fmt.Errorf("unknown workflow %q", id)
	}
	return s.runner.Run(ctx, def, input, WithOwner(owner), WithSession(session))
}
