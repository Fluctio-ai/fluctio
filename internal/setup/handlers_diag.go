package setup

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/fluctio-ai/fluctio/internal/auth"
	"github.com/fluctio-ai/fluctio/internal/diag"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// diagReportsDir is where generated reports live — mirrors the
// diag.ReportOptions default. Kept here so list/download resolve the same
// path generation writes to.
func diagReportsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".fluctio", "diag-reports"), nil
}

// handleDiagReportGenerate runs the LLM error-report generator. Uses the
// owner's default agent as the LLM (its provider + model), so no separate
// diagnostic-model config is needed. Gated on an authenticated session.
//
// Synchronous for now: the LLM call takes a few to tens of seconds; if that
// proves a UX problem behind a proxy timeout, wrap in a job + poll (spec §5).
func (s *Server) handleDiagReportGenerate(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	space, err := s.userResolver.UserSpaceFor(ident.EffectiveUserID())
	if err != nil || space == nil || space.Agents == nil {
		jsonResponse(w, http.StatusServiceUnavailable,
			map[string]any{"error": "no agent manager available"})
		return
	}
	ag := space.Agents.DefaultAgent()
	if ag == nil {
		jsonResponse(w, http.StatusServiceUnavailable,
			map[string]any{"error": "no default agent configured"})
		return
	}
	var body struct {
		Days    int    `json:"days"`
		AgentID string `json:"agentId,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Days <= 0 {
		body.Days = 3 // align with llm_call_diag retention default
	}
	db, ok := s.dataStore.(*store.DBStore)
	if !ok || db == nil {
		jsonResponse(w, http.StatusServiceUnavailable,
			map[string]any{"error": "requires sqlite store"})
		return
	}
	path, err := diag.GenerateReport(r.Context(), db, ag, diag.ReportOptions{
		Since:   time.Now().Add(-time.Duration(body.Days) * 24 * time.Hour),
		AgentID: body.AgentID,
	})
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"path": path,
		"name": filepath.Base(path),
	})
}

// handleDiagReportList returns generated reports, newest first.
func (s *Server) handleDiagReportList(w http.ResponseWriter, r *http.Request) {
	dir, err := diagReportsDir()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	entries, _ := os.ReadDir(dir)
	type report struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		Time string `json:"time"`
	}
	var reps []report
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		fi, _ := e.Info()
		reps = append(reps, report{
			Name: e.Name(),
			Size: fi.Size(),
			Time: fi.ModTime().UTC().Format(time.RFC3339),
		})
	}
	sort.Slice(reps, func(i, j int) bool { return reps[i].Time > reps[j].Time })
	jsonResponse(w, http.StatusOK, map[string]any{"reports": reps})
}

// handleDiagReportDownload serves one report file. Name is sanitized to its
// base + .md extension so a crafted path can't escape diagReportsDir.
func (s *Server) handleDiagReportDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || filepath.Base(name) != name || filepath.Ext(name) != ".md" {
		http.Error(w, "invalid report name", http.StatusBadRequest)
		return
	}
	dir, err := diagReportsDir()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.ServeFile(w, r, filepath.Join(dir, name))
}
