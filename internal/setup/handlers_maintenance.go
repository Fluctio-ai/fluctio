package setup

import (
	"net/http"

	"github.com/fluctio-ai/fluctio/internal/maintenance"
)

// --- /api/maintenance (GET status / POST vacuum) — online SQLite
// maintenance coordinator ---
//
// Status is honest by design: staged (idle/waiting/backup/vacuum/
// done/failed) with elapsed wall time, never a fake percentage. The
// vacuum trigger is super-admin-only because it briefly queues all DB
// traffic behind the single SQLite connection.

// requireMaintenance answers the 503 envelope when the coordinator
// isn't wired; handlers proceed only on true.
func (s *Server) requireMaintenance(w http.ResponseWriter) bool {
	if s.maintenance == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "maintenance not wired"})
		return false
	}
	return true
}

// handleMaintenanceStatus returns the coordinator's current (or last)
// run plus a read-only bloat picture of the database. DB stats are
// expensive (dbstat walks every page of the db), so while a run is in
// flight the response carries only the in-memory status — the frontend
// polls this every 2s during waiting/backup/vacuum.
func (s *Server) handleMaintenanceStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaintenance(w) {
		return
	}
	st := s.maintenance.Status()
	resp := map[string]any{
		"maintenance": st,
		"turnsActive": !s.maintenance.IdleNow(),
	}
	switch st.Stage {
	case maintenance.StageIdle, maintenance.StageDone, maintenance.StageFailed:
		if db, err := s.maintenance.DBStats(r.Context()); err == nil {
			resp["db"] = db
		} else if err != maintenance.ErrNotBound {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	jsonResponse(w, http.StatusOK, resp)
}

// handleMaintenanceVacuum starts one backup→VACUUM run in the
// background. Accepts immediately (202); poll status for the outcome.
// 409 while a run is in flight; nothing about the database has been
// touched before the idle window is acquired, so a rejected call is a
// no-op.
func (s *Server) handleMaintenanceVacuum(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaintenance(w) {
		return
	}
	if err := s.maintenance.Start(); err != nil {
		switch err {
		case maintenance.ErrAlreadyRunning:
			jsonResponse(w, http.StatusConflict, map[string]any{"error": "maintenance already running"})
		case maintenance.ErrSQLiteOnly:
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		default:
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		}
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]any{"ok": true})
}
