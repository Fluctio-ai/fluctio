package setup

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/fluctio-ai/fluctio/internal/auth"
	"github.com/fluctio-ai/fluctio/internal/store"
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
