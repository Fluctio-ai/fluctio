package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Embedder generates embeddings for text via an external API.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
	Dim() int
	Available() bool
}

// nilEmbedder is the no-op default when no embedding provider is configured.
type nilEmbedder struct{}

func (nilEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return nil, fmt.Errorf("embedding not configured")
}
func (nilEmbedder) Model() string   { return "" }
func (nilEmbedder) Dim() int        { return 1024 }
func (nilEmbedder) Available() bool { return false }

// OpenAICompatEmbedder hits /v1/embeddings on any OpenAI-compatible server.
type OpenAICompatEmbedder struct {
	apiBase string
	apiKey  string
	model   string
	dim     int
	// sendDim controls whether the `dimensions` request param is sent. Most
	// APIs reject it (SiliconFlow bge-m3 → 400 "parameter invalid"); only
	// some (Qwen3-Embedding) accept a non-native dim. dim is always the
	// expected vector length used by the startup probe.
	sendDim bool
	client  *http.Client
}

// NewOpenAICompatEmbedder creates an embedder for an OpenAI-compatible API.
// apiBase is the full base URL (e.g. "https://api.openai.com/v1").
// dim defaults to 1024 when set to 0.
// sendDim true sends the `dimensions` param; false omits it.
func NewOpenAICompatEmbedder(apiBase, apiKey, model string, dim int, sendDim bool) *OpenAICompatEmbedder {
	if dim == 0 {
		dim = 1024
	}
	return &OpenAICompatEmbedder{
		apiBase: apiBase,
		apiKey:  apiKey,
		model:   model,
		dim:     dim,
		sendDim: sendDim,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (e *OpenAICompatEmbedder) Model() string   { return e.model }
func (e *OpenAICompatEmbedder) Dim() int        { return e.dim }
func (e *OpenAICompatEmbedder) Available() bool { return e.apiBase != "" && e.apiKey != "" }

type openAIEmbedRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type openAIEmbedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed sends texts to the /v1/embeddings endpoint and returns vectors.
func (e *OpenAICompatEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if !e.Available() {
		return nil, fmt.Errorf("embedder not configured")
	}
	if len(texts) == 0 {
		return nil, nil
	}

	embReq := openAIEmbedRequest{
		Model: e.model,
		Input: texts,
	}
	if e.sendDim {
		embReq.Dimensions = e.dim
	}
	body, err := json.Marshal(embReq)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", e.apiBase+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		var errBody bytes.Buffer
		errBody.ReadFrom(resp.Body)
		return nil, fmt.Errorf("embedding API %d: %s", resp.StatusCode, errBody.String())
	}

	var out openAIEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embedding count mismatch: %d texts, %d embeddings", len(texts), len(out.Data))
	}

	result := make([][]float32, len(out.Data))
	for i, d := range out.Data {
		result[i] = d.Embedding
	}
	return result, nil
}
