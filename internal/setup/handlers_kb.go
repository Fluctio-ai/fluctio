package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/embedding"
	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/wiki"
)

// handleListKBSources lists knowledge base sources for an agent.
func (s *Server) handleListKBSources(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)

	if kbStore == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	sources, err := kbStore.ListSources(r.Context(), agentID, 50, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sources == nil {
		sources = []kb.KBSource{}
	}
	writeJSON(w, http.StatusOK, sources)
}

// handleKBIngestText adds text content to the agent's knowledge base.
func (s *Server) handleKBIngestText(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}
	var req struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}
	title := req.Title
	if title == "" {
		title = "Untitled"
	}
	kbStore := s.kbStoreFor(agentID)

	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	id, err := kbStore.IngestText(r.Context(), agentID, title, req.Content, "text", "api")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"source_id": id, "chars": len(req.Content)})
}

// handleKBIngestURL fetches a URL and adds its content to the knowledge base.
func (s *Server) handleKBIngestURL(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}
	var req struct {
		URL   string `json:"url"`
		Title string `json:"title,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		http.Error(w, "only http/https URLs are allowed", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)

	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	title, content, err := kb.FetchURLContent(r.Context(), req.URL)
	if err != nil {
		http.Error(w, fmt.Sprintf("fetch failed: %s", err), http.StatusBadRequest)
		return
	}
	if req.Title != "" {
		title = req.Title
	}
	id, err := kbStore.IngestText(r.Context(), agentID, title, content, "url", req.URL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"source_id": id, "chars": len(content), "title": title})
}

// handleDeleteKBSource deletes// handleDeleteKBSource deletes a source and all its entries.
func (s *Server) handleDeleteKBSource(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	sourceID := r.PathValue("sourceId")
	if agentID == "" || sourceID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)

	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	if err := kbStore.DeleteSource(r.Context(), agentID, sourceID); err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Wiki page cascade is handled inside DeleteSource via the onDeleteSource
	// hook wired in kbStoreFor, so every delete path (HTTP / MCP / agent tool)
	// stays in sync without each caller repeating the cleanup.
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleGetKBStats returns knowledge base statistics for an agent.
// handleListKBEntries returns the chunk entries for a KB source.
func (s *Server) handleListKBEntries(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	sourceID := r.PathValue("sourceId")
	if agentID == "" || sourceID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	entries, err := kbStore.ListEntries(r.Context(), agentID, sourceID, 50, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []kb.KBEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) handleGetKBStats(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)

	if kbStore == nil {
		writeJSON(w, http.StatusOK, &kb.KBStats{})
		return
	}
	stats, err := kbStore.GetStats(r.Context(), agentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// handleKBSearch searches the knowledge base via HTTP.
func (s *Server) handleKBSearch(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}
	var req struct {
		Query string `json:"query"`
		Limit int    `json:"limit,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Query == "" {
		http.Error(w, "query is required", http.StatusBadRequest)
		return
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 5
	}
	kbStore := s.kbStoreFor(agentID)

	if kbStore == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	results, err := kbStore.Search(r.Context(), agentID, req.Query, limit, 0, 0.5, 0.45)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if results == nil {
		results = []kb.KBResult{}
	}
	writeJSON(w, http.StatusOK, results)
}

// kbStoreFor creates a KBStore for the given agent if the dataStore is available.
// When KBEmbedding is on and an embedder is configured it also equips the store
// with the embedder (+ optional reranker) so admin-panel search uses vector
// recall — same path the agent's own KB tools use.
func (s *Server) kbStoreFor(agentID string) *kb.KBStore {
	if s.dataStore == nil {
		return nil
	}
	dbs, ok := s.dataStore.(*store.DBStore)
	if !ok {
		return nil
	}
	ks := kb.NewKBStore(dbs.DB(), dbs.Dialect())
	var vec config.VectorCfg
	if err := scope.SettingInto(context.Background(), dbs, "vectorization", "", agentID, &vec); err == nil &&
		vec.KBEmbedding && vec.Embedding.Enabled {
		emb := embedding.NewOpenAICompatEmbedder(vec.Embedding.APIBase, vec.Embedding.APIKey, vec.Embedding.Model, vec.Embedding.Dim, vec.Embedding.DimEnabled)
		if emb.Available() {
			var rr embedding.Reranker
			if vec.Reranker.Enabled {
				rr = embedding.NewJinaReranker(vec.Reranker.APIBase, vec.Reranker.APIKey, vec.Reranker.Model)
			}
			ks.SetRetriever(emb, rr)
		}
	}
	// Cascade wiki cleanup on delete so the MCP path matches the HTTP
	// handler — both create their KBStore here, so one hook covers both.
	// The agent-tool path (manager) sets the same hook separately.
	ks.SetOnDeleteSource(func(agentID, sourceID string) {
		if ws := s.wikiStoreFor(agentID); ws != nil {
			_, _ = ws.DeletePagesBySource(context.Background(), agentID, sourceID)
		}
	})
	return ks
}

// handleKBSaveFlash stores one inspiration flash (灵感闪记) — a short,
// un-chunked note — for the agent. Backs the flash tab's "记一笔" input.
func (s *Server) handleKBSaveFlash(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	title := kb.DeriveTitle(req.Content)
	if dup := kbStore.CheckDuplicate(r.Context(), agentID, "flash", title, req.Content, kbStore.DupFlash()); dup.Duplicate {
		writeJSON(w, http.StatusOK, map[string]any{"deduped": true, "existing_source_id": dup.SourceID, "existing_title": dup.Title, "reason": dup.Reason})
		return
	}
	id, err := kbStore.SaveFlash(r.Context(), agentID, req.Content)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"source_id": id})
}

// handleKBSaveTodo creates a todo item with optional status/start_at/end_at.
func (s *Server) handleKBSaveTodo(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}
	var req struct {
		Content string `json:"content"`
		Status  string `json:"status,omitempty"`
		StartAt string `json:"start_at,omitempty"`
		EndAt   string `json:"end_at,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	title := kb.DeriveTitle(req.Content)
	if dup := kbStore.CheckDuplicate(r.Context(), agentID, "todo", title, req.Content, kbStore.DupTodo()); dup.Duplicate {
		writeJSON(w, http.StatusOK, map[string]any{"deduped": true, "existing_source_id": dup.SourceID, "existing_title": dup.Title, "reason": dup.Reason})
		return
	}
	id, err := kbStore.SaveTodo(r.Context(), agentID, req.Content, req.Status, req.StartAt, req.EndAt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"source_id": id})
}

// handleKBUpdateTodo mutates a todo's status/timing. Only non-empty fields land.
func (s *Server) handleKBUpdateTodo(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	sourceID := r.PathValue("sourceId")
	if agentID == "" || sourceID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	var req struct {
		Status  string `json:"status,omitempty"`
		StartAt string `json:"start_at,omitempty"`
		EndAt   string `json:"end_at,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	if err := kbStore.UpdateTodo(r.Context(), agentID, sourceID, req.Status, req.StartAt, req.EndAt); err != nil {
		if errors.Is(err, kb.ErrTodoNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// handleKBListTodos lists the agent's todos, optionally filtered by status
// ("" / "active" / a specific status) and a due-within-hours window.
func (s *Server) handleKBListTodos(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}
	status := r.URL.Query().Get("status")
	dueWithin := 0
	if v := r.URL.Query().Get("due_within_hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			dueWithin = n
		}
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	todos, err := kbStore.ListTodos(r.Context(), agentID, status, dueWithin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if todos == nil {
		todos = []kb.KBSource{}
	}
	writeJSON(w, http.StatusOK, todos)
}

// handleKBListBookmarks lists the agent's saved web bookmarks (newest first).
func (s *Server) handleKBListBookmarks(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	bookmarks, err := kbStore.ListBookmarks(r.Context(), agentID, 200, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if bookmarks == nil {
		bookmarks = []kb.KBBookmark{}
	}
	writeJSON(w, http.StatusOK, bookmarks)
}

// handleKBSaveBookmark saves a URL as a bookmark. The page body is fetched at
// save time (go-readability) so the bookmark survives link rot; a fetch
// failure still saves the URL-only bookmark. title defaults to the page
// <title> when the caller omits it.
func (s *Server) handleKBSaveBookmark(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}
	var req struct {
		URL     string `json:"url"`
		Title   string `json:"title,omitempty"`
		Summary string `json:"summary,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	content := ""
	fetchedTitle := ""
	if t, body, ferr := kb.FetchURLContent(r.Context(), req.URL); ferr == nil {
		content = body
		fetchedTitle = t
	}
	title := req.Title
	if title == "" {
		title = fetchedTitle
	}
	id, err := kbStore.SaveBookmark(r.Context(), agentID, req.URL, title, req.Summary, content, "web")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "title": title, "content_chars": len(content)})
}

// handleKBDeleteBookmark deletes one bookmark (and its embedding) by id.
func (s *Server) handleKBDeleteBookmark(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	bookmarkID := r.PathValue("bookmarkId")
	if agentID == "" || bookmarkID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	if err := kbStore.DeleteBookmark(r.Context(), agentID, bookmarkID); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleKBUpdateBookmark overwrites a bookmark's title and/or summary (the
// editable metadata) and re-embeds it. URL and fetched body are immutable here.
func (s *Server) handleKBUpdateBookmark(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	bookmarkID := r.PathValue("bookmarkId")
	if agentID == "" || bookmarkID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	var req struct {
		Title   string `json:"title,omitempty"`
		Summary string `json:"summary,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	if err := kbStore.UpdateBookmark(r.Context(), agentID, bookmarkID, req.Title, req.Summary); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// handleKBPromoteBookmark promotes a bookmark into a full KB article so it
// enters wiki generation. Reuses an existing same-URL source and stamps
// promoted_to_article_id on the bookmark; idempotent on repeat calls.
func (s *Server) handleKBPromoteBookmark(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	bookmarkID := r.PathValue("bookmarkId")
	if agentID == "" || bookmarkID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	articleID, err := kbStore.PromoteBookmarkToArticle(r.Context(), agentID, bookmarkID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"article_source_id": articleID, "bookmark_id": bookmarkID})
}

// handleKBMCP handles MCP JSON-RPC 2.0 requests for an agent's knowledge base.
func (s *Server) handleKBMCP(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)

	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	kb.ServeMCP(kbStore, agentID).ServeHTTP(w, r)
}

// handleKBGetInsights returns the stored deep-reading insights for one article
// source (200 + JSON), or 404 when none have been generated yet. The article
// detail page calls this to render the 深度解读 tab/section.
func (s *Server) handleKBGetInsights(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	sourceID := r.PathValue("sourceId")
	if agentID == "" || sourceID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	ins, err := kbStore.GetInsights(r.Context(), agentID, sourceID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ins == nil {
		http.Error(w, "no insights", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, ins)
}

// handleKBGenerateInsights runs the deep-reading LLM pass synchronously and
// returns the four generated sections. The call can take tens of seconds over
// a long article, so the ctx gets a 180s budget and the web client shows a
// loading state on the button. Errors are mapped to status codes so the client
// can tell "not an article" (400) / "not found" (404) / "LLM failed" (500).
func (s *Server) handleKBGenerateInsights(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	sourceID := r.PathValue("sourceId")
	if agentID == "" || sourceID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	prov, model := s.providerForAgent(agentID)
	if prov == nil {
		http.Error(w, "no LLM provider configured for this agent", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()
	invoker := kb.InsightInvoker(func(ctx context.Context, messages []provider.Message) (string, error) {
		return wiki.InvokeWithRetry(ctx, func(ctx context.Context, msgs []provider.Message) (string, error) {
			resp, err := prov.Chat(ctx, msgs, nil, model, 8192, 0.3)
			if err != nil {
				return "", err
			}
			return resp.Content, nil
		}, messages, 4)
	})
	ins, err := kbStore.GenerateInsights(ctx, agentID, sourceID, invoker, model, 8192)
	if err != nil {
		msg := err.Error()
		status := http.StatusInternalServerError
		switch {
		case strings.Contains(msg, "not an article"):
			status = http.StatusBadRequest
		case strings.Contains(msg, "not found"), strings.Contains(msg, "no text"):
			status = http.StatusNotFound
		}
		http.Error(w, msg, status)
		return
	}
	writeJSON(w, http.StatusOK, ins)
}

// handleKBListPending returns the agent's pending dedup entries — the cards
// shown on the article list awaiting merge / create / skip.
func (s *Server) handleKBListPending(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	pending, err := kbStore.ListPending(r.Context(), agentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, pending)
}

// handleKBResolvePending resolves a pending article entry: merge into the
// candidate source, create a fresh standalone source, or skip (discard).
func (s *Server) handleKBResolvePending(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	pendingID := r.PathValue("pendingId")
	if agentID == "" || pendingID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	ctx := r.Context()
	p, err := kbStore.GetPending(ctx, pendingID, agentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if p == nil {
		http.Error(w, "pending entry not found (expired or already resolved)", http.StatusNotFound)
		return
	}
	switch req.Action {
	case "skip":
		_ = kbStore.DeletePending(ctx, pendingID, agentID)
		writeJSON(w, http.StatusOK, map[string]any{"action": "skip", "pending_id": pendingID})
	case "create":
		sid, err := kbStore.IngestText(ctx, agentID, p.Title, p.Content, p.SourceType, p.SourceRef)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = kbStore.DeletePending(ctx, pendingID, agentID)
		writeJSON(w, http.StatusOK, map[string]any{"action": "create", "source_id": sid})
	case "merge":
		prov, model := s.providerForAgent(agentID)
		if prov == nil {
			http.Error(w, "no LLM provider configured for merge", http.StatusServiceUnavailable)
			return
		}
		timeoutCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
		defer cancel()
		invoker := kb.InsightInvoker(func(ctx context.Context, messages []provider.Message) (string, error) {
			return wiki.InvokeWithRetry(ctx, func(ctx context.Context, msgs []provider.Message) (string, error) {
				resp, err := prov.Chat(ctx, msgs, nil, model, 8192, 0.3)
				if err != nil {
					return "", err
				}
				return resp.Content, nil
			}, messages, 4)
		})
		if _, err := kbStore.MergeArticles(timeoutCtx, agentID, p.CandidateSourceID, p.Content, invoker, model, 8192); err != nil {
			http.Error(w, "merge failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		_ = kbStore.DeletePending(ctx, pendingID, agentID)
		writeJSON(w, http.StatusOK, map[string]any{"action": "merge", "source_id": p.CandidateSourceID})
	default:
		http.Error(w, "unknown action (merge|create|skip)", http.StatusBadRequest)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
