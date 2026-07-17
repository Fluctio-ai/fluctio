package kb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
	"github.com/fluctio-ai/fluctio/internal/httpclient"
)

func RegisterKBTools(r *tools.Registry, store *KBStore, agentID string) {
	registerKBSearch(r, store, agentID)
	registerKBAdd(r, store, agentID)
	registerKBIngestURL(r, store, agentID)
	registerKBList(r, store, agentID)
	registerKBDelete(r, store, agentID)
}

func registerKBSearch(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_search", "Search the agent's knowledge base for relevant information. Returns matching text chunks with source references. Use this when the user's question might be answered from previously stored knowledge.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "The search query to find relevant knowledge base entries",
			},
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum number of results to return (default 5)",
			},
		},
		"required": []string{"query"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			Query string `json:"query"`
			Limit int    `json:"limit,omitempty"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.Query == "" {
			return "", fmt.Errorf("query is required")
		}
		limit := args.Limit
		if limit <= 0 {
			limit = 5
		}
		results, err := store.Search(ctx, agentID, args.Query, limit, 0, 0.5)
		if err != nil {
			return "", err
		}
		if len(results) == 0 {
			return "No matching entries found in the knowledge base.", nil
		}
		return formatResults(results, args.Query), nil
	})
}

func registerKBAdd(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_add", "Add text content to the agent's knowledge base. The content will be automatically chunked and indexed for future retrieval. Use when the user explicitly asks to save or remember something in the knowledge base.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"title": map[string]interface{}{
				"type":        "string",
				"description": "A descriptive title for this knowledge entry",
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "The text content to add to the knowledge base",
			},
		},
		"required": []string{"title", "content"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.Content == "" {
			return "", fmt.Errorf("content is required")
		}
		title := args.Title
		if title == "" {
			title = "Untitled"
		}
		id, err := store.IngestText(ctx, agentID, title, args.Content, "text", "manual")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Added to knowledge base: source_id=%s (%d chars)", id, len(args.Content)), nil
	})
}

func registerKBIngestURL(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_ingest_url", "Fetch a URL and add its text content to the knowledge base. The page will be extracted, chunked, and indexed for future retrieval.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"url": map[string]interface{}{
				"type":        "string",
				"description": "The URL to fetch and ingest into the knowledge base",
			},
			"title": map[string]interface{}{
				"type":        "string",
				"description": "Optional title override (defaults to page title)",
			},
		},
		"required": []string{"url"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			URL   string `json:"url"`
			Title string `json:"title,omitempty"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.URL == "" {
			return "", fmt.Errorf("url is required")
		}
		u, err := url.Parse(args.URL)
		if err != nil {
			return "", fmt.Errorf("invalid url: %w", err)
		}
		if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
			return "", fmt.Errorf("scheme %q not allowed; use http or https", u.Scheme)
		}
		title, content, err := FetchURLContent(ctx, args.URL)
		if err != nil {
			return "", fmt.Errorf("fetch url: %w", err)
		}
		if args.Title != "" {
			title = args.Title
		}
		id, err := store.IngestText(ctx, agentID, title, content, "url", args.URL)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Ingested URL into knowledge base: source_id=%s (%d chars, title=%q)", id, len(content), title), nil
	})
}

func registerKBList(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_list", "List all sources in the agent's knowledge base.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum number of sources to return (default 20)",
			},
		},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			Limit int `json:"limit,omitempty"`
		}
		json.Unmarshal(rawArgs, &args)
		limit := args.Limit
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
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Knowledge base (%d sources):\n\n", len(sources)))
		for _, s := range sources {
			sb.WriteString(fmt.Sprintf("- %s (id: %s, type: %s, entries: %d, chars: %d)\n",
				s.Title, s.ID[:12], s.SourceType, s.EntryCount, s.TotalChars))
		}
		return sb.String(), nil
	})
}

func registerKBDelete(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_delete", "Delete a source and all its entries from the knowledge base.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"source_id": map[string]interface{}{
				"type":        "string",
				"description": "The ID of the source to delete",
			},
		},
		"required": []string{"source_id"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			SourceID string `json:"source_id"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.SourceID == "" {
			return "", fmt.Errorf("source_id is required")
		}
		if err := store.DeleteSource(ctx, agentID, args.SourceID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Deleted source %s from knowledge base.", args.SourceID), nil
	})
}

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

var kbFetchClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: httpclient.Wrap(&http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 10 * time.Second}
			return d.DialContext(ctx, network, addr)
		},
		ResponseHeaderTimeout: 20 * time.Second,
	}),
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	},
}

func FetchURLContent(ctx context.Context, rawURL string) (title, body string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "Fluctio Agent/1.0 (KB Fetcher)")

	resp, err := kbFetchClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", "", err
	}

	html := string(data)

	// Extract title
	titleRe := regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	if m := titleRe.FindStringSubmatch(html); len(m) > 1 {
		title = strings.TrimSpace(htmlTagRe.ReplaceAllString(m[1], ""))
	}
	if title == "" {
		title = rawURL
	}

	// Strip script/style, then tags
	scriptRe := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	html = scriptRe.ReplaceAllString(html, "")
	styleRe := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	html = styleRe.ReplaceAllString(html, "")
	text := htmlTagRe.ReplaceAllString(html, " ")

	// Decode common entities
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&nbsp;", " ")

	// Collapse whitespace
	spaceRe := regexp.MustCompile(`[ \t]+`)
	text = spaceRe.ReplaceAllString(text, " ")
	nlRe := regexp.MustCompile(`\n{3,}`)
	text = nlRe.ReplaceAllString(text, "\n\n")
	body = strings.TrimSpace(text)

	return title, body, nil
}
