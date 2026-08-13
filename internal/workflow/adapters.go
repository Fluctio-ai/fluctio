package workflow

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// ProviderLLMCaller adapts a provider.Provider to LLMCaller: it issues a single
// bare (no-tools) user-message call and returns the model's raw content, which
// the runner then parses against the node's output schema. Model / token /
// temperature are configured per caller — the tracer bullet carries no
// per-node model field; that arrives with Seam B (ticket 02).
type ProviderLLMCaller struct {
	P         provider.Provider
	Model     string
	MaxTokens int
	Temp      float64
}

// Call implements LLMCaller.
func (c *ProviderLLMCaller) Call(ctx context.Context, prompt string) (string, error) {
	resp, err := c.P.Chat(ctx,
		[]provider.Message{{Role: "user", Content: prompt}},
		nil, c.Model, c.MaxTokens, c.Temp)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// ToolExecutor is the slice of *tools.Registry the runner's tool caller needs.
// Declared locally so this package does not import internal/agent/tools (which
// is agent-scoped and heavyweight); *tools.Registry satisfies it structurally.
type ToolExecutor interface {
	Execute(ctx context.Context, name, args string) (string, error)
}

// RegistryToolCaller adapts a tool registry to ToolCaller: it serialises the
// resolved args map to JSON and delegates to Execute. The raw string the
// registry returns is parsed by the runner's parseOutput.
type RegistryToolCaller struct {
	R ToolExecutor
}

// Call implements ToolCaller.
func (c *RegistryToolCaller) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("marshal tool args: %w", err)
	}
	return c.R.Execute(ctx, name, string(b))
}

// NewProviderRunner is the production wiring: a provider-backed LLM caller and
// a registry-backed tool caller over the given persistence seam. Callers pick
// model / maxTokens / temperature; per-node model selection comes later.
func NewProviderRunner(p provider.Provider, model string, maxTokens int, temp float64, reg ToolExecutor, rs RunStore) *Runner {
	return NewRunner(
		&ProviderLLMCaller{P: p, Model: model, MaxTokens: maxTokens, Temp: temp},
		&RegistryToolCaller{R: reg},
		rs,
	)
}
