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

// handleMaintenanceStatus returns the coordinator's current (or last)
// run plus a read-only bloat picture of the database.
func (s *Server) handleMaintenanceStatus(w http.ResponseWriter, r *http.Request) {
	if s.maintenance == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "maintenance not wired"})
		return
	}
	st := s.maintenance.Status()
	resp := map[string]any{
		"maintenance": st,
		"turnsActive": !s.maintenance.IdleNow(),
	}
	if db, err := s.maintenance.DBStats(r.Context()); err == nil {
		resp["db"] = db
	} else if err != maintenance.ErrNotBound {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleMaintenanceVacuum starts one backup→VACUUM run in the
// background. Accepts immediately (202); poll status for the outcome.
// 409 while a run is in flight; nothing about the database has been
// touched before the idle window is acquired, so a rejected call is a
// no-op.
func (s *Server) handleMaintenanceVacuum(w http.ResponseWriter, r *http.Request) {
	if s.maintenance == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "maintenance not wired"})
		return
	}
	if err := s.maintenance.Start(); err != nil {
		switch err {
		case maintenance.ErrAlreadyRunning:
			writeJSON(w, http.StatusConflict, map[string]any{"error": "maintenance already running"})
		case maintenance.ErrSQLiteOnly:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}
