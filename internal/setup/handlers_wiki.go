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

func (s *Server) handleWikiReindexEmbed(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	if rec := s.requireAgentOwner(w, r, id); rec == nil {
		return
	}
	db, ok := s.dataStore.(*store.DBStore)
	if !ok || db == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "vector store not available"})
		return
	}
	var vec config.VectorCfg
	if err := scope.SettingInto(r.Context(), db, "vectorization", id, &vec); err != nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "vectorization cfg read failed"})
		return
	}
	emb := wiki.EmbedderFromVectorCfg(vec)
	if emb == nil || !emb.Available() {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "wiki embedding not enabled or embedding endpoint not configured"})
		return
	}
	ws := s.wikiStoreFor(id)
	if ws == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "wiki store not available"})
		return
	}
	// Detached so a full re-embed of many pages (one network call + 100ms
	// pacing each) survives gateway timeouts; the client surfaces
	// "continues in background" on 504.
	// Per-agent dedup (same pattern as wikiGenLocks): overlapping
	// force=true reindexes each clear all page embeddings first, then
	// double-bill the embedding API and overwrite each other's vectors.
	if _, loaded := wikiReindexLocks.LoadOrStore(id, true); loaded {
		jsonResponse(w, http.StatusConflict, map[string]any{"ok": false, "error": "reindex already running"})
		return
	}
	defer wikiReindexLocks.Delete(id)
	ctx, cancel := detachedTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	force := r.URL.Query().Get("force") == "true"
	res, err := wiki.ReindexEmbeddings(ctx, ws, emb, id, force, 100*time.Millisecond)
	if err != nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":        true,
		"processed": res.Processed,
		"failed":    res.Failed,
	})
}

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

	if !s.requireWritable(w, r) {
		return
	}
	if s.requireAgentOwner(w, r, agentID) == nil {
		return
	}
	ws := s.wikiStoreFor(agentID)
	if ws == nil {
		http.Error(w, "not found", 404)
		return
	}
	// Resolve the page first and verify it belongs to the path agent:
	// DeletePage keys on the global page UUID, so without this check any
	// caller could delete another agent's page through this route.
	p, err := resolveWikiPage(r.Context(), ws, agentID, pageID)
	if err != nil || p == nil {
		http.Error(w, "not found", 404)
		return
	}
	if err := ws.DeletePage(r.Context(), p.ID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// resolveWikiPage resolves "type:slug" or UUID page ids and (for UUIDs)
// verifies the page actually belongs to agentID, so a foreign page id
// can't be read or written through this agent's route.
func resolveWikiPage(ctx context.Context, ws *wiki.WikiStore, agentID, pageID string) (*wiki.WikiPage, error) {
	var p *wiki.WikiPage
	var err error
	if idx := strings.Index(pageID, ":"); idx > 0 {
		p, err = ws.GetPageBySlug(ctx, agentID, pageID[:idx], pageID[idx+1:])
	} else {
		p, err = ws.GetPage(ctx, pageID)
	}
	if err != nil || p == nil {
		return nil, err
	}
	if p.AgentID != agentID {
		return nil, nil
	}
	return p, nil
}

// handleWikiUpdatePage applies manual edits from the clients (wiki PUT,
// contract §8): title/body only — page_type/slug/source_ids stay owned
// by the generator. Ids resolve exactly like handleWikiGetPage
// ("type:slug" or UUID); UpsertPage stamps UpdatedAt and bumps revision.
func (s *Server) handleWikiUpdatePage(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	pageID := r.PathValue("pageId")

	if rec := s.requireAgentOwner(w, r, agentID); rec == nil {
		return
	}
	ws := s.wikiStoreFor(agentID)
	if ws == nil {
		http.Error(w, "not found", 404)
		return
	}
	var req struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
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
	if p == nil || p.AgentID != agentID {
		http.Error(w, "not found", 404)
		return
	}
	if req.Title != nil && *req.Title != "" {
		p.Title = *req.Title
	}
	if req.Body != nil {
		p.Body = *req.Body
	}
	if err := ws.UpsertPage(r.Context(), p); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// UpsertPage bumps the row's revision in SQL but not the struct —
	// reflect it so the response matches what a follow-up GET returns.
	p.Revision++
	writeJSON(w, http.StatusOK, p)
}

// wikiGenLocks prevents concurrent generation for the same agent.
var wikiGenLocks sync.Map // map[string]bool

// wikiReindexLocks prevents concurrent reindex-embed runs for the same
// agent: two overlapping force=true reindexes each clear the page
// embeddings first, doubling the embedding API spend and repeatedly
// overwriting each other's vectors.
var wikiReindexLocks sync.Map // map[string]bool

// wikiGenProgress tracks per-agent wiki generation progress for the UI.
// The goroutine in runWikiGeneration mutates counters while
// handleWikiProgress reads them concurrently, so all access goes through
// the mutex — the sync.Map only protects the map itself.
type wikiGenProgress struct {
	mu        sync.Mutex
	Total     int       `json:"total"`
	Done      int       `json:"done"`
	Failed    int       `json:"failed"`
	Status    string    `json:"status"` // "running" | "done" | "failed"
	UpdatedAt time.Time `json:"updated_at"`
}

// wikiGenProgressView is the JSON projection of wikiGenProgress — a
// plain struct without the mutex so it can be copied for output.
type wikiGenProgressView struct {
	Total     int       `json:"total"`
	Done      int       `json:"done"`
	Failed    int       `json:"failed"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
}

// snapshot returns a consistent copy for JSON output.
func (p *wikiGenProgress) snapshot() wikiGenProgressView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return wikiGenProgressView{
		Total:     p.Total,
		Done:      p.Done,
		Failed:    p.Failed,
		Status:    p.Status,
		UpdatedAt: p.UpdatedAt,
	}
}

var wikiGenProgressMap sync.Map // agentID -> *wikiGenProgress

func bumpWikiProgress(agentID string, success bool) {
	v, ok := wikiGenProgressMap.Load(agentID)
	if !ok {
		return
	}
	p := v.(*wikiGenProgress)
	p.mu.Lock()
	defer p.mu.Unlock()
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
	// Generation burns the agent owner's LLM quota — gate it behind the
	// same writable+owner pair as the other mutating wiki endpoints.
	if !s.requireWritable(w, r) {
		return
	}
	if s.requireAgentOwner(w, r, agentID) == nil {
		return
	}

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
		// A panic here would take the whole process down (background
		// goroutines don't get net/http's per-request recovery); collapse
		// it into a failed progress entry instead.
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("wiki generate panicked", "agent", agentID, "panic", rec)
				finishWikiProgress(agentID, "failed")
			}
		}()
		s.runWikiGeneration(agentID, req.SourceIDs, req.Force)
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// finishWikiProgress stamps the terminal status on the agent's progress
// entry (done / failed).
func finishWikiProgress(agentID, status string) {
	if v, ok := wikiGenProgressMap.Load(agentID); ok {
		p := v.(*wikiGenProgress)
		p.mu.Lock()
		defer p.mu.Unlock()
		p.Status = status
		p.UpdatedAt = time.Now()
	}
}

func (s *Server) handleWikiProgress(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if v, ok := wikiGenProgressMap.Load(agentID); ok {
		writeJSON(w, http.StatusOK, v.(*wikiGenProgress).snapshot())
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
	var mem config.MemoryCfg
	if s.dataStore != nil {
		_ = scope.SettingInto(ctx, s.dataStore, "memory", agentID, &mem)
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
		// Pending count follows the per-type selection so the status line
		// matches what the sweep will actually process.
		if pending, err := dbs.CountPendingKBSources(ctx, agentID, mem.WikiAutoGen.IncludeTypes); err == nil {
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
		if err != nil {
			// A transient DB error used to leave toProcess nil, so the
			// run silently did nothing after the handler already said
			// "started" — surface it as a failed progress entry instead.
			slog.Warn("wiki generate: ListSources failed", "agent", agentID, "error", err)
			wikiGenProgressMap.Store(agentID, &wikiGenProgress{Status: "failed", UpdatedAt: time.Now()})
			return
		}
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
		return wiki.InvokeWithRetry(ctx, func(ctx context.Context, msgs []provider.Message) (string, error) {
			// JSON mode: page plans embed free-text titles/bodies, where
			// unescaped quotes would break the downstream plan parse.
			resp, err := prov.Chat(provider.WithJSONMode(provider.WithNoThinking(ctx)), msgs, nil, model, 8192, 0.3)
			if err != nil {
				return "", err
			}
			return resp.Content, nil
		}, messages, 4)
	}

	var vec config.VectorCfg
	if dbs, ok := s.dataStore.(*store.DBStore); ok {
		if err := scope.SettingInto(ctx, dbs, "vectorization", agentID, &vec); err != nil {
			slog.Debug("wiki generate: vectorization cfg read failed", "agent", agentID, "error", err)
		}
	}
	gen := wiki.NewGenerator(ws, kbs, invoker, wiki.EmbedderFromVectorCfg(vec), vec.WikiThreshold)
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

	// Cross-source dead-link cleanup: per-source Generate can leave
	// [[links]] pointing at pages a later source's DeletePagesBySource
	// removed (e.g. a merged page that the owning source deleted). Walk
	// every page body once more against the final page set and degrade
	// dead links to plain text.
	if finalPages, _, ferr := ws.ListPages(ctx, agentID, "", 500, 0); ferr == nil {
		finalIDs := map[string]bool{}
		for _, p := range finalPages {
			finalIDs[p.ID] = true
		}
		for i := range finalPages {
			p := &finalPages[i]
			if !strings.Contains(p.Body, "[[") {
				continue
			}
			cleaned := wiki.PostProcessLinks(p.Body, map[string]string{}, finalIDs)
			if cleaned != p.Body {
				p.Body = cleaned
				_ = ws.UpsertPage(ctx, p)
			}
		}
	}
	finishWikiProgress(agentID, "done")
}

// providerForAgent merges system → user(owner) → agent provider scopes
// (agent wins) via scope.Providers, matching the chat path.
func (s *Server) providerForAgent(agentID string) (provider.Provider, string) {
	if s.dataStore == nil {
		return nil, ""
	}

	ctx := context.Background()

	providerMap, err := scope.Providers(ctx, s.dataStore, agentID)
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
