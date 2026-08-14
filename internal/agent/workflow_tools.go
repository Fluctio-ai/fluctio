package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/workflow"
)

// WorkflowsDir returns this agent's workflow YAML directory (home + "workflows").
// The web API uses it to list / read / write workflow files; loadAgentWorkflows
// reads the same directory at boot.
func (a *Agent) WorkflowsDir() string {
	return filepath.Join(a.homePath, "workflows")
}

// ReloadWorkflows re-reads this agent's workflow directory and rebuilds its
// workflow Service, so a saved or edited YAML takes effect without a daemon
// restart. Called by the save handler after writing the file. A missing dir or
// a non-DBStore backend leaves the existing Service as-is.
func (a *Agent) ReloadWorkflows() {
	dbs, ok := a.dataStore.(*store.DBStore)
	if !ok || dbs == nil {
		return
	}
	defs, err := workflow.LoadDir(a.WorkflowsDir())
	if err != nil {
		return
	}
	a.SetWorkflowService(workflow.NewService(defs, dbs))
}

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
	return a.RunWorkflowStream(ctx, id, input, owner, session, nil)
}

// RunWorkflowStream is RunWorkflow with a progress-event sink (M4 node-level
// streaming). The sink receives NodeStart / NodeComplete / Done events as the
// runner executes; nil reproduces RunWorkflow exactly.
func (a *Agent) RunWorkflowStream(ctx context.Context, id string, input map[string]any, owner, session string, sink func(workflow.RunEvent)) (*workflow.ExecutionResult, error) {
	if a.workflowSvc == nil {
		return nil, fmt.Errorf("agent %q has no workflows", a.name)
	}
	llm, tool, code := a.workflowLeaves(ctx, id, session)
	return a.workflowSvc.RunWorkflowStream(ctx, id, input, owner, session, llm, tool, code, sink)
}

// ResumeWorkflowStream resumes one of this agent's waiting runs (M6) with the
// user's form answers. session is the run's original session (reused so code
// nodes keep the same sandbox context); owner is the resuming caller.
func (a *Agent) ResumeWorkflowStream(ctx context.Context, id, runID, formNode string, formValues map[string]any, owner, session string, sink func(workflow.RunEvent)) (*workflow.ExecutionResult, error) {
	if a.workflowSvc == nil {
		return nil, fmt.Errorf("agent %q has no workflows", a.name)
	}
	llm, tool, code := a.workflowLeaves(ctx, id, session)
	return a.workflowSvc.ResumeWorkflowStream(ctx, id, runID, formNode, formValues, owner, llm, tool, code, sink)
}

// workflowLeaves builds the per-run leaf capabilities off this agent's
// current provider + registry (so a provider hot-reload takes effect on the
// next call): the LLM + tool callers every run needs, plus a sandbox code
// caller only when the named workflow actually has a code node — otherwise a
// tool/llm-only run would needlessly start a container.
func (a *Agent) workflowLeaves(ctx context.Context, id, session string) (workflow.LLMCaller, workflow.ToolCaller, workflow.CodeCaller) {
	llm := &workflow.ProviderLLMCaller{P: a.provider, Model: a.model, MaxTokens: a.maxTokens, Temp: a.temperature}
	tool := &workflow.RegistryToolCaller{R: a.registry}
	var code workflow.CodeCaller
	if def, ok := a.workflowSvc.Definition(id); ok && usesCodeNode(def) {
		code = a.workflowCodeCaller(ctx, session)
	}
	return llm, tool, code
}

// workflowCodeCaller builds a SandboxCodeCaller over this agent's per-session
// sandbox executor when one is available, so code nodes run in the same
// isolated sandbox as the agent's exec tool. Returns nil when no sandbox pool
// is wired or the executor can't be obtained — a code node then fails at
// runtime with a clear "no code caller" error rather than failing workflow
// boot. Workflows are not project-scoped, so the project slot is empty.
func (a *Agent) workflowCodeCaller(ctx context.Context, session string) workflow.CodeCaller {
	if a.sandboxPool == nil {
		return nil
	}
	ex, err := a.sandboxPool.Get(ctx, a.name, "", session)
	if err != nil || ex == nil {
		return nil
	}
	return &workflow.SandboxCodeCaller{Ex: ex}
}

// usesCodeNode reports whether def contains any code node.
func usesCodeNode(def *workflow.Definition) bool {
	for _, n := range def.Nodes {
		if n.Kind == workflow.KindCode {
			return true
		}
	}
	return false
}

// registerWorkflowTools adds one tool per workflow definition, plus the shared
// workflow_resume tool (M6): a run that parks on a form node returns
// status=waiting with the pending form's schema, and the loop's LLM relays
// that form to the user in natural language, then submits their answers
// through workflow_resume to continue the run — the conversation itself is
// the form UI on IM.
func (a *Agent) registerWorkflowTools() {
	svc := a.workflowSvc
	reg := a.registry
	for id, def := range svc.Definitions() {
		desc := def.Description
		if desc == "" {
			desc = fmt.Sprintf("Run the %q workflow — a fixed, pre-orchestrated multi-step flow. A result with status=waiting carries pending_form: relay its fields to the user, collect answers, submit them via workflow_resume.", id)
		}
		schema := svc.ToolSchema(def)
		reg.RegisterFrom(id, desc, schema, func(ctx context.Context, raw json.RawMessage) (string, error) {
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
		}, tools.SourceWorkflow)
	}
	reg.RegisterFrom("workflow_resume",
		"Resume a workflow run that is paused on a form node (status=waiting with pending_form). Relay the form's fields to the user in natural language, then call this tool with their answers: run_id from the waiting result, form = {field: value} for exactly the fields the user answered (omit the rest). Do NOT invent answers. The resumed result is terminal or waiting on the next form.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"run_id": map[string]any{"type": "string", "description": "run_id of the waiting run (from the workflow tool's waiting result)"},
				"form":   map[string]any{"type": "object", "description": "answers keyed by the pending form's field names"},
			},
			"required": []any{"run_id", "form"},
		},
		func(ctx context.Context, raw json.RawMessage) (string, error) {
			var req struct {
				RunID string         `json:"run_id"`
				Form  map[string]any `json:"form"`
			}
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &req); err != nil || req.RunID == "" {
					return "", fmt.Errorf("workflow_resume: bad args (want run_id + form)")
				}
			}
			dbs, ok := a.dataStore.(*store.DBStore)
			if !ok || dbs == nil {
				return "", fmt.Errorf("workflow_resume: store unavailable")
			}
			row, err := dbs.GetWorkflowRunRow(ctx, req.RunID)
			if err != nil {
				return "", fmt.Errorf("workflow_resume: run %s not found", req.RunID)
			}
			if row.Status != string(workflow.StatusWaiting) {
				return "", fmt.Errorf("workflow_resume: run %s is %s, not waiting on a form", req.RunID, row.Status)
			}
			if req.Form == nil {
				req.Form = map[string]any{}
			}
			res, rerr := a.ResumeWorkflowStream(ctx, row.DefID, row.ID, row.PendingFormNode, req.Form, reg.EffectiveUserID(), row.SessionID, nil)
			if rerr != nil {
				return "", fmt.Errorf("workflow_resume: %w", rerr)
			}
			b, mErr := json.Marshal(res)
			if mErr != nil {
				return fmt.Sprintf("workflow %s resumed (status=%s) but its result could not be encoded", row.DefID, res.Status), nil
			}
			return string(b), nil
		}, tools.SourceWorkflow)
}
