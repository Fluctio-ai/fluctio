package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/fluctio-ai/fluctio/internal/toolproviders"
)

// Firecrawl calls api.firecrawl.dev's /v2/search endpoint. Unlike the
// web_fetch Firecrawl provider (which uses /v1/scrape to render a single
// page), this one runs a keyword search and returns normalized results.
// Requires an API key (Bearer). Override the endpoint via Config.Endpoint
// with the full URL — same convention as the other search providers.
type Firecrawl struct{}

func (Firecrawl) Category() string { return Category }
func (Firecrawl) Name() string     { return "firecrawl" }

func (f *Firecrawl) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	if req.Config.APIKey == "" {
		return toolproviders.Response{}, fmt.Errorf("firecrawl: missing api key")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	body := map[string]any{
		"query": a.Query,
		"limit": a.Count,
	}
	buf, _ := json.Marshal(body)
	endpoint := "https://api.firecrawl.dev/v2/search"
	if req.Config.Endpoint != "" {
		endpoint = req.Config.Endpoint
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return toolproviders.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.Config.APIKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("firecrawl search: %w", err))
	}
	defer resp.Body.Close()
	if err := retriableHTTP("firecrawl", resp); err != nil {
		return toolproviders.Response{}, err
	}
	var out struct {
		Success bool `json:"success"`
		Data    struct {
			// /v2/search returns data as an object keyed by source. "web"
			// is the default source array (present without scrapeOptions);
			// "images" / "news" only appear when requested via sources.
			Web []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Markdown    string `json:"markdown"`
				Description string `json:"description"`
			} `json:"web"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return toolproviders.Response{}, fmt.Errorf("firecrawl decode: %w", err)
	}
	items := make([]resultItem, 0, len(out.Data.Web))
	for _, r := range out.Data.Web {
		snippet := r.Markdown
		if snippet == "" {
			snippet = r.Description
		}
		items = append(items, resultItem{Title: r.Title, URL: r.URL, Snippet: truncate(snippet, 280)})
	}
	if len(items) == 0 {
		return toolproviders.Response{}, toolproviders.ErrNoResults
	}
	return toolproviders.Response{Text: render(a.Query, items)}, nil
}
