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

func RegisterKBTools(r *tools.Registry, store *KBStore, agentID string, sourceRatioFn func() float64, thresholdFn func() float64) {
	registerKBSearch(r, store, agentID, sourceRatioFn, thresholdFn)
	registerKBSearchRaw(r, store, agentID)
	registerKBAdd(r, store, agentID)
	registerKBIngestURL(r, store, agentID)
	registerKBList(r, store, agentID)
	registerKBDelete(r, store, agentID)
}

func registerKBSearch(r *tools.Registry, store *KBStore, agentID string, sourceRatioFn func() float64, thresholdFn func() float64) {
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
		results, err := store.Search(ctx, agentID, args.Query, limit, 0, resolveRatio(sourceRatioFn), resolveThreshold(thresholdFn))
		if err != nil {
			return "", err
		}
		if len(results) == 0 {
			return "No matching entries found in the knowledge base.", nil
		}
		deduped, ids := numberAndAccumulate(ctx, results)
		if len(deduped) == 0 {
			return "All matching sources were already cited earlier; no new results.", nil
		}
		return formatResults(deduped, args.Query, ids), nil
	})
}

// numberAndAccumulate dedups results by title against the ctx accumulator: a
// source already cited earlier this turn (by auto-query or a prior tool call)
// is dropped — NOT re-fed to the LLM — so wiki/raw overlap doesn't bloat the
// context with repeated content. Fresh sources get continuing [K#] ids
// (len(acc)+1) and are appended. Returns the deduped results + their ids.
func numberAndAccumulate(ctx context.Context, results []KBResult) ([]KBResult, []string) {
	acc := SourcesFromCtx(ctx)
	cited := map[string]bool{}
	if acc != nil {
		for _, s := range *acc {
			cited[s.File] = true
		}
	}
	nextID := 1
	if acc != nil {
		nextID = len(*acc) + 1
	}
	var deduped []KBResult
	var ids []string
	seen := map[string]bool{} // dedup within this batch too
	for _, r := range results {
		if seen[r.SourceTitle] || cited[r.SourceTitle] {
			continue
		}
		seen[r.SourceTitle] = true
		id := fmt.Sprintf("K%d", nextID)
		nextID++
		deduped = append(deduped, r)
		ids = append(ids, id)
		if acc != nil {
			*acc = append(*acc, KnowledgeSource{
				ID: id, File: r.SourceTitle, Kind: r.SourceKind, PageType: r.PageType, Chunk: r.ChunkIndex,
			})
		}
	}
	return deduped, ids
}

// resolveRatio reads the agent's configured source ratio, defaulting to 0.5.
func resolveRatio(fn func() float64) float64 {
	if fn != nil {
		if r := fn(); r >= 0 && r <= 1 {
			return r
		}
	}
	return 0.5
}

// resolveThreshold reads the agent's configured relevance threshold,
// defaulting to 0.45.
func resolveThreshold(fn func() float64) float64 {
	if fn != nil {
		if t := fn(); t >= 0 && t <= 1 {
			return t
		}
	}
	return 0.45
}

func registerKBSearchRaw(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_search_raw", "Fetch the RAW original text chunks of specific knowledge-base sources (verbatim, from kb_entries). Call this AFTER knowledgebase_search, passing the source_id values it returned, when the wiki-based summary is not detailed enough and you need the exact original wording/passages. Optionally narrow with a query phrase. Returns verbatim source chunks with [K#] citation ids.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"source_ids": map[string]interface{}{
				"type":        "array",
				"items":       map[string]interface{}{"type": "string"},
				"description": "source_id values from a prior knowledgebase_search result — the sources whose raw chunks you want. Required.",
			},
			"query": map[string]interface{}{
				"type":        "string",
				"description": "Optional phrase to narrow within the selected sources (LIKE match on chunk text). Omit to pull all chunks of the given sources.",
			},
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum number of raw chunks to return (default 5)",
			},
		},
		"required": []string{"source_ids"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			SourceIDs []string `json:"source_ids"`
			Query     string   `json:"query,omitempty"`
			Limit     int      `json:"limit,omitempty"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if len(args.SourceIDs) == 0 {
			return "", fmt.Errorf("source_ids is required — call knowledgebase_search first and pass the source_id values it returned")
		}
		limit := args.Limit
		if limit <= 0 {
			limit = 5
		}
		results, err := store.SearchRawKB(ctx, agentID, args.Query, args.SourceIDs, limit)
		if err != nil {
			return "", err
		}
		if len(results) == 0 {
			return "No matching raw entries found in the knowledge base.", nil
		}
		deduped, ids := numberAndAccumulate(ctx, results)
		if len(deduped) == 0 {
			return "All matching sources were already cited earlier; no new results.", nil
		}
		return formatResults(deduped, args.Query, ids), nil
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
	req.Header.Set("User-Agent", httpclient.UserAgent())

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
