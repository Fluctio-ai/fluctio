package privacy

import (
	"context"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// WrapProvider returns a provider whose outgoing messages are
// PII-scrubbed when enabled. Background LLM pipelines (auto-persist,
// topic summaries, compaction, wiki/card generation, article insights,
// diary, skill-learner extraction) consume the same conversation content
// the interactive loop scrubs — but they call prov.Chat outside that
// loop, so without this wrapper the privacy.piiScrubbing switch was
// silently bypassed by every one of them. disabled wraps to the original
// provider's behavior with zero copies.
type scrubProvider struct {
	provider.Provider // embed: other methods pass through
	opts    Options
	enabled bool
}

// WrapProvider is the constructor for the scrubbing wrapper. enabled
// toggles scrubbing; opts carries the entropy detection flag.
func WrapProvider(p provider.Provider, opts Options, enabled bool) provider.Provider {
	if !enabled || p == nil {
		return p
	}
	return scrubProvider{Provider: p, opts: opts, enabled: enabled}
}

// Chat scrubs every outgoing message before delegating.
func (s scrubProvider) Chat(ctx context.Context, messages []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.Response, error) {
	return s.Provider.Chat(ctx, ScrubMessages(messages, s.opts), tools, model, maxTokens, temperature)
}

// ChatStream scrubs the same way for streaming callers.
func (s scrubProvider) ChatStream(ctx context.Context, messages []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.StreamReader, error) {
	return s.Provider.ChatStream(ctx, ScrubMessages(messages, s.opts), tools, model, maxTokens, temperature)
}
