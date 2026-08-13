package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fluctio-ai/fluctio/internal/workflow"
)

// SetWorkflowService wires this agent's own workflows (YAMLs the gateway loads
// from its home/workflows directory at boot) and registers one tool per
// definition. Passing nil is a no-op. Safe to call repeatedly — tool
// registration overwrites by name, so a reload re-freshens the set.
func (a *Agent) SetWorkflowService(svc *workflow.Service) {
	a.workflowSvc = svc
	if svc == nil {
		return
	}
	a.registerWorkflowTools()
}

// RunWorkflow runs one of this agent's own workflows directly, using this
// agent's current provider + tool registry as the runner's leaf capabilities
// (so a provider hot-reload takes effect on the next call). It is the shared
// engine behind both the per-workflow tool the LLM drives inside a loop and
// the manual/cron trigger API (spec decision 14). owner is the calling user;
// session is the agent session an LLM-triggered run hangs off ("" for none).
// Returns an error when this agent has no workflows configured.
func (a *Agent) RunWorkflow(ctx context.Context, id string, input map[string]any, owner, session string) (*workflow.ExecutionResult, error) {
	if a.workflowSvc == nil {
		return nil, fmt.Errorf("agent %q has no workflows", a.name)
	}
	llm := &workflow.ProviderLLMCaller{P: a.provider, Model: a.model, MaxTokens: a.maxTokens, Temp: a.temperature}
	tool := &workflow.RegistryToolCaller{R: a.registry}
	return a.workflowSvc.RunWorkflow(ctx, id, input, owner, session, llm, tool)
}

// registerWorkflowTools adds one tool per workflow definition. The closure
// reads the in-flight turn's identity (owner / session) off the registry at
// call time, then delegates to RunWorkflow — so the loop-driven path and the
// manual-trigger path run the exact same engine. The ExecutionResult is
// returned to the loop as JSON.
func (a *Agent) registerWorkflowTools() {
	svc := a.workflowSvc
	reg := a.registry
	for id, def := range svc.Definitions() {
		desc := def.Description
		if desc == "" {
			desc = fmt.Sprintf("Run the %q workflow — a fixed, pre-orchestrated multi-step flow.", id)
		}
		schema := svc.ToolSchema(def)
		reg.Register(id, desc, schema, func(ctx context.Context, raw json.RawMessage) (string, error) {
			var input map[string]any
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &input); err != nil {
					return "", fmt.Errorf("workflow %s: bad input JSON: %w", id, err)
				}
			}
			res, err := a.RunWorkflow(ctx, id, input, reg.EffectiveUserID(), reg.SessionID())
			if err != nil {
				return "", fmt.Errorf("workflow %s: %w", id, err)
			}
			b, mErr := json.Marshal(res)
			if mErr != nil {
				return fmt.Sprintf("workflow %s ran (status=%s) but its result could not be encoded", id, res.Status), nil
			}
			return string(b), nil
		})
	}
}
