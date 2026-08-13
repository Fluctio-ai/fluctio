package setup

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/fluctio-ai/fluctio/internal/auth"
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
