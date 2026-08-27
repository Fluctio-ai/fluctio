package setup

import (
	"testing"

	"github.com/fluctio-ai/fluctio/internal/config"
)

func TestExpandLegacyToolProviders(t *testing.T) {
	cats := []categoryCatalog{
		{Name: "image_gen", Providers: []providerCatalog{{Name: "openai"}, {Name: "fal"}}},
		{Name: "tts", Providers: []providerCatalog{{Name: "openai"}}},
		{Name: "vision", Providers: []providerCatalog{{Name: "openai"}}},
		{Name: "web_search", Providers: []providerCatalog{{Name: "firecrawl"}}},
		{Name: "web_fetch", Providers: []providerCatalog{{Name: "firecrawl"}, {Name: "direct"}}},
	}

	// Legacy shared form: one bare "openai" entry backs three categories.
	// After expansion each category has its own copy and the bare entry is
	// gone so the next save persists the split form.
	providers := map[string]config.ToolProviderCfg{
		"openai":    {APIKey: "sk-shared"},
		"firecrawl": {APIKey: "fc"},
		"exa":       {APIKey: "sk-exa"},
	}
	expandLegacyToolProviders(cats, providers)

	for _, key := range []string{"image_gen|openai", "tts|openai", "vision|openai", "web_search|firecrawl", "web_fetch|firecrawl"} {
		if got := providers[key]; got.APIKey == "" {
			t.Errorf("expected expanded entry %q, got %+v", key, got)
		}
	}
	if _, ok := providers["openai"]; ok {
		t.Error("bare \"openai\" should be removed from the view after expansion")
	}
	if _, ok := providers["firecrawl"]; ok {
		t.Error("bare \"firecrawl\" should be removed from the view after expansion")
	}
	// "exa" matches no provider in the catalog categories passed in — it
	// must pass through untouched (never drop data).
	if got := providers["exa"]; got.APIKey != "sk-exa" {
		t.Errorf("entry matching no catalog provider should be kept, got %+v", got)
	}
	if len(providers) != 6 {
		t.Errorf("expected 6 entries after expansion, got %d: %v", len(providers), providers)
	}
}

func TestExpandLegacyToolProvidersKeepsCategoryScopedEntry(t *testing.T) {
	cats := []categoryCatalog{
		{Name: "image_gen", Providers: []providerCatalog{{Name: "openai"}}},
		{Name: "tts", Providers: []providerCatalog{{Name: "openai"}}},
	}
	providers := map[string]config.ToolProviderCfg{
		"image_gen|openai": {APIKey: "sk-image"},
		"openai":           {APIKey: "sk-legacy"},
	}
	expandLegacyToolProviders(cats, providers)

	if got := providers["image_gen|openai"]; got.APIKey != "sk-image" {
		t.Errorf("existing category-scoped entry should win, got %+v", got)
	}
	// tts has no scoped entry yet — it inherits the legacy shared value.
	if got := providers["tts|openai"]; got.APIKey != "sk-legacy" {
		t.Errorf("missing category entry should expand from legacy bare entry, got %+v", got)
	}
}
