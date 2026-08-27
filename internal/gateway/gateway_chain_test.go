package gateway

import (
	"testing"

	"github.com/fluctio-ai/fluctio/internal/config"
)

// buildToolChainFromResolved must resolve provider credentials from the
// category-scoped key first ("image_gen|openai") so a provider shared across
// categories (openai backs image_gen, tts and vision) can carry a different
// key per category, with the legacy bare-name entry as fallback.
func TestBuildToolChainCategoryScopedProviderConfig(t *testing.T) {
	resolved := config.ResolvedAgent{
		Tools: map[string]config.ToolCategoryCfg{
			"image_gen": {Primary: "openai/gpt-image-1"},
		},
		ToolProviders: map[string]config.ToolProviderCfg{
			"image_gen|openai": {APIKey: "sk-image"},
			"openai":           {APIKey: "sk-legacy"},
		},
	}
	chain := buildToolChainFromResolved(resolved, "image_gen")
	if chain == nil {
		t.Fatal("expected a chain, got nil")
	}
	if got := chain.GetConfig("openai").APIKey; got != "sk-image" {
		t.Errorf("category-scoped entry should win, got key %q", got)
	}
}

func TestBuildToolChainLegacyBareProviderConfig(t *testing.T) {
	resolved := config.ResolvedAgent{
		Tools: map[string]config.ToolCategoryCfg{
			"image_gen": {Primary: "openai/gpt-image-1"},
		},
		ToolProviders: map[string]config.ToolProviderCfg{
			"openai": {APIKey: "sk-legacy"},
		},
	}
	chain := buildToolChainFromResolved(resolved, "image_gen")
	if chain == nil {
		t.Fatal("expected a chain, got nil")
	}
	if got := chain.GetConfig("openai").APIKey; got != "sk-legacy" {
		t.Errorf("legacy bare entry should be the fallback, got key %q", got)
	}
}
