package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// ScoredDocument is a document index + relevance score from a reranker.
type ScoredDocument struct {
	Index int
	Score float64
}

// Reranker re-ranks candidate documents against a query using a
// cross-encoder model. Used as the second stage of the recall pipeline:
// coarse retrieval → Reranker.Rerank → top-K.
type Reranker interface {
	Rerank(ctx context.Context, query string, documents []string, topN int) ([]ScoredDocument, error)
	Model() string
	Available() bool
}

// nilReranker is the no-op default.
type nilReranker struct{}

func (nilReranker) Rerank(ctx context.Context, query string, documents []string, topN int) ([]ScoredDocument, error) {
	return nil, fmt.Errorf("reranker not configured")
}
func (nilReranker) Model() string   { return "" }
func (nilReranker) Available() bool { return false }

// JinaReranker hits Jina AI's /v1/rerank endpoint.
// Also compatible with Cohere's /v1/rerank (same request/response shape).
type JinaReranker struct {
	apiBase string // e.g. "https://api.jina.ai/v1"
	apiKey  string
	model   string // e.g. "jina-reranker-v2-base-multilingual"
	client  *http.Client
}

// NewJinaReranker creates a reranker for Jina (or Cohere) compatible APIs.
func NewJinaReranker(apiBase, apiKey, model string) *JinaReranker {
	return &JinaReranker{
		apiBase: apiBase,
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (r *JinaReranker) Model() string   { return r.model }
func (r *JinaReranker) Available() bool { return r.apiBase != "" && r.apiKey != "" }

type rerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n,omitempty"`
}

type rerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
}

// Rerank sends documents to the reranker API and returns scored indices.
func (r *JinaReranker) Rerank(ctx context.Context, query string, documents []string, topN int) ([]ScoredDocument, error) {
	if !r.Available() {
		return nil, fmt.Errorf("reranker not configured")
	}
	if len(documents) == 0 {
		return nil, nil
	}

	body, err := json.Marshal(rerankRequest{
		Model:     r.model,
		Query:     query,
		Documents: documents,
		TopN:      topN,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", r.apiBase+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.apiKey)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		var errBody bytes.Buffer
		errBody.ReadFrom(resp.Body)
		return nil, fmt.Errorf("rerank API %d: %s", resp.StatusCode, errBody.String())
	}

	var out rerankResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	results := make([]ScoredDocument, len(out.Results))
	for i, r := range out.Results {
		results[i] = ScoredDocument{Index: r.Index, Score: r.RelevanceScore}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	return results, nil
}
