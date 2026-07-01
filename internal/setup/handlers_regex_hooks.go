package setup

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func (s *Server) handleListRegexHooks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	hooks, err := s.dataStore.ListRegexHooks(r.Context(), id)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if hooks == nil {
		hooks = []store.RegexHookRecord{}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"hooks": hooks})
}

func (s *Server) handleGetRegexHook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	hookID := r.PathValue("hookId")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	hook, err := s.dataStore.GetRegexHook(r.Context(), hookID)
	if err != nil || hook == nil || hook.AgentID != id {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "hook not found"})
		return
	}
	jsonResponse(w, http.StatusOK, hook)
}

type saveRegexHookRequest struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Pattern         string `json:"pattern"`
	CLICommand      string `json:"cliCommand"`
	SortOrder       int    `json:"sortOrder"`
	ContinueOnMatch bool   `json:"continueOnMatch"`
	Enabled         bool   `json:"enabled"`
	ShowError       bool   `json:"showError"`
	ErrorMessage    string `json:"errorMessage"`
}

func (s *Server) handleSaveRegexHook(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if s.requireAgentOwner(w, r, agentID) == nil {
		return
	}
	var req saveRegexHookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return
	}
	if req.Name == "" || req.Pattern == "" || req.CLICommand == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "name, pattern, and cliCommand are required"})
		return
	}
	if _, err := regexp.Compile(req.Pattern); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid regex pattern: %v", err)})
		return
	}
	if req.ID == "" {
		req.ID = fmt.Sprintf("rh_%d", time.Now().UnixMilli())
	}
	hook := &store.RegexHookRecord{
		ID:              req.ID,
		AgentID:         agentID,
		Name:            req.Name,
		Pattern:         req.Pattern,
		CLICommand:      req.CLICommand,
		SortOrder:       req.SortOrder,
		ContinueOnMatch: req.ContinueOnMatch,
		Enabled:         req.Enabled,
		ShowError:       req.ShowError,
		ErrorMessage:    req.ErrorMessage,
	}
	if err := s.dataStore.SaveRegexHook(r.Context(), hook); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, hook)
}

func (s *Server) handleDeleteRegexHook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	hookID := r.PathValue("hookId")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	hook, err := s.dataStore.GetRegexHook(r.Context(), hookID)
	if err != nil || hook == nil || hook.AgentID != id {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "hook not found"})
		return
	}
	if err := s.dataStore.DeleteRegexHook(r.Context(), hookID); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleReorderRegexHooks(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if s.requireAgentOwner(w, r, agentID) == nil {
		return
	}
	var req struct {
		HookIDs []string `json:"hookIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return
	}
	if err := s.dataStore.ReorderRegexHooks(r.Context(), agentID, req.HookIDs); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// --- Hook Scripts (upload/list/delete for CLI scripts) ---

// hooksDir returns the agent's hooks script directory:
// ~/.fastclaw/agents/<agentID>/hooks/
func hooksDir(agentID string) (string, error) {
	home, err := config.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "agents", agentID, "hooks"), nil
}

func (s *Server) handleListHookScripts(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if s.requireAgentOwner(w, r, agentID) == nil {
		return
	}
	dir, err := hooksDir(agentID)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			jsonResponse(w, http.StatusOK, map[string]any{"scripts": []any{}})
			return
		}
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	type scriptInfo struct {
		Name    string `json:"name"`
		Size    int64  `json:"size"`
		ModTime string `json:"modTime"`
	}
	var scripts []scriptInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		scripts = append(scripts, scriptInfo{
			Name:    e.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime().Format(time.RFC3339),
		})
	}
	sort.Slice(scripts, func(i, j int) bool { return scripts[i].Name < scripts[j].Name })
	if scripts == nil {
		scripts = []scriptInfo{}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"scripts": scripts})
}

func (s *Server) handleUploadHookScript(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if s.requireAgentOwner(w, r, agentID) == nil {
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	headers := r.MultipartForm.File["file"]
	if len(headers) == 0 {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "no file"})
		return
	}
	dir, err := hooksDir(agentID)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	saved := make([]map[string]any, 0, len(headers))
	for _, h := range headers {
		name := filepath.Base(h.Filename)
		if name == "" || strings.Contains(name, "..") {
			continue
		}
		fh, err := h.Open()
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		data, err := io.ReadAll(fh)
		fh.Close()
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		dest := filepath.Join(dir, name)
		if err := os.WriteFile(dest, data, 0o755); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		saved = append(saved, map[string]any{"name": name, "size": len(data)})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "files": saved})
}

func (s *Server) handleDeleteHookScript(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if s.requireAgentOwner(w, r, agentID) == nil {
		return
	}
	name := r.PathValue("name")
	name = filepath.Base(name)
	if name == "" || strings.Contains(name, "..") {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid script name"})
		return
	}
	dir, err := hooksDir(agentID)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		if os.IsNotExist(err) {
			jsonResponse(w, http.StatusNotFound, map[string]any{"error": "script not found"})
			return
		}
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
