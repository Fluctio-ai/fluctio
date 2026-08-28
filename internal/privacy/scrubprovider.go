package privacy

import (
	"context"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// ScopedProvider reads the (owner, agent)-scoped privacy config and wraps
// base so its calls scrub PII per that row (missing row = off, zero value).
// The single entry point for background pipelines that consume raw
// conversation or article content outside the interactive loop's scrub
// point — wiki/cards autogen, KB insights/merge, skill-learner extraction
// all route through here, so a scope-key or default change lands once.
func ScopedProvider(ctx context.Context, st store.Store, ownerUserID, agentID string, base provider.Provider) provider.Provider {
	var priv config.PrivacyCfg
	_ = scope.SettingInto(ctx, st, "privacy", ownerUserID, agentID, &priv)
	return WrapProvider(base, Options{Entropy: priv.PIIScrubbing.Entropy}, priv.PIIScrubbing.Enabled)
}

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
