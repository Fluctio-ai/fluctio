package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
	"github.com/fluctio-ai/fluctio/internal/cron"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/workflow"
)

// workflowScheduleLoc is the timezone workflow schedules are interpreted in —
// spec decision 16 mandates UTC+8. Mirrors workflowCronLoc in setup and the
// gateway scheduler's own copy (kept separate per the minimal-change rule).
var workflowScheduleLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.UTC
	}
	return loc
}()

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
// definition. Passing nil is a no-op. A zero-definition service still registers
// the shared authoring tools (workflow_list/get/save, workflow_resume) — that
// is how a fresh agent gets them; per-workflow tools then appear as the first
// save reloads the service. Safe to call repeatedly — tool registration
// overwrites by name, so a reload re-freshens the set.
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
		}, tools.SourceWorkflowSys)

	// Workflow authoring tools: let the loop's LLM create / read / update the
	// agent's workflow YAMLs from conversation, so a workflow can be written
	// and iterated without touching the editor. save is an upsert — a new id
	// creates, an existing id edits and bumps the version, and the change takes
	// effect immediately (ReloadWorkflows). get must be called before editing
	// so existing nodes/edges are preserved, never regenerated from memory.
	reg.RegisterFrom("workflow_list",
		"List this agent's workflows (id + version + description). Call before workflow_get / workflow_save to see what exists.",
		map[string]any{"type": "object", "properties": map[string]any{}},
		func(ctx context.Context, _ json.RawMessage) (string, error) {
			if a.workflowSvc == nil {
				return "", fmt.Errorf("workflow_list: agent has no workflows")
			}
			type row struct {
				ID          string `json:"id"`
				Version     int    `json:"version"`
				Description string `json:"description"`
			}
			rows := make([]row, 0, len(a.workflowSvc.Definitions()))
			for id, def := range a.workflowSvc.Definitions() {
				rows = append(rows, row{ID: id, Version: def.Version, Description: def.Description})
			}
			b, _ := json.Marshal(rows)
			return string(b), nil
		}, tools.SourceWorkflowSys)

	reg.RegisterFrom("workflow_get",
		"Read a workflow's full YAML by id. Always read before editing so you preserve existing nodes/edges — never rewrite a workflow from memory.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "workflow id (the YAML filename key)"},
			},
			"required": []any{"id"},
		},
		func(ctx context.Context, raw json.RawMessage) (string, error) {
			var req struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &req); err != nil || req.ID == "" {
				return "", fmt.Errorf("workflow_get: bad args (want id)")
			}
			b, err := os.ReadFile(filepath.Join(a.WorkflowsDir(), req.ID+".yaml"))
			if err != nil {
				return "", fmt.Errorf("workflow_get: %w", err)
			}
			return string(b), nil
		}, tools.SourceWorkflowSys)

	reg.RegisterFrom("workflow_save",
		"Create or update a workflow (upsert). id is the filename key; yaml is the full workflow definition. A new id creates, an existing id edits and bumps the version; the change applies immediately. YAML shape: nodes are kind tool/llm/code/form; references use ${input.x} (run input) or ${node.field} (a node's output); edges connect nodes as {from, to, when?}. Invalid YAML (unknown node refs, missing entry, bad kind) is rejected with a message — fix and retry.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":   map[string]any{"type": "string", "description": "workflow id; becomes the YAML filename"},
				"yaml": map[string]any{"type": "string", "description": "complete workflow definition (version, nodes, edges, input?, output?)"},
			},
			"required": []any{"id", "yaml"},
		},
		func(ctx context.Context, raw json.RawMessage) (string, error) {
			var req struct {
				ID   string `json:"id"`
				YAML string `json:"yaml"`
			}
			if err := json.Unmarshal(raw, &req); err != nil || req.ID == "" || req.YAML == "" {
				return "", fmt.Errorf("workflow_save: bad args (want id + yaml)")
			}
			if err := workflow.ValidateID(req.ID); err != nil {
				return "", fmt.Errorf("workflow_save: %w", err)
			}
			def, err := workflow.Parse(req.ID, []byte(req.YAML))
			if err != nil {
				return "", fmt.Errorf("workflow_save: parse: %w", err)
			}
			path := filepath.Join(a.WorkflowsDir(), req.ID+".yaml")
			version := 1
			if old, err := workflow.LoadFile(path); err == nil {
				version = old.Version + 1
			}
			def.Version = version
			if err := workflow.Validate(def, nil); err != nil {
				return "", fmt.Errorf("workflow_save: validation: %w", err)
			}
			out, err := workflow.Marshal(def)
			if err != nil {
				return "", fmt.Errorf("workflow_save: marshal: %w", err)
			}
			if err := os.MkdirAll(a.WorkflowsDir(), 0o755); err != nil {
				return "", fmt.Errorf("workflow_save: %w", err)
			}
			if err := os.WriteFile(path, out, 0o644); err != nil {
				return "", fmt.Errorf("workflow_save: %w", err)
			}
			a.ReloadWorkflows()
			return fmt.Sprintf("workflow %s saved (version %d); now available", req.ID, version), nil
		}, tools.SourceWorkflowSys)

	// Schedule tools (spec decision 16's scheduler is the executor; these let
	// the loop's LLM manage its agent's schedules from conversation instead of
	// forcing a dashboard trip). Owner is stamped with the agent's owner user —
	// the scheduler resolves UserSpaceForCtx(owner) then AgentByID, so a
	// per-turn IM chatter id would resolve a userspace that doesn't contain
	// this agent and the schedule would never fire.
	reg.RegisterFrom("workflow_schedule_list",
		"List this agent's workflow schedules (id, workflow, cron, input, enabled, next_run, last_run). A schedule fires its workflow automatically on its cron expression (UTC+8).",
		map[string]any{"type": "object", "properties": map[string]any{}},
		func(ctx context.Context, _ json.RawMessage) (string, error) {
			dbs, ok := a.dataStore.(*store.DBStore)
			if !ok || dbs == nil {
				return "", fmt.Errorf("workflow_schedule_list: store unavailable")
			}
			rows, err := dbs.ListWorkflowSchedules(ctx, a.ID())
			if err != nil {
				return "", fmt.Errorf("workflow_schedule_list: %w", err)
			}
			type schedRow struct {
				ID       string         `json:"id"`
				Workflow string         `json:"workflow"`
				Cron     string         `json:"cron"`
				Input    map[string]any `json:"input"`
				Enabled  bool           `json:"enabled"`
				NextRun  string         `json:"next_run"`
				LastRun  string         `json:"last_run,omitempty"`
			}
			out := make([]schedRow, 0, len(rows))
			for _, r := range rows {
				out = append(out, schedRow{ID: r.ID, Workflow: r.WorkflowID, Cron: r.CronExpr, Input: r.Input, Enabled: r.Enabled, NextRun: r.NextRun, LastRun: r.LastRun})
			}
			b, _ := json.Marshal(out)
			return string(b), nil
		}, tools.SourceWorkflowSys)

	reg.RegisterFrom("workflow_schedule_create",
		"Create a cron schedule that fires one of this agent's workflows automatically. cron is a 5-field expression (\"30 7 * * *\" = daily 07:30) interpreted in UTC+8. input is the fixed entry input every fire runs with — read the workflow's input schema via workflow_get first and include every required field. Returns the created schedule including next_run.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"workflow": map[string]any{"type": "string", "description": "workflow id (from workflow_list)"},
				"cron":     map[string]any{"type": "string", "description": "5-field cron expression (min hour dom mon dow), UTC+8"},
				"input":    map[string]any{"type": "object", "description": "fixed entry input for every fire (defaults to {})"},
			},
			"required": []any{"workflow", "cron"},
		},
		func(ctx context.Context, raw json.RawMessage) (string, error) {
			var req struct {
				Workflow string         `json:"workflow"`
				Cron     string         `json:"cron"`
				Input    map[string]any `json:"input"`
			}
			if err := json.Unmarshal(raw, &req); err != nil || req.Workflow == "" || req.Cron == "" {
				return "", fmt.Errorf("workflow_schedule_create: bad args (want workflow + cron)")
			}
			if len(strings.Fields(req.Cron)) != 5 {
				return "", fmt.Errorf("workflow_schedule_create: cron %q must be 5 space-separated fields (min hour dom mon dow), UTC+8", req.Cron)
			}
			dbs, ok := a.dataStore.(*store.DBStore)
			if !ok || dbs == nil {
				return "", fmt.Errorf("workflow_schedule_create: store unavailable")
			}
			def, ok := svc.Definition(req.Workflow)
			if !ok {
				return "", fmt.Errorf("workflow_schedule_create: unknown workflow %q (workflow_list to see ids)", req.Workflow)
			}
			if req.Input == nil {
				req.Input = map[string]any{}
			}
			// Catch a missing required entry field at creation time rather than
			// on the first silent 7:30am fire failure.
			if err := workflow.Validate(def, req.Input); err != nil {
				return "", fmt.Errorf("workflow_schedule_create: input rejected: %w", err)
			}
			owner := a.OwnerUserID()
			if owner == "" {
				owner = reg.EffectiveUserID()
			}
			s := store.WorkflowScheduleRow{
				ID:          fmt.Sprintf("wfs_%d", time.Now().UnixNano()),
				AgentID:     a.ID(),
				WorkflowID:  req.Workflow,
				OwnerUserID: owner,
				CronExpr:    req.Cron,
				Input:       req.Input,
				Enabled:     true,
				NextRun:     cron.NextOccurrenceIn(req.Cron, time.Now(), workflowScheduleLoc).UTC().Format(time.RFC3339),
			}
			if err := dbs.CreateWorkflowSchedule(ctx, s); err != nil {
				return "", fmt.Errorf("workflow_schedule_create: %w", err)
			}
			b, _ := json.Marshal(map[string]any{"id": s.ID, "workflow": s.WorkflowID, "cron": s.CronExpr, "input": s.Input, "enabled": true, "next_run": s.NextRun})
			return string(b), nil
		}, tools.SourceWorkflowSys)

	reg.RegisterFrom("workflow_schedule_delete",
		"Delete one of this agent's workflow schedules by id (from workflow_schedule_list). The workflow itself is untouched; deleting a schedule is how you stop its automatic fires.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "schedule id from workflow_schedule_list"},
			},
			"required": []any{"id"},
		},
		func(ctx context.Context, raw json.RawMessage) (string, error) {
			var req struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &req); err != nil || req.ID == "" {
				return "", fmt.Errorf("workflow_schedule_delete: bad args (want id)")
			}
			dbs, ok := a.dataStore.(*store.DBStore)
			if !ok || dbs == nil {
				return "", fmt.Errorf("workflow_schedule_delete: store unavailable")
			}
			if err := dbs.DeleteWorkflowSchedule(ctx, req.ID); err != nil {
				return "", fmt.Errorf("workflow_schedule_delete: %w", err)
			}
			return fmt.Sprintf("schedule %s deleted", req.ID), nil
		}, tools.SourceWorkflowSys)
}
