package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/embedding"
	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
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
	// Cascade: remove wiki pages generated from this source so deleting a
	// KB source doesn't leave orphan wiki entries behind.
	if ws := s.wikiStoreFor(agentID); ws != nil {
		_, _ = ws.DeletePagesBySource(r.Context(), agentID, sourceID)
	}
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
