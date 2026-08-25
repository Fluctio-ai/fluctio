package kb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpToolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

type mcpToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type mcpToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content []mcpToolContent `json:"content"`
}

var mcpKBTools = []mcpToolDef{
	{
		Name:        "knowledgebase_search",
		Description: "Search the knowledge base for relevant information",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "The search query",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum number of results (default 5)",
				},
			},
			"required": []string{"query"},
		},
	},
	{
		Name:        "knowledgebase_ingest_text",
		Description: "Add text content to the knowledge base",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"title": map[string]interface{}{
					"type":        "string",
					"description": "A descriptive title for the entry",
				},
				"content": map[string]interface{}{
					"type":        "string",
					"description": "The text content to add",
				},
			},
			"required": []string{"title", "content"},
		},
	},
	{
		Name:        "knowledgebase_ingest_url",
		Description: "Fetch a URL and add its content to the knowledge base",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url": map[string]interface{}{
					"type":        "string",
					"description": "The URL to fetch and ingest",
				},
				"title": map[string]interface{}{
					"type":        "string",
					"description": "Optional title override",
				},
			},
			"required": []string{"url"},
		},
	},
	{
		Name:        "knowledgebase_list_sources",
		Description: "List all sources in the knowledge base",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum number of sources (default 20)",
				},
			},
		},
	},
	{
		Name:        "knowledgebase_delete_source",
		Description: "Delete a source and all its entries from the knowledge base",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"source_id": map[string]interface{}{
					"type":        "string",
					"description": "The source ID to delete",
				},
			},
			"required": []string{"source_id"},
		},
	},
}

// ServeMCP handles MCP JSON-RPC 2.0 requests for an agent's knowledge base.
func ServeMCP(store *KBStore, agentID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeRPCError(w, 0, -32700, "parse error")
			return
		}
		if req.JSONRPC != "2.0" {
			writeRPCError(w, req.ID, -32600, "invalid jsonrpc version")
			return
		}

		var resp jsonRPCResponse
		switch req.Method {
		case "initialize":
			resp = handleInitialize(req)
		case "tools/list":
			resp = handleToolsList(req)
		case "tools/call":
			resp = handleToolsCall(r.Context(), store, agentID, req)
		case "ping":
			resp = jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{}`)}
		default:
			writeRPCError(w, req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

func handleInitialize(req jsonRPCRequest) jsonRPCResponse {
	result, _ := json.Marshal(map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": map[string]interface{}{
			"name":    "fluctio-kb",
			"version": "1.0.0",
		},
	})
	return jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
}

func handleToolsList(req jsonRPCRequest) jsonRPCResponse {
	result, _ := json.Marshal(map[string]interface{}{
		"tools": mcpKBTools,
	})
	return jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
}

func handleToolsCall(ctx context.Context, store *KBStore, agentID string, req jsonRPCRequest) jsonRPCResponse {
	var params mcpToolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &jsonRPCError{Code: -32602, Message: "invalid params"},
		}
	}

	var text string
	var err error

	switch params.Name {
	case "knowledgebase_search":
		text, err = mcpExecSearch(ctx, store, agentID, params.Arguments)
	case "knowledgebase_ingest_text":
		text, err = mcpExecIngestText(ctx, store, agentID, params.Arguments)
	case "knowledgebase_ingest_url":
		text, err = mcpExecIngestURL(ctx, store, agentID, params.Arguments)
	case "knowledgebase_list_sources":
		text, err = mcpExecListSources(ctx, store, agentID, params.Arguments)
	case "knowledgebase_delete_source":
		text, err = mcpExecDeleteSource(ctx, store, agentID, params.Arguments)
	default:
		return jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &jsonRPCError{Code: -32602, Message: fmt.Sprintf("unknown tool: %s", params.Name)},
		}
	}

	if err != nil {
		text = fmt.Sprintf("Error: %s", err)
	}

	result, _ := json.Marshal(mcpToolResult{
		Content: []mcpToolContent{{Type: "text", Text: text}},
	})
	return jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
}

func mcpExecSearch(ctx context.Context, store *KBStore, agentID string, args json.RawMessage) (string, error) {
	var p struct {
		Query string `json:"query"`
		Limit int    `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if p.Query == "" {
		return "", fmt.Errorf("query is required")
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 5
	}
	results, err := store.Search(ctx, agentID, p.Query, limit, 0, 0.5, 0.45)
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "No matching entries found.", nil
	}
	deduped, ids := numberAndAccumulate(ctx, results)
	return formatResults(deduped, p.Query, ids, false), nil
}

func mcpExecIngestText(ctx context.Context, store *KBStore, agentID string, args json.RawMessage) (string, error) {
	var p struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if p.Content == "" {
		return "", fmt.Errorf("content is required")
	}
	title := p.Title
	if title == "" {
		title = "Untitled"
	}
	id, err := store.IngestText(ctx, agentID, title, p.Content, "text", "mcp")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Added to knowledge base: source_id=%s (%d chars)", id, len(p.Content)), nil
}

func mcpExecIngestURL(ctx context.Context, store *KBStore, agentID string, args json.RawMessage) (string, error) {
	var p struct {
		URL   string `json:"url"`
		Title string `json:"title,omitempty"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if p.URL == "" {
		return "", fmt.Errorf("url is required")
	}
	title, content, err := FetchURLContent(ctx, p.URL)
	if err != nil {
		return "", fmt.Errorf("fetch url: %w", err)
	}
	if p.Title != "" {
		title = p.Title
	}
	id, err := store.IngestText(ctx, agentID, title, content, "url", p.URL)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Ingested URL: source_id=%s (%d chars, title=%q)", id, len(content), title), nil
}

func mcpExecListSources(ctx context.Context, store *KBStore, agentID string, args json.RawMessage) (string, error) {
	var p struct {
		Limit int `json:"limit,omitempty"`
	}
	json.Unmarshal(args, &p)
	limit := p.Limit
	if limit <= 0 {
		limit = 20
	}
	sources, err := store.ListSources(ctx, agentID, limit, 0)
	if err != nil {
		return "", err
	}
	if len(sources) == 0 {
		return "Knowledge base is empty.", nil
	}
	result, _ := json.MarshalIndent(sources, "", "  ")
	return string(result), nil
}

func mcpExecDeleteSource(ctx context.Context, store *KBStore, agentID string, args json.RawMessage) (string, error) {
	var p struct {
		SourceID string `json:"source_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if p.SourceID == "" {
		return "", fmt.Errorf("source_id is required")
	}
	if err := store.DeleteSource(ctx, agentID, p.SourceID); err != nil {
		return "", err
	}
	return fmt.Sprintf("Deleted source %s.", p.SourceID), nil
}

func writeRPCError(w http.ResponseWriter, id, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: msg},
	}
	json.NewEncoder(w).Encode(resp)
}
