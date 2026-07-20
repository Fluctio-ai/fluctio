package setup

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/wiki"
)

func (s *Server) handleWikiStats(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	ws := s.wikiStoreFor(agentID)
	if ws == nil {
		writeJSON(w, http.StatusOK, &wiki.WikiStats{PageCounts: map[string]int{}})
		return
	}
	stats, err := ws.GetStats(r.Context(), agentID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleWikiListPages(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	pageType := r.URL.Query().Get("type")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	ws := s.wikiStoreFor(agentID)
	if ws == nil {
		writeJSON(w, http.StatusOK, map[string]any{"pages": []any{}, "total": 0})
		return
	}
	pages, total, err := ws.ListPages(r.Context(), agentID, pageType, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pages": pages, "total": total})
}

func (s *Server) handleWikiGetPage(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	pageID := r.PathValue("pageId")

	ws := s.wikiStoreFor(agentID)
	if ws == nil {
		http.Error(w, "not found", 404)
		return
	}
	// Wiki links embed "page_type:slug" rather than a UUID; resolve those
	// by (agent_id, page_type, slug), plain UUIDs via GetPage.
	var p *wiki.WikiPage
	var err error
	if idx := strings.Index(pageID, ":"); idx > 0 {
		p, err = ws.GetPageBySlug(r.Context(), agentID, pageID[:idx], pageID[idx+1:])
	} else {
		p, err = ws.GetPage(r.Context(), pageID)
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if p == nil {
		http.Error(w, "not found", 404)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleWikiGraph(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")

	ws := s.wikiStoreFor(agentID)
	if ws == nil {
		writeJSON(w, http.StatusOK, &wiki.WikiGraph{})
		return
	}
	g, err := ws.GetGraph(r.Context(), agentID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) handleWikiDeletePage(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	pageID := r.PathValue("pageId")

	ws := s.wikiStoreFor(agentID)
	if ws == nil {
		http.Error(w, "not found", 404)
		return
	}
	if err := ws.DeletePage(r.Context(), pageID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// wikiGenLocks prevents concurrent generation for the same agent.
var wikiGenLocks sync.Map // map[string]bool

// wikiGenProgress tracks per-agent wiki generation progress for the UI.
type wikiGenProgress struct {
	Total     int       `json:"total"`
	Done      int       `json:"done"`
	Failed    int       `json:"failed"`
	Status    string    `json:"status"` // "running" | "done"
	UpdatedAt time.Time `json:"updated_at"`
}

var wikiGenProgressMap sync.Map // agentID -> *wikiGenProgress

func bumpWikiProgress(agentID string, success bool) {
	v, ok := wikiGenProgressMap.Load(agentID)
	if !ok {
		return
	}
	p := v.(*wikiGenProgress)
	if success {
		p.Done++
	} else {
		p.Failed++
	}
	p.UpdatedAt = time.Now()
}

type wikiGenerateRequest struct {
	SourceIDs []string `json:"source_ids"`
	Force     bool     `json:"force,omitempty"`
}

func (s *Server) handleWikiGenerate(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")

	var req wikiGenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	if len(req.SourceIDs) == 0 {
		http.Error(w, "source_ids required", 400)
		return
	}

	if _, loaded := wikiGenLocks.LoadOrStore(agentID, true); loaded {
		writeJSON(w, http.StatusConflict, map[string]string{"status": "already_running"})
		return
	}

	go func() {
		defer wikiGenLocks.Delete(agentID)
		s.runWikiGeneration(agentID, req.SourceIDs, req.Force)
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func (s *Server) handleWikiProgress(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if v, ok := wikiGenProgressMap.Load(agentID); ok {
		writeJSON(w, http.StatusOK, v)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "idle"})
}

// handleWikiAutogenStatus returns the auto-generation config + last sweep
// outcome for the Wiki panel's status line: enabled flag from the memory
// setting, last_run/status/error from wiki_autogen_last_run, and a live count
// of KB sources whose wiki_generated_at is NULL.
func (s *Server) handleWikiAutogenStatus(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	ctx := r.Context()

	enabled := false
	if s.dataStore != nil {
		var mem config.MemoryCfg
		_ = scope.SettingInto(ctx, s.dataStore, "memory", "", agentID, &mem)
		enabled = mem.WikiAutoGen.Enabled
	}

	resp := map[string]any{
		"enabled": enabled,
	}
	if dbs, ok := s.dataStore.(*store.DBStore); ok {
		if st, err := dbs.GetWikiAutoGenStatus(ctx, agentID); err == nil && st != nil {
			if !st.LastRunAt.IsZero() {
				resp["last_run"] = st.LastRunAt.Format(time.RFC3339)
			}
			if st.LastStatus != "" {
				resp["last_status"] = st.LastStatus
			}
			if st.LastError != "" {
				resp["last_error"] = st.LastError
			}
		}
		if pending, err := dbs.CountPendingKBSources(ctx, agentID); err == nil {
			resp["pending"] = pending
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) runWikiGeneration(agentID string, sourceIDs []string, force bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ws := s.wikiStoreFor(agentID)
	if ws == nil {
		slog.Warn("wiki generate: no store", "agent", agentID)
		return
	}
	kbs := s.kbStoreFor(agentID)

	prov, model := s.providerForAgent(agentID)
	if prov == nil {
		slog.Warn("wiki generate: no LLM provider available", "agent", agentID)
		return
	}

	// Filter out already-processed sources unless forcing
	var toProcess []string
	if !force && kbs != nil {
		sources, err := kbs.ListSources(ctx, agentID, 100, 0)
		if err == nil {
			sourceMap := make(map[string]*kb.KBSource, len(sources))
			for i := range sources {
				sourceMap[sources[i].ID] = &sources[i]
			}
			for _, sid := range sourceIDs {
				src, ok := sourceMap[sid]
				if !ok || src.WikiGeneratedAt == nil {
					toProcess = append(toProcess, sid)
				} else {
					slog.Info("wiki generate: skipping already processed source", "source", sid)
				}
			}
		}
	}
	if force || kbs == nil {
		toProcess = sourceIDs
	}
	if len(toProcess) == 0 {
		slog.Info("wiki generate: all sources already processed, nothing to do", "agent", agentID)
		return
	}

	wikiGenProgressMap.Store(agentID, &wikiGenProgress{
		Total:     len(toProcess),
		Status:    "running",
		UpdatedAt: time.Now(),
	})

	slog.Info("wiki generate: using model", "model", model, "agent", agentID)
	invoker := func(ctx context.Context, messages []provider.Message) (string, error) {
		resp, err := prov.Chat(ctx, messages, nil, model, 8192, 0.3)
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}

	gen := wiki.NewGenerator(ws, kbs, invoker)
	for _, sid := range toProcess {
		r := gen.Generate(ctx, agentID, sid)
		if r.Error != "" {
			slog.Warn("wiki generate failed", "source", sid, "error", r.Error)
			bumpWikiProgress(agentID, false)
		} else {
			slog.Info("wiki generate done", "source", sid,
				"created", r.PagesCreated, "updated", r.PagesUpdated,
				"failed", r.PagesFailed, "edges", r.EdgesAdded)
			if kbs != nil {
				kbs.MarkSourceGenerated(ctx, sid)
			}
			bumpWikiProgress(agentID, true)
		}
	}
	if v, ok := wikiGenProgressMap.Load(agentID); ok {
		p := v.(*wikiGenProgress)
		p.Status = "done"
		p.UpdatedAt = time.Now()
	}
}

// providerForAgent merges system → user(owner) → agent provider scopes
// (agent wins) via scope.Providers, matching the chat path.
func (s *Server) providerForAgent(agentID string) (provider.Provider, string) {
	if s.dataStore == nil {
		return nil, ""
	}

	ctx := context.Background()

	var ownerUserID string
	if agentID != "" {
		if ag, err := s.dataStore.GetAgent(ctx, agentID); err == nil && ag != nil {
			ownerUserID = ag.UserID
		}
	}

	providerMap, err := scope.Providers(ctx, s.dataStore, ownerUserID, agentID)
	if err != nil || len(providerMap) == 0 {
		slog.Warn("wiki: no providers configured", "error", err)
		return nil, ""
	}

	// Read agent-level model override first, fall back to system default
	model := ""
	if agentID != "" {
		agentRow, _ := s.dataStore.GetConfigByName(ctx, store.KindSetting, agentID, "agents.defaults")
		if agentRow != nil {
			model, _ = agentRow.Data["model"].(string)
		}
	}
	if model == "" {
		defaultsRow, err := s.dataStore.GetConfigByName(ctx, store.KindSetting, "", "agents.defaults")
		if err != nil || defaultsRow == nil {
			slog.Warn("wiki: no agents.defaults config found")
			return nil, ""
		}
		model, _ = defaultsRow.Data["model"].(string)
	}
	if model == "" {
		return nil, ""
	}

	// Parse "provider/model" format
	parts := strings.SplitN(model, "/", 2)
	if len(parts) != 2 {
		return nil, ""
	}

	p, ok := providerMap[parts[0]]
	if !ok || p.APIKey == "" {
		return nil, ""
	}
	return provider.NewProvider(p.APIKey, p.APIBase, p.APIType), model
}

func (s *Server) wikiStoreFor(agentID string) *wiki.WikiStore {
	if s.dataStore == nil {
		return nil
	}
	if dbs, ok := s.dataStore.(*store.DBStore); ok {
		return wiki.NewWikiStore(dbs.DB(), dbs.Dialect())
	}
	return nil
}
