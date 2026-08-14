package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/fluctio-ai/fluctio/internal/auth"
	"github.com/fluctio-ai/fluctio/internal/cron"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/workflow"
)

// handleWorkflowRun — POST /api/agents/{agentID}/workflows/{wfID}/run
//
// Manually triggers one of the agent's own workflows (spec decision 14: the
// run's owner is the calling user; session is "" for a bare manual run). The
// request body is the workflow's input object (may be empty). This is the
// manual-trigger entry point named by ticket 05 AC3 — the same engine the
// LLM-driven workflow tool uses, just invoked outside a chat turn.
//
// Response: {"ok":true,"result":<ExecutionResult>} on success,
// {"ok":false,"error":"..."} otherwise.
func (s *Server) handleWorkflowRun(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")
	wfID := r.PathValue("wfID")
	ag := s.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}
	var input map[string]any
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil && !errors.Is(err, io.EOF) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad input JSON: " + err.Error()})
		return
	}
	owner := ""
	if ident, ok := auth.FromContext(r.Context()); ok {
		owner = ident.EffectiveUserID()
	}
	res, err := ag.RunWorkflow(r.Context(), wfID, input, owner, "")
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

// handleWorkflowRunStream — POST /api/agents/{agentID}/workflows/{wfID}/run/stream
//
// Manually triggers a workflow and streams node-level progress as SSE (M4):
// one "data:" line per RunEvent (node_start / node_complete / done) as nodes
// execute, then a terminal "result" event carrying the ExecutionResult, then
// the stream closes. A go-level error (no run started) is sent as a final
// "error" event. Body is the workflow input object (may be empty).
func (s *Server) handleWorkflowRunStream(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")
	wfID := r.PathValue("wfID")
	ag := s.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}
	var input map[string]any
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil && !errors.Is(err, io.EOF) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad input JSON: " + err.Error()})
		return
	}
	owner := ""
	if ident, ok := auth.FromContext(r.Context()); ok {
		owner = ident.EffectiveUserID()
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// The sink forwards each event as an SSE "data:" line and flushes so the
	// client sees progress in real time. RunWorkflowStream calls it inline from
	// the runner goroutine, so flush is the only (fast) work done here.
	sink := func(e workflow.RunEvent) {
		if b, err := json.Marshal(e); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	res, err := ag.RunWorkflowStream(r.Context(), wfID, input, owner, "", sink)
	if err != nil {
		// Headers are already SSE at this point; send a terminal error event.
		b, _ := json.Marshal(map[string]any{"type": "error", "error": err.Error()})
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	// Terminal result event carrying the full ExecutionResult, then close.
	b, _ := json.Marshal(map[string]any{"type": "result", "result": res})
	fmt.Fprintf(w, "data: %s\n\n", b)
	if flusher != nil {
		flusher.Flush()
	}
}

// --- M6 form resume (form 人机交互) ---

// parseWorkflowResume is the shared pre-flight for both resume endpoints: the
// agent resolves, the run exists under this workflow, it is in the waiting
// state, and the body carries a "form" object. On failure it has already
// written the JSON error response and returns ok=false.
func (s *Server) parseWorkflowResume(w http.ResponseWriter, r *http.Request) (ag AgentHandle, run store.WorkflowRunRow, form map[string]any, ok bool) {
	ag = s.resolveAgent(r, r.PathValue("agentID"))
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return nil, run, nil, false
	}
	var body struct {
		Form map[string]any `json:"form"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad body (want {\"form\": {...}}): " + err.Error()})
		return nil, run, nil, false
	}
	dbs, ok2 := s.dataStore.(*store.DBStore)
	if !ok2 || dbs == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "workflow store unavailable"})
		return nil, run, nil, false
	}
	run, err := dbs.GetWorkflowRunRow(r.Context(), r.PathValue("runID"))
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "run not found"})
		return nil, run, nil, false
	}
	if run.DefID != r.PathValue("wfID") {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "run not found under this workflow"})
		return nil, run, nil, false
	}
	if run.Status != string(workflow.StatusWaiting) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "run is not waiting for a form (status " + run.Status + ")"})
		return nil, run, nil, false
	}
	if body.Form == nil {
		body.Form = map[string]any{}
	}
	return ag, run, body.Form, true
}

// handleWorkflowRunResume — POST /api/agents/{agentID}/workflows/{wfID}/runs/{runID}/resume
//
// Resumes a waiting run with the user's form answers (M6): the runner
// validates them against the waiting node's schema, records them as that
// node's output, and continues the walk — possibly to another waiting form,
// whose schema rides the returned ExecutionResult.pending_form.
// Response: {"ok":true,"result":<ExecutionResult>}.
func (s *Server) handleWorkflowRunResume(w http.ResponseWriter, r *http.Request) {
	ag, run, form, ok := s.parseWorkflowResume(w, r)
	if !ok {
		return
	}
	owner := ""
	if ident, ok := auth.FromContext(r.Context()); ok {
		owner = ident.EffectiveUserID()
	}
	res, err := ag.ResumeWorkflowStream(r.Context(), r.PathValue("wfID"), run.ID, run.PendingFormNode, form, owner, run.SessionID, nil)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

// handleWorkflowRunResumeStream — POST .../runs/{runID}/resume/stream
//
// ResumeWorkflowStream over SSE, mirroring handleWorkflowRunStream: node
// events as they execute, then a terminal "result" event. This is what the
// run-detail UI calls after the user submits the form, so post-form nodes
// stream exactly like a fresh run.
func (s *Server) handleWorkflowRunResumeStream(w http.ResponseWriter, r *http.Request) {
	ag, run, form, ok := s.parseWorkflowResume(w, r)
	if !ok {
		return
	}
	owner := ""
	if ident, ok := auth.FromContext(r.Context()); ok {
		owner = ident.EffectiveUserID()
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sink := func(e workflow.RunEvent) {
		if b, err := json.Marshal(e); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	res, err := ag.ResumeWorkflowStream(r.Context(), r.PathValue("wfID"), run.ID, run.PendingFormNode, form, owner, run.SessionID, sink)
	if err != nil {
		b, _ := json.Marshal(map[string]any{"type": "error", "error": err.Error()})
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	b, _ := json.Marshal(map[string]any{"type": "result", "result": res})
	fmt.Fprintf(w, "data: %s\n\n", b)
	if flusher != nil {
		flusher.Flush()
	}
}

// handleWorkflowRunDelete — DELETE /api/agents/{agentID}/workflows/{wfID}/runs/{runID}
//
// Manually removes one run and its node outputs (spec decision 11, manual
// cleanup by run). The agent gate mirrors the run trigger: only a caller who
// can reach this agent may delete its runs.
func (s *Server) handleWorkflowRunDelete(w http.ResponseWriter, r *http.Request) {
	if s.resolveAgent(r, r.PathValue("agentID")) == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}
	dbs, ok := s.dataStore.(*store.DBStore)
	if !ok || dbs == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "workflow store unavailable"})
		return
	}
	if err := dbs.DeleteWorkflowRun(r.Context(), r.PathValue("runID")); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleWorkflowRunsBatchDelete — DELETE /api/agents/{agentID}/workflows/{wfID}/runs
//
// Batch-deletes runs matching the query filter (spec decision 11, manual
// cleanup by condition). Query params (at least one required — a bare DELETE
// never wipes the table):
//   - status:      succeeded | failed | needs_intervention
//   - older_than:  Go duration relative to now, e.g. "72h" (finished_at before now-d)
//   - owner:       run owner user id
//
// Returns {"ok":true,"deleted":N}.
func (s *Server) handleWorkflowRunsBatchDelete(w http.ResponseWriter, r *http.Request) {
	if s.resolveAgent(r, r.PathValue("agentID")) == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}
	dbs, ok := s.dataStore.(*store.DBStore)
	if !ok || dbs == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "workflow store unavailable"})
		return
	}
	q := r.URL.Query()
	status := q.Get("status")
	owner := q.Get("owner")
	var olderThan time.Time
	if v := q.Get("older_than"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad older_than (use a positive Go duration like 72h)"})
			return
		}
		olderThan = time.Now().Add(-d)
	}
	if status == "" && olderThan.IsZero() && owner == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "at least one of status / older_than / owner is required"})
		return
	}
	n, err := dbs.DeleteWorkflowRunsBy(r.Context(), status, olderThan, owner, 1000)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "deleted": n})
}

// handleWorkflowList — GET /api/agents/{agentID}/workflows
//
// Lists this agent's workflows (metadata only). Ownership is the visibility
// gate: only this agent's directory is read, so other agents' workflows never
// appear (AC1/AC5).
func (s *Server) handleWorkflowList(w http.ResponseWriter, r *http.Request) {
	ag := s.resolveAgent(r, r.PathValue("agentID"))
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}
	defs, err := workflow.LoadDir(ag.WorkflowsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(defs))
	for _, def := range defs {
		out = append(out, map[string]any{
			"id":          def.ID,
			"version":     def.Version,
			"description": def.Description,
			"concurrency": string(def.Concurrency),
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "workflows": out})
}

// handleWorkflowGet — GET /api/agents/{agentID}/workflows/{wfID}
//
// Reads one workflow's YAML source (for the editor's YAML pane, ticket 09).
func (s *Server) handleWorkflowGet(w http.ResponseWriter, r *http.Request) {
	ag := s.resolveAgent(r, r.PathValue("agentID"))
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}
	path := filepath.Join(ag.WorkflowsDir(), r.PathValue("wfID")+".yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "workflow not found"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "yaml": string(b)})
}

// handleWorkflowRunsList — GET /api/agents/{agentID}/workflows/{wfID}/runs
//
// Run history for one workflow (most-recent-first), backs the history view
// (ticket 08 AC3).
func (s *Server) handleWorkflowRunsList(w http.ResponseWriter, r *http.Request) {
	ag := s.resolveAgent(r, r.PathValue("agentID"))
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}
	dbs, ok := s.dataStore.(*store.DBStore)
	if !ok || dbs == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "workflow store unavailable"})
		return
	}
	runs, err := dbs.ListWorkflowRuns(r.Context(), r.PathValue("wfID"), 50)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "runs": runs})
}

// handleWorkflowRunGet — GET /api/agents/{agentID}/workflows/{wfID}/runs/{runID}
//
// One run's detail: the run-level row + every persisted node output (attempts
// included), so the UI can render per-node status and the failing node's error
// (ticket 08 AC3/AC4).
func (s *Server) handleWorkflowRunGet(w http.ResponseWriter, r *http.Request) {
	ag := s.resolveAgent(r, r.PathValue("agentID"))
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}
	dbs, ok := s.dataStore.(*store.DBStore)
	if !ok || dbs == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "workflow store unavailable"})
		return
	}
	runID := r.PathValue("runID")
	run, err := dbs.GetWorkflowRunRow(r.Context(), runID)
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "run not found"})
		return
	}
	nodes, _ := dbs.ListWorkflowNodeOutputs(r.Context(), runID)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "run": run, "nodes": nodes})
}

// handleWorkflowDelete — DELETE /api/agents/{agentID}/workflows/{wfID}
//
// Removes the workflow definition file and reloads the agent's service so the
// tool it was registered as disappears. Run history is retained (separate
// table, kept for audit) — only the definition is deleted.
func (s *Server) handleWorkflowDelete(w http.ResponseWriter, r *http.Request) {
	ag := s.resolveAgent(r, r.PathValue("agentID"))
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}
	path := filepath.Join(ag.WorkflowsDir(), r.PathValue("wfID")+".yaml")
	if err := os.Remove(path); err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "workflow not found"})
		return
	}
	ag.ReloadWorkflows()
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleWorkflowPut — PUT /api/agents/{agentID}/workflows/{wfID}
//
// Publishes a new version of a workflow: parse + validate the posted YAML, bump
// the version (existing+1, or 1 for a new file), write it, then reload the
// agent's workflow Service so it takes effect without a restart (spec decision
// 8 — immutable versions; old runs stay bound to the version they ran on).
// The wfID path segment overrides the YAML's id so the filename stays canonical.
func (s *Server) handleWorkflowPut(w http.ResponseWriter, r *http.Request) {
	ag := s.resolveAgent(r, r.PathValue("agentID"))
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}
	wfID := r.PathValue("wfID")
	var body struct {
		YAML string `json:"yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad request body: " + err.Error()})
		return
	}
	def, err := workflow.Parse(wfID, []byte(body.YAML))
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "parse: " + err.Error()})
		return
	}
	path := filepath.Join(ag.WorkflowsDir(), wfID+".yaml")
	version := 1
	if old, err := workflow.LoadFile(path); err == nil {
		version = old.Version + 1
	}
	def.Version = version
	if err := workflow.Validate(def, nil); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "validation: " + err.Error()})
		return
	}
	out, err := workflow.Marshal(def)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := os.MkdirAll(ag.WorkflowsDir(), 0o755); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	ag.ReloadWorkflows()
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "version": version})
}

// --- Workflow cron schedules (ticket 10, spec decision 16) ---

// workflowCronLoc is the timezone workflow schedules are interpreted in —
// spec decision 16 mandates UTC+8. Falls back to UTC if Asia/Shanghai is
// unavailable on the host.
var workflowCronLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.UTC
	}
	return loc
}()

// handleWorkflowScheduleCreate — POST /api/agents/{agentID}/workflows/{wfID}/schedules
//
// Creates a cron schedule. Body: {"cron_expr":"0 8 * * *","input":{...},"enabled"?:bool}.
// next_run is computed in UTC+8 (first future occurrence). The schedule fires
// the workflow with owner="system", session="".
func (s *Server) handleWorkflowScheduleCreate(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")
	wfID := r.PathValue("wfID")
	if s.resolveAgent(r, agentID) == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}
	var body struct {
		CronExpr string         `json:"cron_expr"`
		Input    map[string]any `json:"input"`
		Enabled  *bool          `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad body: " + err.Error()})
		return
	}
	if body.CronExpr == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "cron_expr required"})
		return
	}
	owner := ""
	if ident, ok := auth.FromContext(r.Context()); ok {
		owner = ident.EffectiveUserID()
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	next := cron.NextOccurrenceIn(body.CronExpr, time.Now(), workflowCronLoc)
	sched := store.WorkflowScheduleRow{
		ID:          fmt.Sprintf("wfs_%d", time.Now().UnixNano()),
		AgentID:     agentID,
		WorkflowID:  wfID,
		OwnerUserID: owner,
		CronExpr:    body.CronExpr,
		Input:       body.Input,
		Enabled:     enabled,
		NextRun:     next.UTC().Format(time.RFC3339),
	}
	dbs, ok := s.dataStore.(*store.DBStore)
	if !ok || dbs == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "workflow store unavailable"})
		return
	}
	if err := dbs.CreateWorkflowSchedule(r.Context(), sched); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "schedule": sched})
}

// handleWorkflowScheduleList — GET /api/agents/{agentID}/workflows/{wfID}/schedules
func (s *Server) handleWorkflowScheduleList(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")
	wfID := r.PathValue("wfID")
	if s.resolveAgent(r, agentID) == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}
	dbs, ok := s.dataStore.(*store.DBStore)
	if !ok || dbs == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "workflow store unavailable"})
		return
	}
	all, err := dbs.ListWorkflowSchedules(r.Context(), agentID)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	out := make([]store.WorkflowScheduleRow, 0, len(all))
	for _, sc := range all {
		if sc.WorkflowID == wfID {
			out = append(out, sc)
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "schedules": out})
}

// handleWorkflowScheduleToggle — PATCH /api/agents/{agentID}/workflows/{wfID}/schedules/{schedID}
// Body: {"enabled": bool}. Enables/disables without deleting.
func (s *Server) handleWorkflowScheduleToggle(w http.ResponseWriter, r *http.Request) {
	if s.resolveAgent(r, r.PathValue("agentID")) == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}
	dbs, ok := s.dataStore.(*store.DBStore)
	if !ok || dbs == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "workflow store unavailable"})
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Enabled == nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "enabled required"})
		return
	}
	if err := dbs.SetWorkflowScheduleEnabled(r.Context(), r.PathValue("schedID"), *body.Enabled); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleWorkflowScheduleDelete — DELETE /api/agents/{agentID}/workflows/{wfID}/schedules/{schedID}
func (s *Server) handleWorkflowScheduleDelete(w http.ResponseWriter, r *http.Request) {
	if s.resolveAgent(r, r.PathValue("agentID")) == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}
	dbs, ok := s.dataStore.(*store.DBStore)
	if !ok || dbs == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "workflow store unavailable"})
		return
	}
	if err := dbs.DeleteWorkflowSchedule(r.Context(), r.PathValue("schedID")); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
