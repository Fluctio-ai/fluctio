package vision

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

// OpenAI understands images via POST /v1/chat/completions using an OpenAI-
// compatible multimodal model (gpt-4o, gpt-4o-mini, …). Any OpenAI-compatible
// endpoint works, so this also covers self-hosted proxies and third-party
// gateways that speak the same protocol — set Endpoint to point at them.
type OpenAI struct{}

func (OpenAI) Category() string { return Category }
func (OpenAI) Name() string     { return "openai" }

func (o *OpenAI) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	if req.Config.APIKey == "" {
		return toolproviders.Response{}, fmt.Errorf("openai: missing api key")
	}
	model := req.Config.Model
	if model == "" {
		model = "gpt-4o-mini"
	}

	imageURL := a.Image
	// Bare base64 (no scheme) — wrap as a PNG data URL so the
	// chat-completions image_url payload is well-formed. http(s) and
	// existing data: URLs pass through unchanged.
	if !strings.HasPrefix(imageURL, "http://") && !strings.HasPrefix(imageURL, "https://") && !strings.HasPrefix(imageURL, "data:") {
		imageURL = "data:image/png;base64," + strings.TrimSpace(imageURL)
	}

	body := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": a.Question},
					{"type": "image_url", "image_url": map[string]any{"url": imageURL}},
				},
			},
		},
		"max_tokens": 1000,
	}
	endpoint := "https://api.openai.com/v1/chat/completions"
	if req.Config.Endpoint != "" {
		endpoint = req.Config.Endpoint
	}
	buf, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return toolproviders.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.Config.APIKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("openai vision request: %w", err))
	}
	defer resp.Body.Close()
	if err := retriableHTTP("openai", resp); err != nil {
		return toolproviders.Response{}, err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return toolproviders.Response{}, fmt.Errorf("openai vision decode: %w", err)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return toolproviders.Response{}, toolproviders.ErrNoResults
	}
	return toolproviders.Response{Text: out.Choices[0].Message.Content}, nil
}

func retriableHTTP(name string, resp *http.Response) error {
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	err := fmt.Errorf("%s HTTP %d: %s", name, resp.StatusCode, string(body))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode >= 500:
		return toolproviders.Retry(err)
	default:
		return err
	}
}
