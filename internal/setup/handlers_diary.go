package setup

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/diary"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// diaryCST is the UTC+8 zone for default date framing (an alias of
// diary.CST, the single source of truth).
var diaryCST = diary.CST

// diaryGenLocks prevents two concurrent manual generations for the same
// (agent, date). Keyed "agentID:date".
var diaryGenLocks sync.Map

// diaryThinkingMode reads the agent's configured ThinkingMode (empty = default).
func (s *Server) diaryThinkingMode(agentID string) string {
	fc, _ := config.AgentFileConfigLoader(agentID, "")
	if fc.Diary != nil {
		return fc.Diary.ThinkingMode
	}
	return ""
}

// handleDiaryList GET /api/agents/{id}/diary?from=&to= — entries in
// [from,to] (YYYY-MM-DD, UTC+8), newest first. Defaults to last 30 days.
func (s *Server) handleDiaryList(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if rec := s.requireAgentOwner(w, r, agentID); rec == nil {
		return
	}
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	now := time.Now().In(diaryCST)
	if to == "" {
		to = now.Format("2006-01-02")
	}
	if from == "" {
		from = now.AddDate(0, 0, -29).Format("2006-01-02")
	}
	dbs, ok := s.dataStore.(*store.DBStore)
	if !ok || dbs == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	list, err := dbs.ListDailyDiaries(r.Context(), agentID, from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleDiaryGet GET /api/agents/{id}/diary/{date} — one entry.
func (s *Server) handleDiaryGet(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	date := r.PathValue("date")
	if rec := s.requireAgentOwner(w, r, agentID); rec == nil {
		return
	}
	// While a manual generation is running for this day, return 200 +
	// generating:true so the front-end's poll loop doesn't log a string
	// of 404s while the LLM is still working.
	if _, running := diaryGenLocks.Load(agentID + ":" + date); running {
		writeJSON(w, http.StatusOK, map[string]any{
			"agentId": agentID, "date": date, "generating": true,
		})
		return
	}
	dbs, ok := s.dataStore.(*store.DBStore)
	if !ok || dbs == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	d, err := dbs.GetDailyDiary(r.Context(), agentID, date)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if d == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handleDiaryGenerate POST /api/agents/{id}/diary/generate {"date":"..."} —
// manually (re)generate one day's diary asynchronously. date defaults to
// yesterday (UTC+8). Returns 202 immediately; the row appears via GET once
// done. 409 if a generation for that (agent, date) is already running.
func (s *Server) handleDiaryGenerate(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if rec := s.requireAgentOwner(w, r, agentID); rec == nil {
		return
	}
	if !s.requireWritable(w, r) {
		return
	}
	var req struct {
		Date string `json:"date"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	date := req.Date
	if date == "" {
		date = time.Now().In(diaryCST).AddDate(0, 0, -1).Format("2006-01-02")
	}

	key := agentID + ":" + date
	if _, loaded := diaryGenLocks.LoadOrStore(key, true); loaded {
		writeJSON(w, http.StatusConflict, map[string]string{"status": "already_running"})
		return
	}

	go func() {
		defer diaryGenLocks.Delete(key)
		dbs, ok := s.dataStore.(*store.DBStore)
		if !ok || dbs == nil {
			slog.Warn("diary gen: store is not DBStore", "agent", agentID)
			return
		}
		prov, model := s.providerForAgent(agentID)
		if prov == nil {
			slog.Warn("diary gen: no provider/model resolvable", "agent", agentID)
			return
		}
		entry, err := diary.Generate(context.Background(), dbs, agentID, date, prov, model, s.diaryThinkingMode(agentID))
		if err != nil {
			slog.Warn("diary gen failed", "agent", agentID, "date", date, "error", err)
			return
		}
		if entry != nil {
			slog.Info("diary manual gen done", "agent", agentID, "date", date,
				"themes", len(entry.Themes), "blindspots", len(entry.Blindspots))
		} else {
			slog.Info("diary manual gen: no summaries for day", "agent", agentID, "date", date)
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "date": date})
}
