package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// providerConfigForAgent resolves the raw config.ProviderConfig that backs
// the given agent's current model. Walks system → user(owner) → agent
// provider scopes (agent wins) via scope.Providers, matching the chat
// path and handlers_wiki.go providerForAgent — but returns the raw
// ProviderConfig (APIKey/APIBase/APIType) instead of a wrapped
// provider.Provider, so fetchUpstreamModelIDs can issue a list-models
// call directly.
func (s *Server) providerConfigForAgent(ctx context.Context, agentID string) (config.ProviderConfig, error) {
	if s.dataStore == nil {
		return config.ProviderConfig{}, fmt.Errorf("data store unavailable")
	}

	var ownerUserID string
	if agentID != "" {
		if ag, err := s.dataStore.GetAgent(ctx, agentID); err == nil && ag != nil {
			ownerUserID = ag.UserID
		}
	}

	providerMap, err := scope.Providers(ctx, s.dataStore, ownerUserID, agentID)
	if err != nil {
		return config.ProviderConfig{}, fmt.Errorf("load providers: %w", err)
	}
	if len(providerMap) == 0 {
		return config.ProviderConfig{}, fmt.Errorf("no providers configured for agent %q", agentID)
	}

	// Read agent-level model override first, fall back to system default.
	model := ""
	if agentID != "" {
		if rec, err := s.dataStore.GetConfigByName(ctx, store.KindSetting, "", agentID, "agents.defaults"); err == nil && rec != nil {
			model, _ = rec.Data["model"].(string)
		}
	}
	if model == "" {
		rec, err := s.dataStore.GetConfigByName(ctx, store.KindSetting, "", "", "agents.defaults")
		if err != nil || rec == nil {
			return config.ProviderConfig{}, fmt.Errorf("no agents.defaults model configured")
		}
		model, _ = rec.Data["model"].(string)
	}
	if model == "" {
		return config.ProviderConfig{}, fmt.Errorf("agent model is empty")
	}

	// Parse "provider/model" — the first segment names the provider key.
	parts := strings.SplitN(model, "/", 2)
	if len(parts) != 2 || parts[0] == "" {
		return config.ProviderConfig{}, fmt.Errorf("model %q has no provider prefix", model)
	}
	p, ok := providerMap[parts[0]]
	if !ok {
		return config.ProviderConfig{}, fmt.Errorf("provider %q not found in resolved scope", parts[0])
	}
	if p.APIKey == "" {
		return config.ProviderConfig{}, fmt.Errorf("provider %q has no API key", parts[0])
	}
	return p, nil
}

// fetchUpstreamModelIDs calls the upstream list-models endpoint for the
// given provider. Supports openai-compatible, gemini, and anthropic;
// other apiTypes return an error so the handler can surface 501.
//
// The returned IDs are raw upstream names (no "provider/" prefix) so
// callers can feed them straight into config.LookupModelMeta.
func fetchUpstreamModelIDs(ctx context.Context, prov config.ProviderConfig) ([]string, error) {
	apiType := prov.APIType
	// NormalizeAPIBase: trims trailing /, adds /v1 for bare openai hosts,
	// strips /v1 for anthropic (anthropic URLs include /v1 in the path).
	base := provider.NormalizeAPIBase(prov.APIBase, apiType)

	switch apiType {
	case "anthropic-messages":
		// Anthropic's list-models endpoint lives at /v1/models.
		// NormalizeAPIBase strips any trailing /v1 for this apiType,
		// so re-append it here.
		return fetchIDs(ctx, base+"/v1/models", prov.APIKey, "2023-06-01")
	case "gemini":
		return fetchIDs(ctx, base+"/models?key="+prov.APIKey, "", "")
	case "openai", "":
		// OpenAI-compatible: /v1/models with Bearer auth. For hosts
		// where NormalizeAPIBase appended /v1 already, this becomes
		// /v1/v1/models — which is wrong. Trim /v1 first then add it
		// back unconditionally to handle both bare and /v1-bearing
		// apiBase values consistently.
		return fetchIDs(ctx, strings.TrimSuffix(base, "/v1")+"/v1/models", "Bearer "+prov.APIKey, "")
	default:
		return nil, fmt.Errorf("unsupported apiType %q for list models", apiType)
	}
}

// fetchIDs issues a GET to url with the appropriate auth headers and
// extracts model IDs from either the OpenAI-shaped `data[].id` array
// or the Gemini-shaped `models[].name` array.
//
// anthropicVersion, when non-empty, switches auth from `Authorization:
// Bearer <k>` to `x-api-key: <k>` + `anthropic-version: <v>` (Anthropic
// convention). bearerKey is expected to already include any "Bearer "
// prefix for the openai path; the anthropic path strips it back off.
func fetchIDs(ctx context.Context, url, bearerKey, anthropicVersion string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if bearerKey != "" {
		if anthropicVersion != "" {
			req.Header.Set("x-api-key", strings.TrimPrefix(bearerKey, "Bearer "))
			req.Header.Set("anthropic-version", anthropicVersion)
		} else {
			req.Header.Set("Authorization", bearerKey)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
		} `json:"models"` // gemini
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read upstream body: %w", err)
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse upstream list: %w", err)
	}

	ids := make([]string, 0, len(parsed.Data)+len(parsed.Models))
	for _, m := range parsed.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	for _, m := range parsed.Models {
		if m.Name != "" {
			ids = append(ids, m.Name)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("upstream returned no models")
	}
	return ids, nil
}
