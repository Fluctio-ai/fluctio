package setup

import (
	"encoding/json"
	"net/http"

	"github.com/fluctio-ai/fluctio/internal/cron"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// --- Per-agent cron jobs (DB-backed) ---
//
// Agents schedule work at runtime via the create_cron_job tool, which
// persists into the cron_jobs DB table, and the cron.Scheduler (which
// actually fires them) only watches the DB. handleListAgentCronJobs
// surfaces them at /api/agents/{id}/cron. The legacy /api/cron set that
// read the flat fluctio.json catalog was removed — it wrote to a store
// neither the scheduler nor the dashboard read.

func (s *Server) handleListAgentCronJobs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	jobs, err := s.dataStore.ListCronJobsByAgent(r.Context(), id)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if jobs == nil {
		jobs = []store.CronJobRecord{}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *Server) handleDeleteAgentCronJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	jobID := r.PathValue("jobId")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	// Verify the job belongs to this agent before deleting — otherwise
	// the path param could be used to delete jobs the caller doesn't
	// own (the cron table has no user_id; we gate via agent ownership).
	job, err := s.dataStore.GetCronJob(r.Context(), jobID)
	if err != nil || job == nil || job.AgentID != id {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "job not found for this agent"})
		return
	}
	if err := s.dataStore.DeleteCronJob(r.Context(), jobID); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleToggleAgentCronJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	jobID := r.PathValue("jobId")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	var req struct {
		Enabled    *bool `json:"enabled,omitempty"`
		Background *bool `json:"background,omitempty"`
		Deliver    *bool `json:"deliver,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return
	}
	job, err := s.dataStore.GetCronJob(r.Context(), jobID)
	if err != nil || job == nil || job.AgentID != id {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "job not found for this agent"})
		return
	}
	if req.Enabled != nil {
		job.Enabled = *req.Enabled
	}
	if req.Background != nil {
		job.Background = *req.Background
	}
	if req.Deliver != nil {
		job.Deliver = *req.Deliver
	}
	// Deliver only means something for a background job. A foreground job
	// runs IN the bound conversation, so its reply is always delivered —
	// leaving deliver=false there would advertise "no delivery" while the
	// turn actually answers in the chat. Normalise on every write.
	if !job.Background {
		job.Deliver = true
	}
	if err := s.dataStore.SaveCronJob(r.Context(), job); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// Wake the DB-mode scheduler: pollStore sleeps until the MIN(next_run)
	// of the enabled set it sampled earlier, so a freshly enabled job with
	// a near next_run would otherwise starve for days (same notify the
	// cron tool's create path sends).
	cron.NotifyJobCreated()
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "job": job})
}

