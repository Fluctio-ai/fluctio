package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/toolproviders"
)

// Exa calls api.exa.ai. The x-api-key header authenticates; the request body
// controls result count and search mode. Model (from "exa/<mode>") is either
// empty, "auto", "keyword" or "neural".
type Exa struct{}

func (Exa) Category() string { return Category }
func (Exa) Name() string     { return "exa" }

func (e *Exa) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	if req.Config.APIKey == "" {
		return toolproviders.Response{}, fmt.Errorf("exa: missing api key")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	mode := req.Config.Model
	if mode == "" {
		mode = "auto"
	}
	buf, _ := json.Marshal(exaRequestBody(a.Query, a.Count, mode))
	// Default Exa endpoint. Override via Config.Endpoint with the full URL
	// (e.g. a self-hosted Exa-compatible service). Used verbatim — no path
	// appended — same convention as image_gen/tts, so every provider treats
	// Endpoint as the complete request URL.
	endpoint := "https://api.exa.ai/search"
	if req.Config.Endpoint != "" {
		if !strings.HasPrefix(req.Config.Endpoint, "https://") {
			return toolproviders.Response{}, fmt.Errorf("exa: endpoint must be https")
		}
		endpoint = req.Config.Endpoint
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return toolproviders.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", req.Config.APIKey)

	resp, err := safeWebsearchClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("exa request: %w", err))
	}
	defer resp.Body.Close()
	if err := retriableHTTP("exa", resp); err != nil {
		return toolproviders.Response{}, err
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return toolproviders.Response{}, fmt.Errorf("read response: %w", err)
	}
	items, err := parseExaResults(respBody)
	if err != nil {
		return toolproviders.Response{}, err
	}
	if len(items) == 0 {
		return toolproviders.Response{}, toolproviders.ErrNoResults
	}
	return toolproviders.Response{Text: render(a.Query, items)}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// exaRequestBody builds the /search payload. Exa returns bare metadata
// (title/url) unless contents is asked for explicitly — without it the
// snippet parsed from results[].text was permanently empty and search
// quality degraded silently.
func exaRequestBody(query string, count int, mode string) map[string]any {
	return map[string]any{
		"query":      query,
		"numResults": count,
		"type":       mode,
		"contents":   map[string]any{"text": true},
	}
}

// parseExaResults maps the /search response to render items. Snippet
// prefers the requested text body and falls back to summary so a results
// shape change degrades loudly (non-empty snippet) rather than rendering
// bare title/url rows.
func parseExaResults(data []byte) ([]resultItem, error) {
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Text    string `json:"text"`
			Summary string `json:"summary"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("exa decode: %w", err)
	}
	items := make([]resultItem, 0, len(out.Results))
	for _, r := range out.Results {
		snippet := r.Text
		if snippet == "" {
			snippet = r.Summary
		}
		items = append(items, resultItem{Title: r.Title, URL: r.URL, Snippet: truncate(snippet, 280)})
	}
	return items, nil
}
