package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

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
// given provider. Supports anthropic-messages, gemini, and any
// OpenAI-compatible apiType (openai-chat, "", etc.); unknown apiTypes
// also fall through to the OpenAI-compatible default, mirroring the
// dispatch pattern in provider.NewProvider and runProviderTest.
//
// The returned IDs are raw upstream names (no "provider/" prefix) so
// callers can feed them straight into config.LookupModelMeta.
func fetchUpstreamModelIDs(ctx context.Context, prov config.ProviderConfig) ([]string, error) {
	apiType := prov.APIType
	base := provider.NormalizeAPIBase(prov.APIBase, apiType)

	switch apiType {
	case "anthropic-messages":
		// Anthropic's list-models endpoint lives at /v1/models.
		// NormalizeAPIBase strips any trailing /v1 for this apiType,
		// so re-append it here.
		return fetchIDs(ctx, base+"/v1/models", prov.APIKey, "2023-06-01")
	case "gemini":
		// Spec requires Gemini list-models support. No gemini provider
		// preset exists in the codebase today, but a custom provider
		// configured with apiType=gemini will hit this path.
		// Gemini uses query-param auth, not a bearer header.
		return fetchIDs(ctx, base+"/models?key="+url.QueryEscape(prov.APIKey), "", "")
	default:
		// OpenAI-compatible (openai-chat, "", and any future apiType).
		// Mirrors NewProvider: anything that isn't anthropic-messages is
		// treated as OpenAI-compatible. NormalizeAPIBase already ensures
		// /v1 is in the base for bare hosts, so we just append /models.
		return fetchIDs(ctx, base+"/models", "Bearer "+prov.APIKey, "")
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

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
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

// handleFetchModelsByConfig is the global-scope counterpart of
// handleFetchProviderModels. Instead of resolving the provider from an
// agent's model binding, it takes the provider config directly from the
// request body (apiBase / apiKey / apiType) — or, when apiKey is empty,
// resolves the stored key from an existing provider row referenced by
// providerId (mirrors handleTestStoredProvider's stored-key path).
//
// This backs the "Fetch model list" button on the global /models page,
// where the operator is editing a provider config that may not be saved
// yet (new provider) or may already have a key on disk (edit mode).
//
// Returns 200 [{id, contextWindow}] (never nil — empty list encodes as
// `[]`); 400 when neither apiKey nor providerId is supplied; 501 when
// the upstream is unreachable or returns no models.
func (s *Server) handleFetchModelsByConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIBase    string `json:"apiBase"`
		APIKey     string `json:"apiKey"`
		APIType    string `json:"apiType"`
		ProviderID string `json:"providerId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}

	prov := config.ProviderConfig{
		APIBase: body.APIBase,
		APIType: body.APIType,
	}

	if body.APIKey != "" {
		// Inline key path: trust what the form has. Used during Add
		// (key freshly typed) or when the operator pastes a new key
		// into the Edit dialog's key field before clicking Fetch.
		prov.APIKey = body.APIKey
	} else if body.ProviderID != "" {
		// Stored-key path: the Edit dialog sends the saved provider's
		// id when the key field was left untouched (useStoredKey mode).
		// Resolve the row server-side — the unmasked key never reaches
		// the browser. Body apiBase / apiType override the stored values
		// so the operator can test edits before saving (same semantics
		// as handleTestStoredProvider).
		rec, err := s.dataStore.GetConfig(r.Context(), body.ProviderID)
		if err != nil || rec == nil || rec.Kind != store.KindProvider {
			jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "provider not found"})
			return
		}
		if !s.authorizeScope(w, r, rec.LegacyScope(), rec.LegacyScopeID(), scopeRead) {
			return
		}
		var stored config.ProviderConfig
		if blob, err := json.Marshal(rec.Data); err == nil {
			_ = json.Unmarshal(blob, &stored)
		}
		prov.APIKey = stored.APIKey
		if prov.APIBase == "" {
			prov.APIBase = stored.APIBase
		}
		if prov.APIType == "" {
			prov.APIType = stored.APIType
		}
		prov.AuthType = stored.AuthType
	} else {
		jsonResponse(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "apiKey or providerId required",
		})
		return
	}

	ids, err := fetchUpstreamModelIDs(r.Context(), prov)
	if err != nil {
		jsonResponse(w, http.StatusNotImplemented, map[string]any{
			"ok":    false,
			"error": "upstream list failed: " + err.Error(),
		})
		return
	}

	type item struct {
		ID            string `json:"id"`
		ContextWindow int    `json:"contextWindow"`
	}
	out := make([]item, 0, len(ids))
	for _, id := range ids {
		meta, _ := config.LookupModelMeta(id)
		out = append(out, item{ID: id, ContextWindow: meta.ContextWindow})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
