package setup

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
	"github.com/fluctio-ai/fluctio/internal/auth"
	"github.com/fluctio-ai/fluctio/internal/buildinfo"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/users"
	"github.com/fluctio-ai/fluctio/internal/workspace"
)

// agentShareModelConfig reports whether the agent's owner has opted to
// share their model + provider configuration with chatters. Default
// true: when the key is absent from rec.Config, sharing is on. Owners
// explicitly opt OUT by writing `false`. Centralised here so the API
// layer, the runtime overlay gate (EnsureAgent), and the listProviders
// auth relaxation read the flag with one consistent default.
func agentShareModelConfig(rec *store.AgentRecord) bool {
	if rec == nil {
		return true
	}
	v, ok := rec.Config["shareModelConfig"].(bool)
	if !ok {
		return true
	}
	return v
}

// agentScopeModel reads the per-agent model override from the configs
// table — the kind=setting, scope=agent row that supersedes the
// system/user defaults when set.
func (s *Server) agentScopeModel(r *http.Request, agentID string) string {
	rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, agentID, "agents.defaults")
	if err != nil || rec == nil {
		return ""
	}
	if v, ok := rec.Data["model"].(string); ok {
		return v
	}
	return ""
}

// saveAgentScopeModel upserts (model="") or deletes (model=="") the
// agent-scope agents.defaults row.
func (s *Server) saveAgentScopeModel(r *http.Request, agentID, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return scope.SaveSettingByScope(r.Context(), s.dataStore, scope.Agent, agentID, "agents.defaults", nil)
	}
	return scope.SaveSettingByScope(r.Context(), s.dataStore, scope.Agent, agentID, "agents.defaults", map[string]interface{}{"model": model})
}

// agentScopeDefaultsRead returns the current agent-scope agents.defaults
// row data, or an empty map if the row doesn't exist yet. Callers use
// this as the base for merge-aware patches (read-modify-write) so a
// single PATCH that touches one field doesn't clobber the rest.
func (s *Server) agentScopeDefaultsRead(r *http.Request, agentID string) map[string]interface{} {
	rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, agentID, "agents.defaults")
	if err != nil || rec == nil || rec.Data == nil {
		return map[string]interface{}{}
	}
	// Copy so callers mutating the result don't accidentally write
	// back through the cached store object.
	out := make(map[string]interface{}, len(rec.Data))
	for k, v := range rec.Data {
		out[k] = v
	}
	return out
}

// applyAgentScopeDefaultsPatch merges patch into the current
// agents.defaults row and writes the result. Keys whose value is nil are
// DELETED from the row (the caller's signal for "clear this override").
// A row that ends up empty is removed entirely so MergedAgentConfig
// falls all the way back to system/user defaults.
func (s *Server) applyAgentScopeDefaultsPatch(r *http.Request, agentID string, patch map[string]interface{}) error {
	if len(patch) == 0 {
		return nil
	}
	data := s.agentScopeDefaultsRead(r, agentID)
	for k, v := range patch {
		if v == nil {
			delete(data, k)
			continue
		}
		data[k] = v
	}
	if len(data) == 0 {
		return scope.SaveSettingByScope(r.Context(), s.dataStore, scope.Agent, agentID, "agents.defaults", nil)
	}
	return scope.SaveSettingByScope(r.Context(), s.dataStore, scope.Agent, agentID, "agents.defaults", data)
}

// applyAgentScopePluginsPatch merges per-agent plugin enable
// overrides into the (scope=agent, name=plugins.enabled) row.
//
// patch keys whose value is true/false are written; the rest of the
// row is preserved (so a UI toggle for one plugin doesn't clobber
// overrides for sibling plugins). When reset is true, the entire row
// is dropped — agent falls back to system-wide plugin enable state.
func (s *Server) applyAgentScopePluginsPatch(r *http.Request, agentID string, patch map[string]bool, reset bool) error {
	if reset {
		return scope.SaveSettingByScope(r.Context(), s.dataStore, scope.Agent, agentID, "plugins.enabled", nil)
	}
	if len(patch) == 0 {
		return nil
	}
	data := map[string]interface{}{}
	if rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, agentID, "plugins.enabled"); err == nil && rec != nil {
		for k, v := range rec.Data {
			data[k] = v
		}
	}
	for k, v := range patch {
		data[k] = v
	}
	return scope.SaveSettingByScope(r.Context(), s.dataStore, scope.Agent, agentID, "plugins.enabled", data)
}

// agentScopeSplitReplies reads the per-agent multi-bubble override.
// Returns nil when absent — nil is treated as false by every runtime
// consumer, so the distinction only matters for the GET response (the
// dashboard could choose to render "unset" differently from "off", but
// today the Switch renders both as off and that's fine).
func (s *Server) agentScopeSplitReplies(r *http.Request, agentID string) *bool {
	rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, agentID, "agents.defaults")
	if err != nil || rec == nil {
		return nil
	}
	v, ok := rec.Data["splitReplies"].(bool)
	if !ok {
		return nil
	}
	return &v
}

// agentScopePromptMode reads the per-agent promptMode override.
func (s *Server) agentScopePromptMode(r *http.Request, agentID string) string {
	rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, agentID, "agents.defaults")
	if err != nil || rec == nil {
		return ""
	}
	if v, ok := rec.Data["promptMode"].(string); ok {
		return v
	}
	return ""
}

// agentScopeGuidance reads the per-agent guidance override ("autonomous"
// vs "guided"). Empty = no override; callers fall back to the default
// ("guided") at prompt-build time.
func (s *Server) agentScopeGuidance(r *http.Request, agentID string) string {
	rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, agentID, "agents.defaults")
	if err != nil || rec == nil {
		return ""
	}
	if v, ok := rec.Data["guidance"].(string); ok {
		return v
	}
	return ""
}

// agentScopePlugins reads the per-agent plugin enable overlay. Returns
// nil when no row exists. Keyed pluginID → bool; missing keys fall
// through to the system-wide plugin entry's enabled state.
func (s *Server) agentScopePlugins(r *http.Request, agentID string) map[string]bool {
	rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, agentID, "plugins.enabled")
	if err != nil || rec == nil {
		return nil
	}
	out := make(map[string]bool, len(rec.Data))
	for k, v := range rec.Data {
		if b, ok := v.(bool); ok {
			out[k] = b
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// agentScopeAutoPersist reads the per-agent autoPersist override.
// Returns nil when absent — same convention as agentScopeSplitReplies.
// Drives the runPostTurn AutoPersistMemory pass (LLM-distilled writes to
// USER.md / MEMORY.md) which is the only chatter-memory persistence
// path in chatbot mode.
func (s *Server) agentScopeAutoPersist(r *http.Request, agentID string) *bool {
	rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, agentID, "agents.defaults")
	if err != nil || rec == nil {
		return nil
	}
	v, ok := rec.Data["autoPersist"].(bool)
	if !ok {
		return nil
	}
	return &v
}

// agentScopeSharedIdentity returns true when ANY channel bound to this
// agent has shared_identity enabled. The toggle is conceptually agent-
// level (Context page) but physically stored per-channel so the gateway
// routing hot-path can read it without an extra DB lookup.
func (s *Server) agentScopeSharedIdentity(r *http.Request, ownerUserID, agentID string) bool {
	chs, err := s.dataStore.ListChannels(r.Context(), ownerUserID, agentID)
	if err != nil {
		return false
	}
	for _, ch := range chs {
		if ch.SharedIdentity {
			return true
		}
	}
	return false
}

// effectiveUserID returns the resolved user_id for the request: the
// caller's own id, or — for super_admin in actAs mode — the impersonated
// user's id.
func (s *Server) effectiveUserID(r *http.Request) string {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		return ""
	}
	return ident.EffectiveUserID()
}

// requireWritable returns true if the caller may mutate, writing a 4xx
// response and false otherwise.
func (s *Server) requireWritable(w http.ResponseWriter, r *http.Request) bool {
	_, ok := auth.FromContext(r.Context())
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return false
	}
	return true
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	uid := s.effectiveUserID(r)
	if uid == "" {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	owned, err := s.dataStore.ListAgents(r.Context(), uid)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(owned))
	for _, ar := range owned {
		desc, _ := ar.Config["description"].(string)
		out = append(out, map[string]any{
			"id":          ar.ID,
			"name":        ar.Name,
			"description": desc,
			"model":       s.agentScopeModel(r, ar.ID),
			"avatarUrl":   "/api/agents/" + ar.ID + "/files/avatar.png",
			"createdAt":   ar.CreatedAt,
			"userId":      ar.UserID,
			"role":        "owner",
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"agents": out})
}

type createAgentRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Model       string `json:"model,omitempty"`
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	ident, _ := auth.FromContext(r.Context())
	if !ident.CanCreateAgent() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "type=agent api keys cannot create agents"})
		return
	}
	uid := s.effectiveUserID(r)
	var req createAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.Name == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "name required"})
		return
	}
	id, err := generateID("agt_")
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	rec := &store.AgentRecord{
		ID:     id,
		UserID: uid,
		Name:   req.Name,
	}
	if req.Description != "" {
		// Description lives in the agents.config JSON blob — keeps the
		// schema stable while still surfacing through GetAgentConfig and
		// the agents.config namespace settings overlay.
		rec.Config = map[string]interface{}{"description": req.Description}
	}
	if err := s.dataStore.SaveAgent(r.Context(), rec); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if req.Model != "" {
		if err := s.saveAgentScopeModel(r, id, req.Model); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	s.invalidateUser(uid)
	jsonResponse(w, http.StatusCreated, map[string]any{
		"agent": map[string]any{
			"id":     rec.ID,
			"userId": s.effectiveUserID(r),
			"name":   rec.Name,
			"model":  req.Model,
			"config": rec.Config,
		},
	})
}

// requireUserOrAdmin gates the /api/users/{id}/* nested routes:
//   - any caller may operate on themselves (pathUserID == ident.UserID)
//   - super_admin / type=admin apikey may operate on any user
//
// Returns true on success; on failure writes a 401/403 and returns false.
// Callers should still validate that the path user actually exists when
// the operation depends on it.
func (s *Server) requireUserOrAdmin(w http.ResponseWriter, r *http.Request, pathUserID string) bool {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return false
	}
	if pathUserID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "user id required"})
		return false
	}
	if pathUserID == ident.EffectiveUserID() {
		return true
	}
	if ident.CanAdminPlatform() {
		return true
	}
	jsonResponse(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
	return false
}

// requireAgentOwner returns the agent record if the caller owns it (or is
// super_admin), otherwise writes a 403/404 and returns nil.
func (s *Server) requireAgentOwner(w http.ResponseWriter, r *http.Request, agentID string) *store.AgentRecord {
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return nil
	}
	// Single-user flatten: every authenticated caller owns every agent.
	return rec
}

// requireAgentReadable allows access when the caller is the owner, a
// super_admin, holds an apikey-ACL grant (CanAccessAgent), OR the
// agent is marked public and the caller is at least an authenticated
// session. Public agents are link-shared: any signed-in user who hits
// the URL can chat under their own user_id namespace, while the
// agent's identity (SOUL/IDENTITY/skills) is reused from the owner's
// row. This is the same gate /api/chat/history uses, so app_user
// requests proxied through an integration with X-Fluctio-End-User
// can read artifacts for sessions they own without 403'ing on the
// strict ownership check.
// callerOwnsAgent returns true when the caller is the agent's owner, a
// super_admin, or an apikey explicitly scoped to the agent. Unlike
// requireAgentReadable this does NOT grant public-agent readers — used
// by file-scope code that needs to distinguish "browse everything"
// (owner) from "scope to your own session" (foreign caller on a public
// agent). Failures are silent: caller decides how to respond.
func (s *Server) callerOwnsAgent(r *http.Request, agentID string) bool {
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		return false
	}
	_ = rec
	// Single-user flatten: every authenticated caller owns every agent.
	return true
}

func (s *Server) requireAgentReadable(w http.ResponseWriter, r *http.Request, agentID string) bool {
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return false
	}
	_ = rec
	// Single-user flatten: every authenticated caller can read every agent.
	return true
}

func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	var req struct {
		Name        string  `json:"name,omitempty"`
		Description *string `json:"description,omitempty"` // ptr so empty-string clears it
		Model       *string `json:"model,omitempty"`       // ptr so empty-string clears the agent-scope override
		// PromptMode is a ptr so the caller can distinguish "leave
		// unchanged" (omitted / null) from "clear override" (empty
		// string). Allowed string values: "agent" | "chatbot" |
		// "customize" — empty falls back to system default ("agent").
		// PromptMode also drives the built-in tool surface; there is
		// no separate allowlist field by design (extend via plugins).
		PromptMode *string `json:"promptMode,omitempty"`
		// Guidance per-agent override: nil = leave unchanged; ptr-to-
		// string = "autonomous" | "guided" (empty clears the override).
		Guidance *string `json:"guidance,omitempty"`
		// SplitReplies per-agent override: nil = leave unchanged,
		// non-nil pointer-to-bool = set explicit value (true/false).
		// Distinct from "clear" which is a separate signal — the
		// dashboard sends `splitRepliesReset: true` to delete
		// the override and fall back to system default.
		SplitReplies      *bool `json:"splitReplies,omitempty"`
		SplitRepliesReset bool  `json:"splitRepliesReset,omitempty"`
		// AutoPersist per-agent override — same semantics as SplitReplies.
		// `autoPersistReset:true` clears the override and falls back to
		// system default (currently effectively disabled).
		AutoPersist      *bool `json:"autoPersist,omitempty"`
		AutoPersistReset bool  `json:"autoPersistReset,omitempty"`
		// SharedIdentity toggles cross-channel session/memory sharing.
		// When true, all channels bound to this agent use the channel
		// owner's user_id as the chatter identity, so sessions and
		// memory are shared across web + IM channels. Default false.
		SharedIdentity *bool `json:"sharedIdentity,omitempty"`
		// Plugins per-agent enable overlay. Keys are plugin IDs, values
		// are bool. Patch semantics: only the keys present in this map
		// get written; other keys in the existing row are preserved.
		// To clear all overrides for this agent, send pluginsReset:true.
		Plugins      map[string]bool `json:"plugins,omitempty"`
		PluginsReset bool            `json:"pluginsReset,omitempty"`
		// MCPServers is a whole-map replace: omit to leave untouched,
		// send {} to clear, or send the full desired map to replace.
		MCPServers      map[string]config.MCPServerConfig `json:"mcpServers,omitempty"`
		MCPServersReset bool                              `json:"mcpServersReset,omitempty"`
		// KB auto-query config (slice 4b-1). nil = leave unchanged;
		// non-nil = write the per-agent KB override. Send enabled=false
		// to disable (no separate reset signal).
		KB *config.AgentKBCfg `json:"kb,omitempty"`
		// Daily-diary generation config (nil = leave unchanged; non-nil =
		// write the per-agent diary override). Same blob pattern as KB.
		Diary *config.AgentDiaryCfg `json:"diary,omitempty"`
		// Q&A-card generation/push config (nil = leave unchanged; non-nil =
		// write the per-agent cards override). Same blob pattern as Diary.
		Cards *config.AgentCardsCfg `json:"cards,omitempty"`
		// Language is the default UI language for slash-command replies
		// when the inbound source carries none (IM channels). ptr so
		// nil = leave unchanged, empty string = clear (fall back to the
		// runtime default), "en"/"zh-CN" = set. Lives in the agent config
		// blob alongside mcpServers / kb.
		Language *string `json:"language,omitempty"`
		// CompactionMode selects the margin aggressiveness for the
		// dynamic compaction threshold: "conservative" / "balanced" /
		// "aggressive". ptr so nil = leave unchanged, empty = clear
		// (fall back to balanced default). Stored in the config blob;
		// MergedAgentConfig forwards it to ResolvedAgent.CompactionMode.
		CompactionMode *string `json:"compactionMode,omitempty"`
		// CompactionThreshold is an operator-set fixed threshold (tokens).
		// 0 = use the dynamic computation from CompactionMode. ptr so
		// nil = leave unchanged. Stored in the config blob.
		CompactionThreshold *int `json:"compactionThreshold,omitempty"`
		// MaxTokens / Temperature / MaxToolIterations are per-agent
		// overrides for LLM generation + the ReAct iteration budget. ptr
		// so nil = leave unchanged; <=0 = clear (fall back to the
		// agents.defaults entry → system default of 8192 / 0.7 / 20).
		// Stored in the agents.defaults config row next to model /
		// promptMode; MergedAgentConfig forwards them to ResolvedAgent.
		MaxTokens         *int     `json:"maxTokens,omitempty"`
		Temperature       *float64 `json:"temperature,omitempty"`
		MaxToolIterations *int     `json:"maxToolIterations,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.Name != "" {
		rec.Name = req.Name
	}
	if req.Description != nil {
		if rec.Config == nil {
			rec.Config = map[string]interface{}{}
		}
		if *req.Description == "" {
			delete(rec.Config, "description")
		} else {
			rec.Config["description"] = *req.Description
		}
	}
	// MCP servers: whole-map replace into the agent config blob.
	if req.MCPServersReset {
		if rec.Config != nil {
			delete(rec.Config, "mcpServers")
		}
	} else if req.MCPServers != nil {
		if rec.Config == nil {
			rec.Config = map[string]interface{}{}
		}
		if len(req.MCPServers) == 0 {
			delete(rec.Config, "mcpServers")
		} else {
			rec.Config["mcpServers"] = req.MCPServers
		}
	}
	// KB auto-query override lives in the agent config blob alongside
	// mcpServers; MergedAgentConfig forwards it into ResolvedAgent.KB so
	// the manager's AutoQueryHook + RegisterKBTools wiring picks it up.
	if req.KB != nil {
		if rec.Config == nil {
			rec.Config = map[string]interface{}{}
		}
		rec.Config["kb"] = req.KB
	}
	// Daily-diary override lives in the same config blob alongside kb.
	if req.Diary != nil {
		if rec.Config == nil {
			rec.Config = map[string]interface{}{}
		}
		rec.Config["diary"] = req.Diary
	}
	// Q&A-card generation/push override, same blob pattern as diary.
	if req.Cards != nil {
		if rec.Config == nil {
			rec.Config = map[string]interface{}{}
		}
		rec.Config["cards"] = req.Cards
	}
	// Language override lives in the agent config blob; MergedAgentConfig
	// forwards it into ResolvedAgent.Language so HandleMessage can fall
	// back to it when msg.Lang is empty (IM channels).
	if req.Language != nil {
		if rec.Config == nil {
			rec.Config = map[string]interface{}{}
		}
		if *req.Language == "" {
			delete(rec.Config, "language")
		} else {
			rec.Config["language"] = strings.TrimSpace(*req.Language)
		}
	}
	// CompactionMode + CompactionThreshold live in the config blob.
	// MergedAgentConfig forwards them to ResolvedAgent so the loop's
	// compactionThresholdNow picks them up. nil ptr = leave unchanged;
	// ptr-to-empty = clear (fall back to balanced default); valid
	// string = set. Only documented values are accepted so a typo
	// doesn't silently degrade to balanced.
	if req.CompactionMode != nil {
		if rec.Config == nil {
			rec.Config = map[string]interface{}{}
		}
		cm := strings.TrimSpace(*req.CompactionMode)
		switch cm {
		case "":
			delete(rec.Config, "compactionMode")
		case "conservative", "balanced", "aggressive":
			rec.Config["compactionMode"] = cm
		default:
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "compactionMode must be one of: conservative, balanced, aggressive"})
			return
		}
	}
	// CompactionThreshold: nil = leave unchanged; 0 = clear (use dynamic
	// from mode); >0 = fixed threshold.
	if req.CompactionThreshold != nil {
		if rec.Config == nil {
			rec.Config = map[string]interface{}{}
		}
		if *req.CompactionThreshold <= 0 {
			delete(rec.Config, "compactionThreshold")
		} else {
			rec.Config["compactionThreshold"] = *req.CompactionThreshold
		}
	}
	if err := s.dataStore.SaveAgent(r.Context(), rec); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// Per-agent defaults live in one configs row (kind=setting, scope=agent,
	// namespace=agents.defaults). Collect every field the caller touched
	// into a single merge-aware patch so e.g. updating promptMode doesn't
	// clobber an existing model override and vice versa. nil pointer =
	// caller didn't touch the field; ptr-to-empty = "clear this override".
	defaultsPatch := map[string]interface{}{}
	if req.Model != nil {
		m := strings.TrimSpace(*req.Model)
		if m == "" {
			defaultsPatch["model"] = nil
		} else {
			defaultsPatch["model"] = m
		}
	}
	if req.PromptMode != nil {
		pm := strings.TrimSpace(*req.PromptMode)
		// Allow only the documented values plus empty (= clear).
		// Anything else is a 400 — silently coercing to "agent" would
		// mask typos from the dashboard or CLI.
		switch pm {
		case "":
			defaultsPatch["promptMode"] = nil
		case config.PromptModeAgent, config.PromptModeChatbot, config.PromptModeCustomize:
			defaultsPatch["promptMode"] = pm
		default:
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "promptMode must be one of: agent, chatbot, customize"})
			return
		}
	}
	if req.Guidance != nil {
		g := strings.TrimSpace(*req.Guidance)
		switch g {
		case "":
			defaultsPatch["guidance"] = nil
		case "autonomous", "guided":
			defaultsPatch["guidance"] = g
		default:
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "guidance must be one of: autonomous, guided"})
			return
		}
	}
	if req.SplitRepliesReset {
		// Reset wins over set in the same request — the dashboard's
		// "Inherit" pill writes this flag.
		defaultsPatch["splitReplies"] = nil
	} else if req.SplitReplies != nil {
		defaultsPatch["splitReplies"] = *req.SplitReplies
	}
	if req.AutoPersistReset {
		defaultsPatch["autoPersist"] = nil
	} else if req.AutoPersist != nil {
		defaultsPatch["autoPersist"] = *req.AutoPersist
	}
	// Generation + loop budgets. nil = leave unchanged; <=0 = clear
	// (fall back to agents.defaults → system default).
	if req.MaxTokens != nil {
		if *req.MaxTokens <= 0 {
			defaultsPatch["maxTokens"] = nil
		} else {
			defaultsPatch["maxTokens"] = *req.MaxTokens
		}
	}
	if req.Temperature != nil {
		if *req.Temperature <= 0 {
			defaultsPatch["temperature"] = nil
		} else {
			defaultsPatch["temperature"] = *req.Temperature
		}
	}
	if req.MaxToolIterations != nil {
		if *req.MaxToolIterations <= 0 {
			defaultsPatch["maxToolIterations"] = nil
		} else {
			defaultsPatch["maxToolIterations"] = *req.MaxToolIterations
		}
	}
	if err := s.applyAgentScopeDefaultsPatch(r, rec.ID, defaultsPatch); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// Plugins-enabled overlay: separate config row (scope=agent,
	// name=plugins.enabled), so doesn't go through the agents.defaults
	// patch path. Reset clears the row entirely; otherwise we merge
	// the incoming map keys into the existing data.
	if req.PluginsReset || req.Plugins != nil {
		if err := s.applyAgentScopePluginsPatch(r, rec.ID, req.Plugins, req.PluginsReset); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	// SharedIdentity: batch-update all channels for this agent.
	if req.SharedIdentity != nil {
		chs, _ := s.dataStore.ListChannels(r.Context(), s.effectiveUserID(r), rec.ID)
		for i := range chs {
			if chs[i].SharedIdentity != *req.SharedIdentity {
				chs[i].SharedIdentity = *req.SharedIdentity
				_ = s.dataStore.SaveChannel(r.Context(), &chs[i])
			}
		}
	}
	// invalidateAgent (not invalidateUser) so super_admin / public-link
	// viewers / apikey callers that lazy-attached this agent into their
	// own UserSpace also drop their stale rc.Model — without this they
	// keep firing the previous model until the 30-min idle eviction.
	s.invalidateAgent(rec.ID)
	gen := s.agentScopeDefaultsRead(r, rec.ID)
	jsonResponse(w, http.StatusOK, map[string]any{
		"agent": map[string]any{
			"id":                rec.ID,
			"userId":            s.effectiveUserID(r),
			"name":              rec.Name,
			"model":             s.agentScopeModel(r, rec.ID),
			"promptMode":        s.agentScopePromptMode(r, rec.ID),
			"guidance":          s.agentScopeGuidance(r, rec.ID),
			"splitReplies":      s.agentScopeSplitReplies(r, rec.ID),
			"autoPersist":       s.agentScopeAutoPersist(r, rec.ID),
			"sharedIdentity":    s.agentScopeSharedIdentity(r, s.effectiveUserID(r), rec.ID),
			"plugins":           s.agentScopePlugins(r, rec.ID),
			"maxTokens":         gen["maxTokens"],
			"temperature":       gen["temperature"],
			"maxToolIterations": gen["maxToolIterations"],
			"config":            rec.Config,
		},
	})
}

// handleGetAgent returns the basic AgentRecord (id, name, description,
// userId) for one agent. Used by the chat header / sidebar switcher to
// resolve a display name. Permission is read-level — owner, super_admin,
// or any grantee of a sharing record.
func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	rec, err := s.dataStore.GetAgent(r.Context(), id)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	desc, _ := rec.Config["description"].(string)
	// Single-user: caller is always the owner.
	role := "owner"
	gen := s.agentScopeDefaultsRead(r, rec.ID)
	jsonResponse(w, http.StatusOK, map[string]any{
		"agent": map[string]any{
			"id":                rec.ID,
			"name":              rec.Name,
			"description":       desc,
			"userId":            s.effectiveUserID(r),
			"role":              role,
			"model":             s.agentScopeModel(r, rec.ID),
			"promptMode":        s.agentScopePromptMode(r, rec.ID),
			"guidance":          s.agentScopeGuidance(r, rec.ID),
			"splitReplies":      s.agentScopeSplitReplies(r, rec.ID),
			"autoPersist":       s.agentScopeAutoPersist(r, rec.ID),
			"sharedIdentity":    s.agentScopeSharedIdentity(r, s.effectiveUserID(r), rec.ID),
			"plugins":           s.agentScopePlugins(r, rec.ID),
			"maxTokens":         gen["maxTokens"],
			"temperature":       gen["temperature"],
			"maxToolIterations": gen["maxToolIterations"],
			"avatarUrl":         "/api/agents/" + rec.ID + "/files/avatar.png",
			"createdAt":         rec.CreatedAt,
		},
	})
}

func (s *Server) handleGetAgentConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	cfg := config.AgentFileConfig{}
	if len(rec.Config) > 0 {
		blob, _ := json.Marshal(rec.Config)
		_ = json.Unmarshal(blob, &cfg)
	}
	jsonResponse(w, http.StatusOK, cfg)
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	if err := s.dataStore.DeleteAgent(r.Context(), id); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// Drop the agent from every cached UserSpace, not just the owner's,
	// so foreign callers stop resolving the now-deleted agent through
	// EnsureAgent's lazy-attach path.
	s.invalidateAgent(rec.ID)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// Agent identity / memory files — all live in agent_files, agent-scoped.
// Two classes:
//
//   - identity files (agentIdentityFiles below) are the canonical "shared
//     template" for the agent. They live under a single row keyed by the
//     agent owner's user_id — so admin provisioning, the owner's edits,
//     and the agent's own BOOTSTRAP-flow write_file calls all converge on
//     the same row. Mirrors handlers_admin.forkAgentFiles and
//     internal/agent/tools.identityFiles; keep these three lists in sync.
//
//   - per-user files (USER.md, MEMORY.md) are state that genuinely
//     differs per chatter. They're keyed by the caller's effective
//     user_id; a non-owner caller can author their own override and the
//     read path falls back to the owner's row when none exists.
//
// Filename allowlist gates which files this endpoint can touch at all;
// agent-runtime tool calls go through the workspace store instead.
var agentSystemFileAllowlist = map[string]bool{
	"SOUL.md": true, "IDENTITY.md": true, "AGENTS.md": true,
	"BOOTSTRAP.md": true, "TOOLS.md": true, "MEMORY.md": true,
	"HEARTBEAT.md": true, "USER.md": true, "agent.json": true,
}

var agentIdentityFiles = map[string]bool{
	"SOUL.md": true, "IDENTITY.md": true, "AGENTS.md": true,
	"BOOTSTRAP.md": true, "TOOLS.md": true, "HEARTBEAT.md": true,
	"agent.json": true,
}

func (s *Server) handleGetAgentSystemFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	if !agentSystemFileAllowlist[name] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "filename not allowed"})
		return
	}
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	// Flattened: every system file is a single agent-scoped row, so the
	// identity-vs-per-user overlay is gone. One read — source "owner"
	// means "agent has content" (keeps the dashboard's edit badge / value
	// prefill working), "default" is the empty state.
	data, err := s.dataStore.GetAgentFile(r.Context(), id, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			jsonResponse(w, http.StatusOK, map[string]any{"content": "", "source": "default"})
			return
		}
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"content": string(data), "source": "owner"})
}

func (s *Server) handlePutAgentSystemFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	name := r.PathValue("name")
	if !agentSystemFileAllowlist[name] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "filename not allowed"})
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if !s.checkSystemFileWritable(w, r, id, name) {
		return
	}
	if err := s.dataStore.SaveAgentFile(r.Context(), id, name, []byte(body.Content)); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateAgent(id)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteAgentSystemFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	name := r.PathValue("name")
	if !agentSystemFileAllowlist[name] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "filename not allowed"})
		return
	}
	if !s.checkSystemFileWritable(w, r, id, name) {
		return
	}
	if err := s.dataStore.DeleteAgentFile(r.Context(), id, name); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateAgent(id)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// checkSystemFileWritable gates write/delete access on (agentID, name):
//
//   - Identity files (SOUL/IDENTITY/AGENTS/BOOTSTRAP/TOOLS/HEARTBEAT/
//     agent.json) are the canonical shared template; caller must be the
//     agent owner or hold platform admin (super_admin session or
//     type=admin apikey).
//   - Per-user files (USER.md, MEMORY.md) just need read access to the
//     agent.
//
// Writes 4xx and returns false on permission/lookup failures. Pre-flatten
// this also resolved *which* user_id row a write targeted (owner for
// identity files, caller for per-user overrides); with agent_files
// flattened to one row per (agent, filename) the target user is gone —
// only the access check remains.
func (s *Server) checkSystemFileWritable(w http.ResponseWriter, r *http.Request, agentID, name string) bool {
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return false
	}
	if agentIdentityFiles[name] {
		// Single-user: every authenticated caller owns every agent.
		return true
	}
	if !s.requireAgentReadable(w, r, agentID) {
		return false
	}
	return true
}

// Workspace files — list / get / upload of agent-produced artifacts.
// Backed by the workspace.Store blob backend, whose layout is
//
//   workspaces/<agent_id>/<session_id>/<path>
//
// The HTTP file endpoints below operate at the agent-root level
// (sessionID="") — that's where uploads land and where ListByAgent
// returns objects across every session of that agent. The agent runtime
// passes its own sessionID for in-chat tool calls; those land under the
// session sub-prefix automatically.

// workspaceSessionScope translates the URL `?sessionId=` token into
// the directory name used under workspaces/<agent>/sessions/. The URL
// token is the session_key (so the dashboard can address any session
// uniformly), but workspace artifacts are namespaced by chat_id
// instead — that's what the agent runtime passed at write time.
//
// Returns the chat_id when the session_key resolves under the caller's
// (user_id, agent_id). Returns "" when the lookup fails — including
// the case where the session belongs to a DIFFERENT user — so callers
// don't accidentally widen scope into another user's files. Pre-fix
// behavior was to fall back to the raw URL token; on a public agent
// that let a non-owner caller pass a known chat_id of the owner and
// read its files because the resulting scope was sessions/<their chat>/.
func (s *Server) workspaceSessionScope(ctx context.Context, agentID, urlToken string) string {
	tok := strings.TrimSpace(urlToken)
	if tok == "" || s.dataStore == nil {
		return ""
	}
	uid := config.UserIDFromContext(ctx)
	if uid == "" {
		return ""
	}
	// Ownership check only — workspace is namespaced by session_key now,
	// so the token IS the scope segment; we don't remap to chat_id.
	if _, _, _, err := s.dataStore.LookupSessionTriple(ctx, agentID, tok); err != nil {
		return ""
	}
	return tok
}

// remapSessionPath is now a no-op (kept so the path-shape helper stays
// co-located with the on-disk layout). Workspace writes and the chat
// client both namespace by session_key now (per-session file isolation:
// a /new starts a fresh file set instead of inheriting the prior
// session's files), so seg already matches the stored path and
// workspaceSessionScope returns it unchanged — the function reduces to
// "return rel" in every case. The old remap existed because the runtime
// stored by chat_id while the client addressed files by session_key.
//
// No-op when <X> isn't a session_key this caller owns (already a
// chat_id, foreign session, anonymous caller, or a non-session path);
// the path-escape / ownership checks below still run on the result.
func (s *Server) remapSessionPath(ctx context.Context, agentID, rel string) string {
	const prefix = "sessions/"
	if !strings.HasPrefix(rel, prefix) {
		return rel
	}
	rest := rel[len(prefix):]
	i := strings.IndexByte(rest, '/')
	if i <= 0 {
		return rel
	}
	seg := rest[:i]
	chatID := s.workspaceSessionScope(ctx, agentID, seg)
	if chatID == "" || chatID == seg {
		return rel
	}
	return prefix + chatID + rest[i:]
}

func (s *Server) handleAgentFileList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.workspaceStore == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"files": []any{}})
		return
	}
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	// Always List with project + session both empty so returned paths
	// stay agent-relative (e.g. "sessions/<sid>/foo.png" or
	// "projects/<pid>/notes.md") — the download endpoint expects that
	// shape, and filtering here is cheaper than two divergent code paths.
	objects, err := s.workspaceStore.List(r.Context(), id, "", "")
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	scope := s.fileScopeForRequest(r, id)
	files := make([]map[string]any, 0, len(objects))
	for _, o := range objects {
		if !scope.acceptPath(o.Path) {
			continue
		}
		files = append(files, map[string]any{
			"path":    o.Path,
			"size":    o.Size,
			"modTime": o.ModTime.UnixMilli(),
		})
	}
	// scopePrefixes tells the client which directory prefixes to peel
	// off before rendering the tree — it can't derive the project a
	// session belongs to from the chat URL alone, so the scope (which
	// just resolved it) hands it over. nil (agent-wide) serializes as
	// null; the client treats that as "strip nothing".
	jsonResponse(w, http.StatusOK, map[string]any{
		"files":         files,
		"scopePrefixes": scope.stripPrefixes,
	})
}

// fileScope describes which agent-relative paths to surface for the
// file browser / zip filter. acceptPath returns true for paths the
// scope considers in-bounds:
//
//	loose chat:  paths under sessions/<chat_id>/
//	project chat: paths under projects/<pid>/<chat_id>/ (the chat's
//	              own files), PLUS entries directly at projects/<pid>/
//	              that aren't another chat's subtree — root-level
//	              "shared/legacy" files AND shared subdirectories
//	              (projects/<pid>/math-course/…) the agent or operator
//	              dropped at the project root. Other chats' subtrees
//	              (projects/<pid>/<other-sid>/...) are excluded —
//	              those belong to that chat's panel.
//	no session:  everything (admin browser).
//
// archiveSuffix returns the human-readable scope id used in the zip
// filename — chat_id for loose chats, "<pid>-<chat_id>" for project
// chats so a download names "agent-pid-sid.zip" instead of
// disambiguating by chat_id alone.
//
// stripPrefixes (most specific first) are the scope directory prefixes
// the frontend peels off before rendering the file tree, so a project
// chat shows its own files bare and shared ones relative to the
// project root instead of as nested projects/<pid>/… folders. Empty
// for the agent-wide browser (paths are already rootless there).
type fileScope struct {
	acceptPath    func(string) bool
	archiveSuffix string
	stripPrefixes []string
}

// stripScopePrefix removes the deepest known scope prefix from an
// agent-relative path so zip entries read as plain filenames. Order
// matters: project chats are tried before session chats so a
// `projects/<pid>/<sid>/foo.md` collapses to `foo.md` rather than
// `<pid>/<sid>/foo.md`. Top-level project files keep the leading
// `projects/<pid>/` strip so they read as bare filenames too.
func stripScopePrefix(p string) string {
	for _, top := range []string{"projects/", "sessions/"} {
		if !strings.HasPrefix(p, top) {
			continue
		}
		rest := p[len(top):]
		// Cut after the scope id (one path segment).
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			rest = rest[i+1:]
			// Project paths can have a second id segment for the
			// per-chat subdir; collapse that too when present.
			if top == "projects/" {
				if j := strings.IndexByte(rest, '/'); j >= 0 {
					// Only treat the first segment as a chat id when it
					// looks like one (s-... prefix). Otherwise keep
					// rest as-is so legacy "subdir/file.md" structures
					// don't get over-stripped.
					if first := rest[:j]; strings.HasPrefix(first, "s-") {
						rest = rest[j+1:]
					}
				}
			}
			return rest
		}
		return ""
	}
	return p
}

// rejectAllScope returns a fileScope that lets nothing through. Used
// when the caller asked for a sessionId we can't resolve for them, so
// a non-owner can't widen into another user's files on a public agent
// just by guessing/leaking a chat_id.
func rejectAllScope() fileScope {
	return fileScope{acceptPath: func(string) bool { return false }}
}

func (s *Server) fileScopeForRequest(r *http.Request, agentID string) fileScope {
	rawSession := r.URL.Query().Get("sessionId")
	rawProject := r.URL.Query().Get("projectId")
	// Project landing page: no specific chat is open, so the panel
	// shows everything under projects/<pid>/ — every chat's subtree
	// plus root-level shared files. The sessionId branch below is
	// the per-chat view; use this branch when the URL is
	// /agents/<aid>/project/<pid> with no chat selected.
	if rawSession == "" && rawProject != "" {
		prefix := "projects/" + rawProject + "/"
		return fileScope{
			acceptPath:    func(p string) bool { return strings.HasPrefix(p, prefix) },
			archiveSuffix: rawProject,
			stripPrefixes: []string{prefix},
		}
	}
	if rawSession == "" {
		// Agent-wide view (no scope params at all). Owner / super_admin
		// can legitimately browse every file; non-owners (public-agent
		// viewers, foreign apikey callers) must specify a session they
		// own, otherwise we'd hand them other users' files.
		if s.callerOwnsAgent(r, agentID) {
			return fileScope{acceptPath: func(string) bool { return true }}
		}
		return rejectAllScope()
	}
	scopeKey := s.workspaceSessionScope(r.Context(), agentID, rawSession)
	if scopeKey == "" {
		// sessionId didn't resolve to a session THIS caller owns — either
		// it doesn't exist or it belongs to another user. Either way,
		// surface nothing. Pre-fix behavior was to widen back to
		// "accept all", which on a public agent meant non-owners could
		// list every chat's files by passing a junk sessionId.
		return rejectAllScope()
	}
	if pid := s.resolveSessionProject(r.Context(), r, agentID, rawSession); pid != "" {
		ownPrefix := "projects/" + pid + "/" + scopeKey + "/"
		rootPrefix := "projects/" + pid + "/"
		return fileScope{
			acceptPath: func(p string) bool {
				if strings.HasPrefix(p, ownPrefix) {
					return true
				}
				// Entry directly under the project root. Other chats'
				// subtrees (first segment a session key "s-…") stay out —
				// those belong to that chat's panel — but shared
				// subdirectories (projects/<pid>/math-course/…) belong to
				// the whole project and surface here too, not just legacy
				// root-level files.
				if strings.HasPrefix(p, rootPrefix) {
					rest := p[len(rootPrefix):]
					if rest == "" {
						return false
					}
					first := rest
					if i := strings.IndexByte(rest, '/'); i >= 0 {
						first = rest[:i]
					}
					return !strings.HasPrefix(first, "s-")
				}
				return false
			},
			archiveSuffix: pid + "-" + scopeKey,
			// Own subdir first so its files render bare; shared entries
			// fall through to the project-root strip and keep their
			// subdirectory structure (math-course/…).
			stripPrefixes: []string{ownPrefix, rootPrefix},
		}
	}
	prefix := "sessions/" + scopeKey + "/"
	return fileScope{
		acceptPath:    func(p string) bool { return strings.HasPrefix(p, prefix) },
		archiveSuffix: scopeKey,
		stripPrefixes: []string{prefix},
	}
}

// handleAgentFilesZip streams a zip of every workspace file for the agent
// (or just one session when ?sessionId= is set). Files are added with
// their session-relative path so the archive layout matches what the user
// sees in the chat panel — no enclosing wrapper directory.
func (s *Server) handleAgentFilesZip(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.workspaceStore == nil {
		http.Error(w, "no workspace store", http.StatusServiceUnavailable)
		return
	}
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	objects, err := s.workspaceStore.List(r.Context(), id, "", "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	scope := s.fileScopeForRequest(r, id)
	archiveName := fmt.Sprintf("%s.zip", id)
	if scope.archiveSuffix != "" {
		archiveName = fmt.Sprintf("%s-%s.zip", id, scope.archiveSuffix)
	}
	// Wrap entries in a folder named after the archive so extractors
	// (macOS Archive Utility, Windows Explorer, 7zip…) place every
	// file inside one directory instead of dumping them loose next
	// to the zip. Without this, "5 files extracted" looks like
	// "files went missing" because they fan out into Downloads/.
	wrapper := strings.TrimSuffix(archiveName, ".zip") + "/"

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archiveName))

	zw := zip.NewWriter(w)
	written, skipped, failed := 0, 0, 0
	for _, o := range objects {
		if !scope.acceptPath(o.Path) {
			skipped++
			continue
		}
		// Strip the deepest scope prefix from the archive entry name
		// so the user sees clean filenames in the zip rather than
		// nested `projects/<pid>/<sid>/foo.md` paths.
		entryName := stripScopePrefix(o.Path)
		if entryName == "" {
			skipped++
			continue
		}
		hdr := &zip.FileHeader{
			Name:     wrapper + entryName,
			Method:   zip.Deflate,
			Modified: o.ModTime,
		}
		entry, err := zw.CreateHeader(hdr)
		if err != nil {
			// Continue, not return — finalizing the archive with the
			// rest of the entries is more useful than bailing out and
			// leaving the user with a single file. Pre-fix behavior:
			// any transient hiccup partway through truncated the zip
			// to whatever was already written, surfacing in prod as
			// "only one image came out".
			slog.Warn("zip: create entry failed", "agent", id, "path", o.Path, "err", err)
			failed++
			continue
		}
		rc, err := s.workspaceStore.Get(r.Context(), id, "", "", o.Path)
		if err != nil {
			slog.Warn("zip: open object failed", "agent", id, "path", o.Path, "err", err)
			failed++
			continue
		}
		_, copyErr := io.Copy(entry, rc)
		rc.Close()
		if copyErr != nil {
			slog.Warn("zip: copy failed", "agent", id, "path", o.Path, "err", copyErr)
			failed++
			continue
		}
		written++
	}
	if err := zw.Close(); err != nil {
		slog.Warn("zip: writer close failed", "agent", id, "err", err)
	}
	slog.Info("zip: archive sent", "agent", id, "archive", archiveName,
		"objects", len(objects), "written", written, "skipped", skipped, "failed", failed)
}

// handleAgentWorkspaceReveal opens the chatter's workspace folder in
// the operator's native file browser (Finder/Explorer/xdg-open).
// Self-hosted only — hosted deployments don't have a meaningful
// concept of "the operator's local filesystem" and the chatter
// doesn't own the daemon, so exposing this would be a privilege
// leak. Reads sessionId / projectId from the query string, mirrors
// fileScopeForRequest's resolution (session_key → chat_id, project
// lookup) so the revealed dir matches what the chat-side Workspace
// panel is showing.
//
// Best-effort: returns 200 with the resolved path on success, 4xx
// on bad scope, 503 when the configured workspace store doesn't
// expose a host path (S3 / R2 deploys), 500 if the OS open command
// fails. Non-blocking — we don't wait for Finder to actually
// surface the window.
func (s *Server) handleAgentWorkspaceReveal(w http.ResponseWriter, r *http.Request) {
	if buildinfo.IsHostedDeploy() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "workspace reveal is disabled on hosted deployments"})
		return
	}
	id := r.PathValue("id")
	if s.workspaceStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "no workspace store configured"})
		return
	}
	if !s.requireAgentReadable(w, r, id) {
		return
	}

	scoper, ok := s.workspaceStore.(workspace.LocalScoper)
	if !ok {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "workspace store has no local path (e.g. S3-backed) — open in Finder is unavailable"})
		return
	}

	rawSession := r.URL.Query().Get("sessionId")
	rawProject := r.URL.Query().Get("projectId")

	// Resolve to the same (project, chatID) the chat-side panel is
	// scoped to. Empty rawSession + non-empty projectId means project
	// landing — reveal the project root. Empty both means agent root
	// (admin browser); we still allow it because requireAgentReadable
	// has already gated access.
	chatID := ""
	projectID := rawProject
	if rawSession != "" {
		chatID = s.workspaceSessionScope(r.Context(), id, rawSession)
		if pid := s.resolveSessionProject(r.Context(), r, id, rawSession); pid != "" {
			projectID = pid
		}
	}

	dir, ok := scoper.LocalScopeDir(id, projectID, chatID)
	if !ok || dir == "" {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "workspace store did not return a host path"})
		return
	}

	// Pre-create the dir so `open <missing-path>` doesn't error out
	// on a brand-new chat that hasn't written any files yet — empty
	// folder still feels like progress to the user.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	if err := openInFileBrowser(dir); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "path": dir})
}

// openInFileBrowser shells out to the platform-appropriate "open"
// command. macOS and Linux behave consistently (open the directory
// in the default file manager); Windows uses explorer.exe. We
// deliberately don't wait on the child — Finder in particular
// returns immediately, and there's no useful exit code to surface
// either way.
func openInFileBrowser(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		// `explorer` returns exit code 1 even on success, so we
		// don't check err. The only real failure mode is "binary
		// not on PATH", which Start() reports.
		cmd = exec.Command("explorer", path)
		return cmd.Start()
	default:
		// Linux / *BSD — xdg-open is the freedesktop standard.
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	// Detach: we don't care about the file manager's lifetime.
	go func() { _ = cmd.Wait() }()
	return nil
}

func (s *Server) handleAgentFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rel := r.PathValue("path")
	if rel == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "path required"})
		return
	}
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	// IM sessions carry the URL session_key (s-...) but store workspace
	// files under sessions/<chat_id>/ (e.g. <openid>@im.wechat). The chat
	// client renders /workspace/<name> links with the session_key, so
	// remap the path's leading session segment to the storage chat_id —
	// same session_key→chat_id translation fileScopeForRequest does for
	// list/zip. No-op for webchat (session_key == chat_id) and for
	// foreign or anonymous callers (translation returns "").
	rel = s.remapSessionPath(r.Context(), id, rel)
	// Never serve a file named SKILL.md as a downloadable artifact. Skill
	// manifests are the agent's IP and never legitimately land in the
	// workspace (skill-creator writes them to the skills bucket, not here),
	// so a SKILL.md showing up under /workspace is the tail of the
	// `cat /skills/foo/SKILL.md > /workspace/foo.md` exfil chain. This is a
	// name-level guard only — it does not catch manifest content saved
	// under a different filename; that residual is the model-cooperation
	// case the load_skill / system-prompt confidentiality directives cover.
	if strings.EqualFold(filepath.Base(filepath.Clean(rel)), "SKILL.md") {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "refused: skill manifests are not downloadable"})
		return
	}
	if s.workspaceStore != nil {
		s.serveFileFromWorkspaceStore(w, r, id, rel)
		return
	}
	// Workspace store not configured — fall back to direct FS read.
	// The local FS layout mirrors the workspace store's:
	// ~/.fluctio/workspaces/<agent_id>/<path>.
	home, err := config.HomeDir()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	root := filepath.Join(home, "workspaces", id)
	abs := filepath.Join(root, filepath.Clean("/"+rel))
	if !strings.HasPrefix(abs, root+string(os.PathSeparator)) && abs != root {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "path escape"})
		return
	}
	// ServeFile sets Content-Type from the mime database itself; we just
	// add the CSP sandbox for HTML on top — same rationale as in
	// setFileResponseHeaders above.
	if ext := strings.ToLower(filepath.Ext(rel)); ext == ".html" || ext == ".htm" {
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, abs)
}

func (s *Server) serveFileFromWorkspaceStore(w http.ResponseWriter, r *http.Request, agentID, path string) {
	rc, err := s.workspaceStore.Get(r.Context(), agentID, "", "", path)
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	defer rc.Close()
	setFileResponseHeaders(w, path)
	io.Copy(w, rc)
}

// setFileResponseHeaders picks the right Content-Type for a user-produced
// workspace file and locks down agent-generated HTML so it can't reach the
// app's cookies/storage even if the user opens the URL in a bare tab. The
// Content-Type derived from the extension is what lets iframes render the
// file (octet-stream → about:blank, since iframes don't sniff). The CSP
// `sandbox` header is the same protection the chat preview gets via the
// iframe `sandbox` attribute, but applied at the HTTP layer so it kicks in
// no matter how the file is loaded.
func setFileResponseHeaders(w http.ResponseWriter, path string) {
	ext := strings.ToLower(filepath.Ext(path))
	ctype := mime.TypeByExtension(ext)
	if ctype == "" {
		ctype = imageMimeByExt(ext) // .jfif/.avif/.heic/.tiff… not in the system mime db
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if ext == ".html" || ext == ".htm" {
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts")
	}
}

// imageMimeByExt covers image extensions the system mime database often
// doesn't register (.jfif, .avif, .heic, .tiff, …). Without it the workspace
// store serving those files returns application/octet-stream and the browser
// refuses to render them as <img>. Returns "" for non-image / unknown.
func imageMimeByExt(ext string) string {
	switch ext {
	case ".jpg", ".jpeg", ".jfif":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".svg":
		return "image/svg+xml"
	case ".avif":
		return "image/avif"
	case ".apng":
		return "image/apng"
	case ".heic", ".heif":
		return "image/heic"
	case ".tiff", ".tif":
		return "image/tiff"
	case ".ico":
		return "image/x-icon"
	}
	return ""
}

func (s *Server) handleAgentFileUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	if s.workspaceStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "no workspace store"})
		return
	}
	if rec := s.requireAgentOwner(w, r, id); rec == nil {
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// The chat client sends one form field "file" per attachment, so the
	// multipart payload often carries several entries under the same key.
	// r.FormFile only returns the first — iterate over MultipartForm.File
	// so multi-attach uploads land all of their files, not just one.
	headers := r.MultipartForm.File["file"]
	if len(headers) == 0 {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "no file"})
		return
	}
	// sessionId scopes the upload to the sandbox mount the agent actually
	// sees: projects/<pid>/ for project chats (alongside the agent's own
	// writes), sessions/<sid>/ for loose chats. The client's explicit
	// projectId hint wins (a project chat's very first send fires before
	// the session row exists, so no lookup could resolve it); afterwards
	// the server resolves the session's project. Web's session_key equals
	// the urlToken (see resolveOrMintKey), so both paths land in the dir
	// the sandbox mounts.
	projectID := r.URL.Query().Get("projectId")
	sessionID := ""
	if projectID == "" {
		// Loose chat: the mount target is sessions/<sessionKey>/, and for
		// web the sessionKey IS the urlToken the client just sent.
		sessionID = r.URL.Query().Get("sessionId")
		// A project chat's sandbox mounts projects/<pid>/, not
		// sessions/<sid>/. The chat URL carries no projectId (the client
		// only has it on a project page's first send), so attachments
		// uploaded from inside an existing project chat used to land in
		// the loose-chat dir — the upload "succeeded" but the agent's
		// /workspace couldn't see the file and the chat file panel
		// filtered it out. Resolve the session's project server-side and
		// redirect the upload there; falls through to the loose scope
		// when the session has no project or isn't minted yet.
		if sessionID != "" {
			if pid := s.resolveSessionProject(r.Context(), r, id, sessionID); pid != "" {
				projectID = pid
				sessionID = ""
			}
		}
	}
	saved := make([]map[string]any, 0, len(headers))
	for _, h := range headers {
		fh, err := h.Open()
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		data, err := io.ReadAll(fh)
		fh.Close()
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := s.workspaceStore.Put(r.Context(), id, projectID, sessionID, h.Filename, strings.NewReader(string(data)), int64(len(data)), ""); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		saved = append(saved, map[string]any{"name": h.Filename, "size": len(data)})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "files": saved})
}

func defaultIfEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// invalidateUser drops the user's lazy-loaded UserSpace so the next
// access reloads it from the DB. The gateway implements InvalidateUser
// behind the api.UserResolver interface.
func (s *Server) invalidateUser(userID string) {
	if userID == "" || s.userResolver == nil {
		return
	}
	if r, ok := s.userResolver.(interface{ InvalidateUser(string) }); ok {
		r.InvalidateUser(userID)
	}
	slog.Debug("invalidated user space", "user", userID)
}

// invalidateAgent drops every cached UserSpace that holds this agent —
// owner plus any foreign caller that lazy-attached via EnsureAgent
// (super_admin chat, public-link viewer, apikey user). Use this after
// writes that mutate the agent's resolved runtime (agents.defaults,
// agent-scope providers); plain user-scope writes can stick with
// invalidateUser.
func (s *Server) invalidateAgent(agentID string) {
	if agentID == "" || s.userResolver == nil {
		return
	}
	if r, ok := s.userResolver.(interface{ InvalidateAgent(string) }); ok {
		r.InvalidateAgent(agentID)
	}
	slog.Debug("invalidated user spaces holding agent", "agent", agentID)
}

// requireOwnerOrSuperAdmin guards endpoints that mutate another user's
// resources.
func (s *Server) requireOwnerOrSuperAdmin(w http.ResponseWriter, r *http.Request, ownerID string) bool {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return false
	}
	if ident.UserID == ownerID || ident.Role == users.RoleSuperAdmin {
		return true
	}
	jsonResponse(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
	return false
}

var _ workspace.Store = (workspace.Store)(nil)

// handleListAgentRegisteredTools returns the live tool registry for the
// specified agent. Drives the Tools tab's allowlist checkbox picker —
// the operator clicks rather than typing tool names from memory.
//
// Permission is read-level (owner / super_admin / shared-link viewer)
// rather than owner-only because viewers might want to see what they
// have access to, even if they can't change the allowlist. The PUT
// path stays owner-gated.
func (s *Server) handleListAgentRegisteredTools(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	ag := s.resolveAgent(r, id)
	if ag == nil {
		// Agent isn't loaded in the caller's UserSpace and lazy-attach
		// also failed. We could fall back to the DB record, but the
		// whole point of this endpoint is the LIVE registry (MCP tools
		// only exist once the agent is attached), so a 404 here is
		// honest rather than misleadingly returning just the builtins.
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not loaded"})
		return
	}
	toolList := ag.RegisteredTools()
	if toolList == nil {
		toolList = []tools.ToolInfo{}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"tools": toolList})
}
