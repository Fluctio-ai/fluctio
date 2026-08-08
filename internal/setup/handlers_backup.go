package setup

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/fluctio-ai/fluctio/internal/backup"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/scope"
)

// --- /api/backup (GET/PUT) — system-level scheduled-backup config ---
//
// Reads/writes the system-scope "backup" row (agentID=""). The backup
// ticker reads this fresh every cycle, so PUT takes effect without a
// gateway restart.

func (s *Server) handleGetSystemBackup(w http.ResponseWriter, r *http.Request) {
	var cfg config.BackupCfg
	if s.dataStore != nil {
		_ = scope.SettingInto(r.Context(), s.dataStore, "backup", "", "", &cfg)
	}
	writeJSON(w, http.StatusOK, map[string]any{"backup": cfg})
}

func (s *Server) handleUpdateSystemBackup(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	cfg, _ := body["backup"].(map[string]any)
	if err := scope.SaveSetting(r.Context(), s.dataStore, "", "", "backup", cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- /api/backup/list (GET) — enumerate existing snapshots ---

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	items, err := backup.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if items == nil {
		items = []backup.Info{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": items})
}

// --- /api/backup/now (POST) — create a snapshot immediately ---

func (s *Server) handleBackupNow(w http.ResponseWriter, r *http.Request) {
	if s.dataStore == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "store unavailable"})
		return
	}
	cst := time.FixedZone("CST", 8*3600)
	name, size, err := backup.Create(r.Context(), s.dataStore, time.Now().In(cst))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name, "size": size})
}

// --- /api/backup/download?file= (GET) — serve one snapshot ---

func (s *Server) handleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("file")
	path, err := backup.FullPath(name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	http.ServeFile(w, r, path)
}

// --- /api/backup?file= (DELETE) — remove one snapshot ---

func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("file")
	if err := backup.Remove(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
