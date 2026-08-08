export interface StatusResponse {
  configured: boolean;
  registrationOpen?: boolean;
  running: boolean;
  port: number;
  mode?: string;
  version?: string;
  uptime: string;
  agents: AgentInfo[];
  channels: ChannelInfo[];
  provider: ProviderInfo;
  cronJobs?: number;
  plugins?: number;
  userId?: string;
  isAdmin?: boolean;
  users?: number;
  chatters?: number;
}

export interface AgentInfo {
  id: string;
  name?: string;
  model: string;
  workspace: string;
}

export interface ChannelInfo {
  type: string;
  botUsername: string;
  enabled?: boolean;
  status?: string;
}

export interface ProviderInfo {
  name: string;
  model: string;
  apiBase: string;
  apiKey: string;
}

export interface AgentDetail {
  id: string;
  name?: string;
  description?: string;
  avatarUrl?: string;       // /api/agents/{id}/files/avatar.png — may 404
  userId?: string;          // owner's user id (agents.user_id)
  // role distinguishes agents the caller owns from agents accessed via
  // a public link. "viewer" gates UI out of configuration tabs
  // (Customize / Skills / Channels / Scheduler / Models). Backend
  // always sends one of these on /api/agents and /api/agents/{id}.
  role?: "owner" | "viewer";
  model: string;
  workspace?: string;
  maxTokens?: number;
  temperature?: number;
  maxToolIterations?: number;
  thinking?: string;
  // promptMode is what the backend currently has saved on the
  // agents.defaults row. Empty / undefined = no override (runtime
  // falls back to "agent"). See AgentUpdatePayload.promptMode for
  // the allowed values. The built-in tool set the LLM sees is a
  // function of this mode — there's no separate allowlist field by
  // design. Extend tools via Plugin or MCP, not per-agent toggles.
  promptMode?: string;
  // guidance: per-agent operational-constraint strength. "" / undefined
  // = no override (runtime defaults to "guided"); "guided" = firm rules
  // (sub-flagship models); "autonomous" = soft, judgement-led phrasing
  // (top-tier models).
  guidance?: string;
  // splitReplies is the per-agent multi-bubble override. Applies to
  // every IM channel uniformly — when on, the agent may emit the
  // SplitMessageMarker between bubbles and the dispatcher honors it.
  // null / undefined / false-ish = single bubble per reply (default).
  splitReplies?: boolean | null;
  // autoPersist is the per-agent "remember the chatter automatically"
  // toggle. When on, every N turns the runtime fires an LLM-driven
  // distill pass that appends extracted facts to USER.md (chatter
  // profile) and MEMORY.md (long-term notes). Mainly needed in chatbot
  // mode — that mode's curated tool allowlist excludes write_file, so
  // this is the only path the agent has to remember a chatter across
  // sessions.
  autoPersist?: boolean | null;
  // sharedIdentity — when true, all channels share sessions and memory
  // with the web channel (owner's identity). Default false.
  sharedIdentity?: boolean;
  // plugins is the per-agent hook-plugin enable overlay: pluginID →
  // enabled. Missing keys fall back to the system-wide enable state
  // (visible via /api/plugins). null/undefined means "no per-agent
  // override at all".
  plugins?: Record<string, boolean> | null;
  soul?: string;
  skills?: string[];
  tools?: string[];
  // config mirrors the backend agents row's Config blob (rec.Config) —
  // per-agent AgentFileConfig fields (kb, mcpServers, language, ...) as
  // a raw map. The Context page reads config.language for the reply-
  // language override.
  config?: Record<string, unknown>;
}

export interface SkillEnvSpec {
  name: string;
  description?: string;
  required?: boolean;
  secret?: boolean;
}

export interface SkillInfo {
  name: string;
  description: string;
  location: string;
  type: string;
  envSpec?: SkillEnvSpec[];
}

export interface SkillEntryCfg {
  enabled?: boolean;
  apiKey?: string;
  env?: Record<string, string>;
}

// updateSkillEntries persists skill env / apiKey patches. When agentId
// is set the patch lands in cfg.Skills.AgentEntries[agentId] (per-agent
// override), otherwise in cfg.Skills.Entries (global default). The
// runtime resolves agent-scoped first, falling back to global.
export async function updateSkillEntries(
  entries: Record<string, SkillEntryCfg>,
  agentId?: string,
) {
  const body = agentId
    ? { skills: { agentEntries: { [agentId]: entries } } }
    : { skills: { entries } };
  const res = await apiFetch("/api/config", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  return res.json();
}

export interface PluginInfo {
  id: string;
  type: string;
  version: string;
  status: string;
  enabled: boolean;
  config?: Record<string, unknown>;
}

export interface CronJobInfo {
  id: string;
  name: string;
  type: string;
  schedule: string;
  agentId: string;
  channel: string;
  chatId: string;
  message: string;
  enabled: boolean;
  lastRun?: string;
  nextRun?: string;
}

export interface ModelCost {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
}

export interface ModelEntry {
  id: string;
  name: string;
  reasoning: boolean;
  input: string[];
  cost: ModelCost;
  contextWindow: number;
  maxTokens: number;
}

export interface ProviderData {
  apiKey: string;
  apiBase: string;
  apiType?: string;
  authType?: string;
  models?: ModelEntry[];
}

export interface ConfigResponse {
  providers: Record<string, ProviderData>;
  agents: {
    defaults: {
      model: string;
      maxTokens: number;
      temperature: number;
      maxToolIterations: number;
    };
  };
  channels: Record<string, { enabled: boolean; botToken?: string }>;
  storage: { type: string; dsn?: string };
  sandbox?: {
    enabled: boolean;
    backend?: string;
    // Legacy single-slot image field; read-only fallback. The per-
    // backend fields below are authoritative when set so switching
    // backends in the UI preserves each backend's last-entered value.
    image?: string;
    dockerImage?: string;
    e2bTemplate?: string;
    boxliteSnapshot?: string;
    e2bKey?: string;
    boxliteUrl?: string;
    boxliteClientId?: string;
    boxliteKey?: string;
    boxlitePrefix?: string;
  };
  prefs?: {
    timezone?: string;
  };
  wechat?: {
    splitReplies?: boolean;
  };
  hooks: { enabled: boolean; token?: string; path?: string; port?: number };
  cronJobs?: Array<Record<string, unknown>>;
  skills?: {
    entries?: Record<string, SkillEntryCfg>;
    // Per-agent overrides, keyed agentID → skillName → entry. The UI
    // surfaces these only on the agent-scoped /agents/<id>/skills page;
    // SkillsLoader.SkillEnvVars resolves agentEntries[<agent>][<skill>]
    // first, falling back to the global entries map.
    agentEntries?: Record<string, Record<string, SkillEntryCfg>>;
  };
  // Presentation hints the dashboard needs to render inheritance state
  // without re-resolving the scope chain client-side. systemDefaultModel
  // is the value `agents.defaults.model` would resolve to from system
  // scope alone — compare against `agents.defaults.model` (the merged
  // value) to know whether the caller has overridden at user scope.
  meta?: {
    systemDefaultModel?: string;
    serverTimezone?: string;
  };
}

// Auth token for cloud mode. Set via setAuthToken() on login; empty in local mode.
let authToken = "";

export function setAuthToken(token: string) {
  authToken = token;
  if (token) {
    localStorage.setItem("fluctio_token", token);
  } else {
    localStorage.removeItem("fluctio_token");
  }
}

export function getAuthToken(): string {
  if (!authToken) {
    authToken = localStorage.getItem("fluctio_token") || "";
  }
  return authToken;
}

// Wrapper around fetch that injects Authorization header when a token is set
// and always includes the cookie session for username/password logins. Cookie
// is the primary credential for the web UI; the bearer is only used by
// programmatic clients that put the token into localStorage manually.
//
// When the page URL carries `?actAs=<userId>`, the same param is mirrored
// into every API request so super_admin opening another user's resources
// (e.g. /agents/<id>/chat/<sid>/?actAs=<uid> reached from the admin Chats
// page) actually reads/writes against that user's scope. The middleware-
// level actAs lock makes these requests read-only.
export async function apiFetch(url: string, init?: RequestInit): Promise<Response> {
  const token = getAuthToken();
  const headers: Record<string, string> = {
    ...(init?.headers as Record<string, string> || {}),
  };
  if (token) {
    headers["Authorization"] = `Bearer ${token}`;
  }
  if (typeof window !== "undefined") {
    const pageActAs = new URLSearchParams(window.location.search).get("actAs");
    if (pageActAs && !/[?&]actAs=/.test(url)) {
      url += (url.includes("?") ? "&" : "?") + "actAs=" + encodeURIComponent(pageActAs);
    }
  }
  return fetch(url, { credentials: "same-origin", ...init, headers });
}

// In-flight dedup for shared GETs. When the sidebar's status poll, a page's
// mount effect, and a child component's effect all call getStatus() (or
// getMe/getConfig/getTools) within the same tick, they share one network
// request instead of firing N identical ones. The entry is cleared on
// settle, so the next call — e.g. the sidebar's next 15s poll tick — fetches
// fresh data. No staleness, just fewer redundant round-trips over a slow link.
const inflightGets = new Map<string, Promise<unknown>>();
function dedupGet<T>(key: string, run: () => Promise<T>): Promise<T> {
  const existing = inflightGets.get(key) as Promise<T> | undefined;
  if (existing) return existing;
  const p = run().finally(() => {
    if (inflightGets.get(key) === p) inflightGets.delete(key);
  });
  inflightGets.set(key, p);
  return p;
}

// Login + logout + me

export interface MeResponse {
  ok: boolean;
  user?: {
    id: string;
    username: string;
    email: string;
    role: string;
    displayName?: string;
    avatarUrl?: string;
    status: string;
  };
  authMethod?: string;
  readOnly?: boolean;
  // 'self-hosted' (default) or 'hosted' — driven by FLUCTIO_DEPLOY
  // env var on the daemon. Frontend uses this to gate local-only
  // conveniences (open-in-Finder, future $EDITOR hooks).
  deployMode?: "self-hosted" | "hosted";
  error?: string;
}

export async function login(loginField: string, password: string): Promise<MeResponse> {
  const res = await fetch("/api/login", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ login: loginField, password }),
  });
  return res.json();
}

export async function logout(): Promise<void> {
  await apiFetch("/api/logout", { method: "POST" });
  setAuthToken("");
}

export async function getMe(): Promise<MeResponse> {
  return dedupGet("/api/me", async () => {
    const res = await apiFetch("/api/me");
    return res.json();
  });
}

export async function updateMe(req: { displayName: string; avatarUrl: string }) {
  const res = await apiFetch("/api/me", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function changeMyPassword(req: { oldPassword: string; newPassword: string }) {
  const res = await apiFetch("/api/me/password", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

// Onboard

export interface OnboardRequest {
  username: string;
  email: string;
  password: string;
  displayName?: string;
  provider?: string;
  apiBase?: string;
  apiKey?: string;
  apiType?: string;
  authType?: string;
  model?: string;
  agentName?: string;
  sandboxEnabled?: boolean;
  sandboxBackend?: string;
  sandboxImage?: string;
  sandboxE2BKey?: string;
  sandboxBoxliteUrl?: string;
  sandboxBoxliteClientId?: string;
  sandboxBoxliteKey?: string;
  sandboxBoxlitePrefix?: string;
}

export async function onboard(req: OnboardRequest): Promise<{ ok: boolean; error?: string }> {
  const res = await fetch("/api/onboard", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

// User management — admin-only at the top level (CRUD), admin-or-self
// for the nested resources (apikeys/agents under /api/users/{id}/...).
// The /api/admin/* prefix was removed in favor of flat resource paths;
// permission is enforced inside each handler.

export async function adminListUsers() {
  const res = await apiFetch("/api/users");
  return res.json();
}

export async function adminListAgents() {
  const res = await apiFetch("/api/agents?all=true");
  return res.json();
}

// Apikeys (per-user)

export async function listApikeys() {
  const res = await apiFetch("/api/apikeys");
  return res.json();
}

export async function createApikey(req: { name: string }) {
  const res = await apiFetch("/api/apikeys", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function deleteApikey(id: string) {
  const res = await apiFetch(`/api/apikeys/${id}`, { method: "DELETE" });
  return res.json();
}

export async function rotateApikey(id: string) {
  const res = await apiFetch(`/api/apikeys/${id}/rotate`, { method: "POST" });
  return res.json();
}

// --- Diagnostic error reports (manual trigger + download) ---

export async function generateDiagReport(opts: {
  days?: number;
  agentId?: string;
}): Promise<{ path: string; name: string }> {
  const res = await apiFetch("/api/diag/reports", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(opts),
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || res.statusText);
  }
  return res.json();
}

export type DiagReportEntry = {
  name: string;
  size: number;
  time: string;
};

export async function listDiagReports(): Promise<DiagReportEntry[]> {
  const res = await apiFetch("/api/diag/reports");
  if (!res.ok) return [];
  const data = await res.json();
  return data.reports ?? [];
}

// diagReportDownloadUrl is opened directly by the browser — the endpoint
// serves the .md file, so no fetch is needed.
export function diagReportDownloadUrl(name: string): string {
  return `/api/diag/reports/${encodeURIComponent(name)}`;
}

export async function deleteDiagReport(name: string): Promise<void> {
  const res = await apiFetch(`/api/diag/reports/${encodeURIComponent(name)}`, {
    method: "DELETE",
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || res.statusText);
  }
}

// Scoped providers + channels

export type ScopeName = "system" | "user" | "agent";

export interface ProviderRow {
  id: string;
  scope: ScopeName;
  scopeId: string;
  name: string;
  apiBase?: string;
  apiKey?: string;       // masked on read
  apiType?: string;
  authType?: string;
  models?: ModelEntry[];
  updatedAt?: string;
}

export interface ChannelRow {
  id: string;
  scope: ScopeName;
  scopeId: string;
  type: string;
  enabled: boolean;
  botToken?: string;     // masked on read
  appToken?: string;
  credentialKey?: string;
  updatedAt?: string;
}

export async function listProviders(scope?: ScopeName, scopeId?: string) {
  const params = new URLSearchParams();
  if (scope) params.set("scope", scope);
  if (scopeId) params.set("scopeId", scopeId);
  const qs = params.toString();
  const url = "/api/providers" + (qs ? `?${qs}` : "");
  const res = await apiFetch(url);
  return res.json();
}

export async function createProvider(req: {
  scope: ScopeName;
  scopeId: string;
  name: string;
  apiBase?: string;
  apiKey?: string;
  apiType?: string;
  authType?: string;
  models?: ModelEntry[];
}) {
  const res = await apiFetch("/api/providers", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function updateProvider(id: string, req: Partial<ProviderRow>) {
  const res = await apiFetch(`/api/providers/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function deleteProvider(id: string) {
  const res = await apiFetch(`/api/providers/${id}`, { method: "DELETE" });
  return res.json();
}

// testStoredProvider hits the saved provider row server-side using its
// own apiKey, so the Edit dialog can verify a model id without forcing
// the user to re-paste the secret. The backend never returns unmasked
// keys to the browser, so this is the only way to test from edit mode.
//
// Non-secret overrides (apiBase / apiType / authType) are passed through
// when the user has edited them in the form — the saved row's values are
// only used as fallback. Without this, editing just the URL and clicking
// Test would silently re-ping the old saved URL and report green.
export async function testStoredProvider(
  providerId: string,
  model: string,
  overrides?: { apiBase?: string; apiType?: string; authType?: string },
): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch(`/api/providers/${providerId}/test`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ model, ...(overrides ?? {}) }),
  });
  return res.json();
}

export async function listScopedChannels(scope?: ScopeName, scopeId?: string) {
  const params = new URLSearchParams();
  if (scope) params.set("scope", scope);
  if (scopeId) params.set("scopeId", scopeId);
  const qs = params.toString();
  const url = "/api/scoped-channels" + (qs ? `?${qs}` : "");
  const res = await apiFetch(url);
  return res.json();
}

export async function createScopedChannel(req: {
  scope: ScopeName;
  scopeId: string;
  type: string;
  enabled: boolean;
  botToken?: string;
  appToken?: string;
  credentialKey?: string;
}) {
  const res = await apiFetch("/api/scoped-channels", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function updateScopedChannel(id: string, req: Partial<ChannelRow>) {
  const res = await apiFetch(`/api/scoped-channels/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function deleteScopedChannel(id: string) {
  const res = await apiFetch(`/api/scoped-channels/${id}`, { method: "DELETE" });
  return res.json();
}

// Status
export async function getStatus(): Promise<StatusResponse> {
  return dedupGet("/api/status", async () => {
    const res = await apiFetch("/api/status");
    return res.json();
  });
}

// Provider
export async function testProvider(config: { apiBase: string; apiKey: string; model: string; apiType?: string; authType?: string }) {
  const res = await apiFetch("/api/test-provider", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(config),
  });
  return res.json();
}

// Builtin model table — merged from the embedded JSON + local override file.
// Returns { modelId: { contextWindow, maxTokens } } for every known model.
// Used by the models page for autocomplete suggestions and autofill.
export interface BuiltinModelMeta {
  contextWindow: number;
  maxTokens: number;
}
export async function getBuiltinModels(): Promise<Record<string, BuiltinModelMeta>> {
  const r = await apiFetch("/api/models/builtin");
  if (!r.ok) throw new Error(`HTTP ${r.status}`);
  return r.json();
}

// Fetch the live model list from the agent's bound upstream provider.
// Each item includes a contextWindow resolved via the builtin table.
// Unsupported / unreachable providers yield HTTP 501.
export async function fetchProviderModels(
  agentId: string
): Promise<{ id: string; contextWindow: number; maxTokens: number }[]> {
  const r = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/models/fetch`,
    { method: "POST" }
  );
  if (!r.ok) throw new Error(`HTTP ${r.status}`);
  return r.json();
}

// Fetch the live model list from a provider config (global /models page).
// When apiKey is empty, pass providerId so the backend resolves the
// stored key server-side (Edit dialog "useStoredKey" mode).
export async function fetchModelsByConfig(body: {
  apiBase: string;
  apiKey?: string;
  apiType: string;
  providerId?: string;
}): Promise<{ id: string; contextWindow: number; maxTokens: number }[]> {
  const r = await apiFetch("/api/models/fetch", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!r.ok) throw new Error(`HTTP ${r.status}`);
  return r.json();
}

// Config — persisted system_settings block (super_admin only).
export async function saveConfig(config: Record<string, unknown>) {
  const res = await apiFetch("/api/config", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(config),
  });
  return res.json();
}

export async function getConfig(): Promise<ConfigResponse> {
  return dedupGet("/api/config", async () => {
    const res = await apiFetch("/api/config");
    return res.json();
  });
}

export async function updateConfig(config: Record<string, unknown>) {
  const res = await apiFetch("/api/config", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(config),
  });
  return res.json();
}

// Workspace files listing — used to diff a turn's outputs so the chat
// UI can surface produced files under the final reply.
export interface WorkspaceFile {
  path: string;
  size: number;
  modTime: number;
}

// revealAgentWorkspace opens the workspace folder for this scope in
// the operator's native file browser (Finder/Explorer/xdg-open).
// Self-hosted only — hosted deployments 403; the UI hides the
// trigger button so callers shouldn't normally hit that path. One
// of sessionId/projectId scopes the reveal: pass sessionId for a
// chat, projectId for the project landing page, neither for the
// agent root (admin browser).
export async function revealAgentWorkspace(
  agentId: string,
  sessionId?: string,
  projectId?: string,
): Promise<{ ok: boolean; path?: string; error?: string }> {
  const params = new URLSearchParams();
  if (sessionId) params.set("sessionId", sessionId);
  if (projectId) params.set("projectId", projectId);
  const qs = params.toString();
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/workspace/reveal${qs ? "?" + qs : ""}`,
    { method: "POST" },
  );
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    return { ok: false, error: (data.error as string) || `HTTP ${res.status}` };
  }
  return { ok: true, path: data.path as string };
}

export async function listAgentFiles(
  agentId: string,
  sessionId?: string,
  projectId?: string,
): Promise<WorkspaceFile[]> {
  // sessionId scopes to a single chat; projectId (used on the project
  // landing page when no chat is selected) scopes to the whole project
  // tree (every chat under it + root-level shared files). Caller passes
  // one or the other — both empty means agent-wide (admin browser).
  const params = new URLSearchParams();
  if (sessionId) params.set("sessionId", sessionId);
  if (projectId) params.set("projectId", projectId);
  const qs = params.toString();
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/files${qs ? "?" + qs : ""}`,
  );
  if (!res.ok) return [];
  const data = await res.json();
  return (data.files || []) as WorkspaceFile[];
}

// getScopePreview returns the live dev-server preview for the current chat
// scope (sessionId for a loose chat, projectId for a project), or status
// "none" when nothing is running. Backs the workspace panel's "open
// preview" entry.
export interface ScopePreview {
  previewUrl?: string;
  status: string; // none|scaffolding|starting|running|sleeping|crashed
}
export async function getScopePreview(
  agentId: string,
  sessionId?: string,
  projectId?: string,
): Promise<ScopePreview> {
  const params = new URLSearchParams();
  if (sessionId) params.set("sessionId", sessionId);
  if (projectId) params.set("projectId", projectId);
  const qs = params.toString();
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/preview${qs ? "?" + qs : ""}`,
  );
  if (!res.ok) return { status: "none" };
  const data = await res.json().catch(() => ({ status: "none" }));
  return { previewUrl: data.previewUrl as string | undefined, status: (data.status as string) || "none" };
}

// getScopePreviewLogs tails the build/dev log for the current chat scope.
// The preview panel polls it while the app is scaffolding so the user sees
// the live pnpm-install output instead of an opaque spinner. Returns "" when
// there's nothing yet (no runtime, or the scaffold hasn't written a line).
export async function getScopePreviewLogs(
  agentId: string,
  sessionId?: string,
  projectId?: string,
  tail = 400,
): Promise<string> {
  const params = new URLSearchParams();
  if (sessionId) params.set("sessionId", sessionId);
  if (projectId) params.set("projectId", projectId);
  if (tail > 0) params.set("tail", String(tail));
  const qs = params.toString();
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/preview/logs${qs ? "?" + qs : ""}`,
  );
  if (!res.ok) return "";
  const data = await res.json().catch(() => ({ logs: "" }));
  return (data.logs as string) || "";
}

// getChangedFiles returns only the files the agent created/modified vs the
// template baseline (git diff in the running app). `available` is false when
// there's no live runtime/baseline — the caller then lists all files.
export async function getChangedFiles(
  agentId: string,
  sessionId?: string,
  projectId?: string,
): Promise<{ files: WorkspaceFile[]; available: boolean }> {
  const params = new URLSearchParams();
  if (sessionId) params.set("sessionId", sessionId);
  if (projectId) params.set("projectId", projectId);
  const qs = params.toString();
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/changed-files${qs ? "?" + qs : ""}`,
  );
  if (!res.ok) return { files: [], available: false };
  const data = await res.json().catch(() => ({ files: [], available: false }));
  return { files: (data.files || []) as WorkspaceFile[], available: !!data.available };
}

// Chat
export interface ChatHistoryMessage {
  role: "user" | "assistant" | "tool";
  content?: string;
  // Set to "compaction_notice" for auto-compaction notice entries so the
  // chat UI can render them as centered system bubbles instead of normal
  // assistant messages.
  kind?: string;
  toolCalls?: { id: string; name: string; arguments: string }[];
  name?: string;
  toolCallId?: string;
  // For role==="tool" this carries the sandbox flag etc.; for
  // role==="assistant" it can carry iterationCapReached / iterationCapValue
  // so the chat UI can badge the bubble on history reload.
  metadata?: ToolResultMetadata;
  timestamp?: number; // unix ms — places produced files under the message that created them
  // Set on user-role messages whose original turn carried image
  // attachments. The chat UI renders these as inline thumbnails on
  // bubbles loaded from history.
  imageUrls?: string[];
  // Populated for user turns that arrived via an IM bridge (Discord,
  // Telegram, ...). The chat panel renders an avatar + nickname header
  // on each such bubble so the agent owner can see who they're looking
  // at. None of these reach the LLM — they live on Message.Metadata
  // and are stripped from the persisted Content too.
  senderName?: string;
  senderAvatarUrl?: string;
  senderId?: string;
  senderChannel?: string;
  /** session_messages.seq (0-based) — daily-diary #seq-N deep links scroll to this. */
  seq?: number;
}

export interface TodoItem {
  text: string;
  done: boolean;
}

export interface TodoState {
  items: TodoItem[];
  raw: string;
}

// getChatTodo fetches the per-session todo.md the agent maintains.
// Returns {items: [], raw: ""} when no file exists yet (fresh session
// or a turn that didn't use the todo convention) — caller should hide
// the panel in that case.
export async function getChatTodo(agentId: string, sessionId: string): Promise<TodoState> {
  if (!agentId || !sessionId) return { items: [], raw: "" };
  const res = await apiFetch(
    `/api/chat/todo?agentId=${encodeURIComponent(agentId)}&sessionId=${encodeURIComponent(sessionId)}`,
  );
  if (!res.ok) return { items: [], raw: "" };
  const data = await res.json().catch(() => ({}));
  return {
    items: Array.isArray(data?.items) ? data.items : [],
    raw: typeof data?.raw === "string" ? data.raw : "",
  };
}

export async function getChatHistory(agentId: string, sessionId: string): Promise<ChatHistoryMessage[]> {
  const res = await apiFetch(`/api/chat/history?agentId=${encodeURIComponent(agentId)}&sessionId=${encodeURIComponent(sessionId)}`);
  if (!res.ok) return [];
  const data = await res.json();
  // Backend wraps in { history: [...] }; older shape was a raw array.
  if (Array.isArray(data?.history)) return data.history;
  return Array.isArray(data) ? data : [];
}

// ChatHistoryWithCursor returns the same history list plus the latest
// chat_events.seq for this session — the resume cursor that the
// subscribe SSE wants. Use this when mounting the chat panel; the
// cursor is fed into /api/chat/subscribe?since=N so a freshly reloaded
// page picks up any in-flight turn that's still streaming on the
// server.
export interface ChatHistoryResult {
  history: ChatHistoryMessage[];
  latestEventSeq: number; // -1 when there's nothing logged yet
}

export async function getChatHistoryWithCursor(agentId: string, sessionId: string): Promise<ChatHistoryResult> {
  const res = await apiFetch(`/api/chat/history?agentId=${encodeURIComponent(agentId)}&sessionId=${encodeURIComponent(sessionId)}`);
  if (!res.ok) return { history: [], latestEventSeq: -1 };
  const data = await res.json();
  const history: ChatHistoryMessage[] = Array.isArray(data?.history)
    ? data.history
    : Array.isArray(data) ? data : [];
  const seqRaw = data?.latestEventSeq;
  const latestEventSeq = typeof seqRaw === "number" ? seqRaw : -1;
  return { history, latestEventSeq };
}

export interface ChatSessionEntry {
  id: string;
  // channel/accountId/chatId let the sidebar render a per-channel icon
  // and the chats page tell apart "the same agent's wechat thread vs
  // its web thread". Empty channel means "legacy row that escaped the
  // backfill" — falls back to web styling on the UI side.
  channel?: string;
  accountId?: string;
  chatId?: string;
  // projectId groups this chat under a per-(user, agent) project.
  // Empty = loose chat (rendered in the flat Chats section).
  projectId?: string;
  title?: string;
  preview: string;
  thumbnailUrl?: string;
  createdAt?: number;
  updatedAt?: number;
}

export interface ProjectEntry {
  id: string;
  name: string;
  description?: string;
  createdAt?: string;
  updatedAt?: string;
}

export async function listProjects(agentId: string): Promise<ProjectEntry[]> {
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/projects`,
  );
  if (!res.ok) return [];
  const data = await res.json();
  return Array.isArray(data?.projects) ? data.projects : [];
}

export async function createProject(
  agentId: string,
  req: { name: string; description?: string },
): Promise<ProjectEntry | { error: string }> {
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/projects`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
    },
  );
  return res.json();
}

export async function updateProject(
  agentId: string,
  projectId: string,
  req: { name?: string; description?: string },
): Promise<ProjectEntry | { error: string }> {
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/projects/${encodeURIComponent(projectId)}`,
    {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
    },
  );
  return res.json();
}

// deleteProject returns a structured shape because the server replies
// 409 when the project still owns chats — surface sessionCount so the
// caller can render a useful prompt instead of just "delete failed".
export async function deleteProject(
  agentId: string,
  projectId: string,
): Promise<{ ok?: boolean; error?: string; sessionCount?: number }> {
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/projects/${encodeURIComponent(projectId)}`,
    { method: "DELETE" },
  );
  return res.json();
}


// AdminChatSessionEntry extends ChatSessionEntry with the (user, agent)
// ownership info needed to render a cross-tenant Chats listing — agent
// name + owner display fields, joined server-side so the client doesn't
// fan out per-agent. Backed by GET /api/admin/chats (super_admin only).
export interface AdminChatSessionEntry extends ChatSessionEntry {
  agentId: string;
  agentName?: string;
  userId: string;
  ownerUsername?: string;
  ownerDisplayName?: string;
  ownerEmail?: string;
}

export async function adminListChats(): Promise<AdminChatSessionEntry[]> {
  const res = await apiFetch("/api/admin/chats");
  if (!res.ok) return [];
  const data = await res.json();
  return Array.isArray(data?.sessions) ? data.sessions : [];
}

export async function getChatSessions(agentId: string): Promise<ChatSessionEntry[]> {
  const res = await apiFetch(`/api/chat/sessions?agentId=${encodeURIComponent(agentId)}`);
  if (!res.ok) return [];
  const data = await res.json();
  // Backend wraps the list in { sessions: [...] }. Tolerate raw array
  // shape too in case an older deployment is still around.
  if (Array.isArray(data?.sessions)) return data.sessions;
  return Array.isArray(data) ? data : [];
}

export async function renameChatSession(agentId: string, sessionId: string, title: string) {
  const res = await apiFetch(`/api/chat/sessions/${encodeURIComponent(sessionId)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ agentId, title }),
  });
  return res.json();
}

export async function deleteChatSession(agentId: string, sessionId: string) {
  const res = await apiFetch(
    `/api/chat/sessions/${encodeURIComponent(sessionId)}?agentId=${encodeURIComponent(agentId)}`,
    { method: "DELETE" },
  );
  return res.json();
}

// moveChatSessionToProject reassigns a chat to a project (or detaches
// it back to the loose-chat list when projectId is ""). Backs the
// sidebar drag-and-drop affordance. Returns { ok } on success;
// { error, code? } on failure — code="destination_exists" when the
// target workspace dir already has files (defensive 409).
export async function moveChatSessionToProject(
  agentId: string,
  sessionId: string,
  projectId: string,
): Promise<{ ok?: boolean; error?: string; code?: string }> {
  const res = await apiFetch(
    `/api/chat/sessions/${encodeURIComponent(sessionId)}/project`,
    {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ agentId, projectId }),
    },
  );
  return res.json();
}

export async function sendChat(agentId: string, sessionId: string, message: string): Promise<{ response: string }> {
  const res = await apiFetch("/api/chat", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ agentId, sessionId, message }),
  });
  return res.json();
}

// steerChat buffers a message into an in-flight turn for the session.
// Resolves true when the server folded it into a running turn (200),
// false when no turn is active (409) — caller should then fall back to
// a normal sendChatStream. Throws only on unexpected/transport errors.
export async function steerChat(
  agentId: string,
  sessionId: string,
  message: string,
  projectId?: string,
): Promise<boolean> {
  const res = await apiFetch("/api/chat/steer", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      agentId,
      sessionId,
      projectId: projectId || undefined,
      message,
    }),
  });
  if (res.status === 409) return false;
  if (!res.ok) throw new Error(`steer failed: ${res.status}`);
  const data = await res.json().catch(() => ({}));
  return data?.buffered === true;
}

// KnowledgeSource is one [K#]-citable KB source the agent attached to an
// assistant message (metadata.knowledgeSources). The chat renderer turns
// [K#] markers in the reply into clickable badges.
export interface KnowledgeSource {
  id: string; // "K1"
  file: string; // source title
  kind?: "wiki" | "kb";
  pageType?: "source" | "concept" | "entity" | "query";
  chunk?: number; // 0-based chunk index
}

export interface ToolResultMetadata {
  sandbox?: boolean;
  // Stamped on the forced-final-delivery assistant message that the
  // backend emits when the per-turn tool-iteration cap was hit. Lets the
  // UI surface a small badge so the user knows the answer was synthesized
  // under-budget and may be incomplete.
  iterationCapReached?: boolean;
  iterationCapValue?: number;
  // Stamped on assistant messages produced by plan mode (composer toggle).
  // The bubble is a plan, not an execution result — UI shows a distinct
  // badge so the user knows to review it and reply with "go" (or edits).
  planMode?: boolean;
  // [K#]-numbered KB sources for this turn's retrieval; the chat renderer
  // turns [K1]/[K2]… markers in the reply into clickable citation badges.
  knowledgeSources?: KnowledgeSource[];
}

export interface ChatStreamEvent {
  type:
    | "content"
    | "content_delta"
    | "tool_call"
    | "tool_result"
    | "steer"
    | "error"
    | "done"
    | "subagent_progress"
    | "auth_prompt"
    | "compaction_notice"
    | "skill_pending";
  // Per-session monotonic sequence assigned by chat_events. Lets the
  // chat page dedupe events arriving on both the active POST stream
  // and the parallel /api/chat/subscribe SSE connection. -1 means
  // "not assigned" (legacy / pre-persist code path).
  //
  // content_delta is intentionally NOT persisted (would generate 100+
  // rows per turn for no replay value — the trailing `content` event
  // carries the full final text). So content_delta always arrives
  // with seq=-1; the panel must accept it without dedup.
  seq?: number;
  data?: {
    content?: string;
    // delta is the incremental text appended to the in-flight
    // assistant bubble for content_delta events.
    delta?: string;
    id?: string;
    name?: string;
    arguments?: string;
    result?: string;
    message?: string;
    metadata?: ToolResultMetadata;
    // subagent_progress payload — only populated when type === "subagent_progress".
    iteration?: number;
    max?: number;
    phase?: "thinking" | "running" | "final-delivery" | "done";
    tools?: string[];
    // auth_prompt payload — only populated when type === "auth_prompt".
    // Web front-end renders this as the authorization prompt bubble; IM
    // channels get a plain-text content event fallback (see loop_auth.go).
    description?: string;
    options?: { cmd: string; label_zh: string; label_en: string }[];
    // compaction_notice payload — the backend's pre-formatted text plus
    // the raw before/after token counts. The web UI displays `content`
    // directly (no client-side formatting) so the live bubble matches
    // what history reload returns.
    before?: number;
    after?: number;
    retained_turns?: number;
    // skill_pending payload — only populated when type === "skill_pending".
    // Emitted post-turn when pendingSkillNames(agentHome) > 0 to surface
    // "run `fluctio skill approve <name>`" to the chatter. See loop.go
    // runPostTurn emit + loop_auth.go emitAuthPrompt for the shape parity.
    count?: number;
    names?: string[];
  };
}

// currentLocale reads the chatter's i18n locale straight from localStorage
// (same key i18n.tsx uses: "fluctio-locale"). Read here in the API layer so
// every sendChatStream call carries the locale without each caller having
// to thread it through. Returns "en" outside the browser (SSR).
function currentLocale(): string {
  if (typeof window === "undefined") return "en";
  return localStorage.getItem("fluctio-locale") || "en";
}

export async function sendChatStream(
  agentId: string,
  sessionId: string,
  message: string,
  onEvent: (evt: ChatStreamEvent) => void,
  signal?: AbortSignal,
  imageUrls?: string[],
  // projectId, when set, is the "this chat belongs to project X" hint
  // the URL carries (`?project=<pid>`) before any session row exists.
  // Server stamps it on the first SaveSession; subsequent turns ignore
  // it (the row is authoritative).
  projectId?: string,
  // params is a free-form blob the backend forwards as
  // bus.InboundMessage.Params. The agent loop reads recognized keys
  // (planMode etc.) directly; unrecognized keys land in a "Client
  // Parameters" system message via renderClientParams.
  params?: Record<string, unknown>,
): Promise<void> {
  const res = await apiFetch("/api/chat/stream", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      agentId,
      sessionId,
      projectId: projectId || undefined,
      message,
      imageUrls: imageUrls ?? [],
      // Lang follows the chatter's i18n locale so the backend can localize
      // slash-command replies (agent.popLang lifts it onto Lang). Spread
      // after caller params so the locale always reflects the live setting.
      params: { ...(params || {}), lang: currentLocale() },
    }),
    signal,
  });
  if (!res.ok) {
    let msg = `stream failed: ${res.status}`;
    try {
      const data = await res.json();
      if (data?.error) msg = String(data.error);
    } catch { /* non-JSON body — keep status fallback */ }
    throw new Error(msg);
  }
  const contentType = res.headers.get("content-type") || "";
  if (contentType && !contentType.includes("text/event-stream")) {
    let msg = "stream failed: unexpected response";
    try {
      const body = await res.text();
      if (body) {
        try {
          const data = JSON.parse(body);
          msg = String(data?.error || data?.message || msg);
        } catch {
          msg = body.slice(0, 240);
        }
      }
    } catch { /* keep fallback */ }
    throw new Error(msg);
  }
  if (!res.body) throw new Error("stream failed: no body");

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let sawEvent = false;

  // Reader loop exits on either an explicit {type:"done"} event from the
  // server or a clean stream end (done flag from getReader). We tear down
  // early on "done" so any trailing bytes that may have been queued behind
  // the final flush don't get re-parsed and surfaced as spurious errors.
  let finished = false;
  while (!finished) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });

    const lines = buffer.split("\n");
    buffer = lines.pop() || "";

    for (const line of lines) {
      if (!line.startsWith("data: ")) continue;
      try {
        const evt = JSON.parse(line.slice(6)) as ChatStreamEvent;
        sawEvent = true;
        onEvent(evt);
        if (evt.type === "done") {
          finished = true;
        }
      } catch {
        throw new Error("stream failed: malformed event from server");
      }
    }
  }
  try { await reader.cancel(); } catch { /* ignore */ }
  if (!sawEvent) {
    throw new Error("stream ended without any response from the server");
  }
}

export interface UploadedFile {
  path: string;
  size: number;
}

export async function uploadAgentFiles(
  agentId: string,
  sessionId: string,
  files: File[],
  projectId?: string,
): Promise<UploadedFile[]> {
  const fd = new FormData();
  for (const f of files) fd.append("file", f, f.name);
  // Scope the upload to the exact dir the sandbox will mount. projectId is
  // required for project chats (files must land in projects/<pid>/), which
  // the session lookup can't resolve until the session is minted on the
  // first message — and the client uploads before that fires.
  const params = new URLSearchParams();
  if (sessionId) params.set("sessionId", sessionId);
  if (projectId) params.set("projectId", projectId);
  const qs = params.toString() ? `?${params.toString()}` : "";
  const res = await apiFetch(`/api/agents/${encodeURIComponent(agentId)}/files${qs}`, {
    method: "POST",
    body: fd,
  });
  if (!res.ok) throw new Error(`upload failed: ${res.status}`);
  const data = await res.json();
  return (data.files || []) as UploadedFile[];
}

// Agents
export async function getAgents(): Promise<AgentDetail[]> {
  const res = await apiFetch("/api/agents");
  if (!res.ok) {
    // 401 etc. return a JSON error envelope — throw so callers fall back
    // to [] instead of crashing on .map of a non-array.
    throw new Error(`getAgents failed: ${res.status}`);
  }
  const data = await res.json();
  // Backend returns { agents: [...] }. Tolerate raw array too in case an
  // older handler is still around.
  if (Array.isArray(data?.agents)) return data.agents as AgentDetail[];
  return Array.isArray(data) ? (data as AgentDetail[]) : [];
}

// Single-agent detail. Falls back through the same permission rules as
// the rest of /api/agents/{id} — owner or super_admin can fetch. Used
// by the chat header to resolve a name when the agent isn't in the
// caller's own list (admin viewing another user's agent).
export async function getAgent(id: string): Promise<AgentDetail | null> {
  const res = await apiFetch(`/api/agents/${encodeURIComponent(id)}`);
  if (!res.ok) return null;
  const data = await res.json();
  return (data?.agent as AgentDetail) || null;
}

// getAgentStatus surfaces the raw HTTP status alongside the agent so
// callers can branch on 403 (forbidden — not the owner, not public)
// vs 404 (no such agent) vs success. The plain getAgent() collapses
// every failure to null, which the chat page can't tell apart.
export async function getAgentStatus(
  id: string,
): Promise<{ status: number; agent: AgentDetail | null }> {
  const res = await apiFetch(`/api/agents/${encodeURIComponent(id)}`);
  if (!res.ok) return { status: res.status, agent: null };
  const data = await res.json();
  return { status: res.status, agent: (data?.agent as AgentDetail) || null };
}

// AgentRegisteredTool is what /api/agents/{id}/tools/registered returns
// per tool: name (the canonical identifier the allowlist uses),
// description (one-liner for the picker UI), and source (where the tool
// came from: builtin / mcp / plugin). Stable order is guaranteed by the
// backend so dashboard renders are deterministic.
export interface AgentRegisteredTool {
  name: string;
  description: string;
  source: "builtin" | "mcp" | "plugin" | string;
}

// listAgentRegisteredTools fetches the live tool registry for an agent.
// Drives the Tools tab's allowlist checkbox picker so operators can
// click rather than type names from memory. Returns null on auth failure
// or when the agent isn't loaded (the backend 404s in that case).
export async function listAgentRegisteredTools(
  id: string,
): Promise<AgentRegisteredTool[] | null> {
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(id)}/tools/registered`,
  );
  if (!res.ok) return null;
  const data = await res.json();
  return (data?.tools as AgentRegisteredTool[]) || [];
}

export async function createAgent(agent: Partial<AgentDetail>) {
  const res = await apiFetch("/api/agents", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(agent),
  });
  return res.json();
}

export interface AgentSkillsConfig {
  disabled?: string[];
  alwaysLoad?: string[];
}

// The backend accepts model / soul / skills / providers on update.
// `AgentDetail.skills` is a flat string[] (legacy), but per-agent skills
// config is really { disabled, alwaysLoad } — use an explicit payload
// type so the two shapes don't collide in the type system.
export interface AgentUpdatePayload {
  name?: string;
  description?: string;
  model?: string;
  soul?: string;
  skills?: AgentSkillsConfig;
  // Whole-map replace: omit to leave providers untouched, send {} to
  // clear them, or send the full desired map to replace.
  providers?: Record<string, ProviderData>;
  // PromptMode selects how heavily the framework system prompt
  // participates: "agent" (full, default), "chatbot" (slim — drops
  // task-delegation / tool-use discipline / workspace-update so
  // companion / role-play personas stay in character), "customize"
  // (only the date anchor + bootstrap files — author writes the whole
  // system prompt themselves via SOUL.md / IDENTITY.md). Pass "" to clear.
  promptMode?: "" | "agent" | "chatbot" | "customize";
  // Guidance per-agent override: "guided" (default, firm) or
  // "autonomous" (soft). Pass "" to clear the override.
  guidance?: "" | "autonomous" | "guided";
  // Multi-bubble per-agent override (applies to all IM channels).
  // Tri-state: omit to leave the saved value alone; pass true/false to
  // set explicit; pass `splitRepliesReset: true` to delete the override
  // so default behavior (single bubble) applies.
  splitReplies?: boolean;
  splitRepliesReset?: boolean;
  // Auto-persist per-agent override. Same tri-state semantics as
  // splitReplies. When true, every N turns the runtime runs a small
  // LLM call that distills the conversation into USER.md (chatter
  // profile) and MEMORY.md (long-term facts) — see Agent.autoPersist.
  autoPersist?: boolean;
  autoPersistReset?: boolean;
  // Shared identity across channels. When true, all channels bound
  // to this agent use the owner's user_id as chatter so sessions
  // and memory are shared across web + IM channels.
  sharedIdentity?: boolean;
  // Per-agent plugin enable overrides (patch semantics — keys not in
  // the map are preserved). Pass pluginsReset:true to clear ALL
  // per-agent overrides and fall back to system-wide enable state.
  plugins?: Record<string, boolean>;
  pluginsReset?: boolean;
  // MCP servers whole-map replace. Omit to leave untouched, send {}
  // to clear, or send the full desired map to replace.
  mcpServers?: Record<string, MCPServerConfig>;
  mcpServersReset?: boolean;
  // Default UI language for slash-command replies when the inbound
  // source carries none (IM channels). "" clears the override (falls
  // back to runtime default, Chinese); "en" / "zh-CN" sets it.
  language?: string;
  // CompactionMode selects the margin aggressiveness for the dynamic
  // compaction threshold: "conservative" / "balanced" / "aggressive".
  // "" clears the override (falls back to balanced default). Omit to
  // leave the saved value unchanged.
  compactionMode?: "" | "conservative" | "balanced" | "aggressive";
  // CompactionThreshold is an operator-set fixed compaction threshold
  // (tokens). 0 = use dynamic computation from compactionMode. Omit
  // to leave the saved value unchanged.
  compactionThreshold?: number;
  // Per-agent KB config blob (whole-replace: caller must read existing kb
  // via getAgentConfig first to avoid clobbering wikiRatio/threshold/etc).
  // The dedup thresholds (articleDupHigh/Mid/flashDupThreshold/todoDupThreshold)
  // gate inbound write dedup; nil = built-in default.
  kb?: AgentKBCfg;
}

export async function updateAgent(id: string, agent: AgentUpdatePayload) {
  const res = await apiFetch(`/api/agents/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(agent),
  });
  return res.json();
}

// CompactionPreview is what /api/agents/{id}/compaction/preview returns.
// The context-page compaction selector uses it to show what each mode
// would threshold at before the operator picks one.
export interface CompactionPreview {
  contextWindow: number;
  maxTokens: number;
  systemPromptTokens: number;
  modes: { conservative: number; balanced: number; aggressive: number };
  manualThreshold?: number;
  compactionMode?: string;
}

export async function getCompactionPreview(agentId: string): Promise<CompactionPreview | null> {
  const r = await apiFetch(`/api/agents/${encodeURIComponent(agentId)}/compaction/preview`);
  if (!r.ok) return null;
  return r.json();
}

// HookPlugin is the metadata shape returned by /api/plugins/hook —
// read-only listing of hook-type plugins available on this install.
// Operators pick which to enable per-agent on the Context page.
export interface HookPlugin {
  id: string;
  name?: string;
  description?: string;
  version?: string;
}

export async function listHookPlugins(): Promise<HookPlugin[]> {
  try {
    const res = await apiFetch("/api/plugins/hook");
    if (!res.ok) return [];
    return (await res.json()) as HookPlugin[];
  } catch {
    return [];
  }
}

export interface MCPServerConfig {
  type: "http" | "stdio";
  url?: string;
  headers?: Record<string, string>;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
}

export interface AgentFileConfig {
  model?: string;
  maxTokens?: number;
  temperature?: number;
  maxToolIterations?: number;
  workspace?: string;
  skills?: AgentSkillsConfig;
  providers?: Record<string, ProviderData>;
  mcpServers?: Record<string, MCPServerConfig>;
  kb?: AgentKBCfg;
  diary?: AgentDiaryCfg;
  // admins mirrors config.AgentFileConfig.Admins — per-channel admin
  // platform IDs (IM identity claim flow binds via /claim <code>).
  admins?: Record<string, string[]>;
  // language mirrors config.AgentFileConfig.Language — default UI
  // language for slash replies on IM channels. Read via getAgentConfig.
  language?: string;
}

// AgentKBCfg mirrors config.AgentKBCfg (slice 4b-1) — per-agent KB
// auto-query config. The web Knowledge page reads/writes it via
// getAgentConfig/updateAgent, bypassing the agentcli CLI whitelist gap.
export interface AgentKBCfg {
  enabled?: boolean;
  autoMode?: string;
  keywords?: string[];
  maxResults?: number;
  searchMode?: string;
  emptyAction?: string;
  /** Fraction [0,1] of result slots for wiki pages vs kb_entries. Default 0.5. */
  wikiRatio?: number;
  /** Minimum normalized relevance [0,1] for a wiki result to be kept. Default 0.45. */
  threshold?: number;
  /** IM channel for due-todo reminders (wechat/qq/telegram/...). Default wechat. */
  reminderChannel?: string;
  // Dedup thresholds for inbound KB writes (nil/undefined = built-in default
  // 0.90/0.72/0.85/0.78). Edited from the Knowledge page's dedup panel.
  articleDupHigh?: number;
  articleDupMid?: number;
  flashDupThreshold?: number;
  todoDupThreshold?: number;
}

// AgentDiaryCfg mirrors config.AgentDiaryCfg — per-agent daily-diary
// generation config. Read/written via getAgentConfig/updateAgent under
// the Knowledge settings tab.
export interface AgentDiaryCfg {
  enabled?: boolean;
  /** Daily generation time "HH:MM" UTC+8. Default "02:30". */
  cronTime?: string;
  /** Blindspot-detection strength: ""/"blindspots" (default), "off", "deep". */
  thinkingMode?: string;
}

// --- Daily diary types + API (per-agent daily-diary generator) ---
export interface DiarySegRef {
  session: string;
  start: number;
  end: number;
}
export interface DiaryTheme {
  title: string;
  summary: string;
  points: string[];
  topics?: string[];
  session?: string;
  segments?: DiarySegRef[];
}
export interface DiaryBlindspot {
  point: string;
  reason: string;
}
export interface DailyDiary {
  agentId: string;
  date: string;
  overview: string;
  /** Set by GET while a manual generation is running (row not written yet). */
  generating?: boolean;
  themes: DiaryTheme[];
  blindspots: DiaryBlindspot[];
  archives: string[];
  model: string;
  generatedAt: string;
}

export async function listDiary(agentId: string, from?: string, to?: string): Promise<DailyDiary[]> {
  const p = new URLSearchParams();
  if (from) p.set("from", from);
  if (to) p.set("to", to);
  const res = await apiFetch(`/api/agents/${agentId}/diary?${p}`);
  if (!res.ok) return [];
  const data = await res.json();
  return Array.isArray(data) ? data : [];
}
export async function getDiary(agentId: string, date: string): Promise<DailyDiary | null> {
  const res = await apiFetch(`/api/agents/${agentId}/diary/${date}`);
  if (!res.ok) return null;
  return res.json();
}
export async function generateDiary(
  agentId: string,
  date?: string,
): Promise<{ status: string; date: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/diary/generate`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(date ? { date } : {}),
  });
  return res.json();
}

// --- Knowledge base types + API (slice 4a REST handlers) ---
export interface KBSource {
  id: string;
  agent_id: string;
  title: string;
  source_type: string;
  source_ref: string;
  entry_count: number;
  total_chars: number;
  wiki_generated_at?: string;
  created_at: string;
  updated_at: string;
  /** Content type: article (default) / flash / todo. */
  type?: string;
  /** Todo lifecycle: pending / in_progress / done / cancelled. */
  status?: string;
  start_at?: string;
  end_at?: string;
  /** Last due-reminder push timestamp (dedup for the reminders sweep). */
  reminded_at?: string;
}
export interface KBStats { source_count: number; entry_count: number; total_chars: number; }
export interface KBEntry { id: number; source_id: string; chunk_index: number; content: string; }

export async function listKBSources(agentId: string): Promise<KBSource[]> {
  const res = await apiFetch(`/api/agents/${agentId}/kb/sources`);
  if (!res.ok) return [];
  const data = await res.json().catch(() => []);
  return Array.isArray(data) ? data : [];
}
export async function getKBStats(agentId: string): Promise<KBStats> {
  const res = await apiFetch(`/api/agents/${agentId}/kb/stats`);
  return res.json().catch(() => ({ source_count: 0, entry_count: 0, total_chars: 0 }));
}
export async function listKBEntries(agentId: string, sourceId: string): Promise<KBEntry[]> {
  const res = await apiFetch(`/api/agents/${agentId}/kb/sources/${sourceId}/entries`);
  if (!res.ok) return [];
  const data = await res.json().catch(() => []);
  return Array.isArray(data) ? data : [];
}
export async function kbIngestText(agentId: string, title: string, content: string): Promise<{ source_id?: string; chars?: number; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/kb/ingest/text`, {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ title, content }),
  });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json();
}
export async function kbIngestURL(agentId: string, url: string, title?: string): Promise<{ source_id?: string; chars?: number; title?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/kb/ingest/url`, {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ url, title }),
  });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json();
}
export async function deleteKBSource(agentId: string, sourceId: string): Promise<{ status?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/kb/sources/${sourceId}`, { method: "DELETE" });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json();
}
export async function kbSaveFlash(agentId: string, content: string): Promise<{ source_id?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/kb/flash`, {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ content }),
  });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json();
}
export async function kbSaveTodo(
  agentId: string,
  content: string,
  status?: string,
  startAt?: string,
  endAt?: string,
): Promise<{ source_id?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/kb/todo`, {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ content, status, start_at: startAt, end_at: endAt }),
  });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json();
}
export async function kbUpdateTodo(
  agentId: string,
  sourceId: string,
  patch: { status?: string; start_at?: string; end_at?: string },
): Promise<{ status?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/kb/todos/${sourceId}`, {
    method: "PATCH", headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json();
}
export async function kbListTodos(agentId: string, status?: string): Promise<KBSource[]> {
  const q = status ? `?status=${encodeURIComponent(status)}` : "";
  const res = await apiFetch(`/api/agents/${agentId}/kb/todos${q}`);
  if (!res.ok) return [];
  const data = await res.json().catch(() => []);
  return Array.isArray(data) ? data : [];
}

// A KB write parked at the mid dedup tier pending user merge / create / skip.
export type KBPending = {
  id: string;
  title: string;
  content: string;
  source_type: string;
  source_ref: string;
  candidate_source_id: string;
  candidate_title: string;
  similarity: number;
  created_at: string;
  expires_at: string;
};

export async function listKBPending(agentId: string): Promise<KBPending[]> {
  const res = await apiFetch(`/api/agents/${agentId}/kb/pending`);
  if (!res.ok) return [];
  const data = await res.json().catch(() => []);
  return Array.isArray(data) ? data : [];
}

export async function resolveKBPending(
  agentId: string,
  pendingId: string,
  action: "merge" | "create" | "skip",
): Promise<{ action?: string; source_id?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/kb/pending/${pendingId}/resolve`, {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ action }),
  });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json();
}
// --- Article deep-reading insights (深度解读): summary / quotes / actions / sprouts. ---
export interface InsightPoint { label: string; text: string; }
export interface InsightTopic { heading: string; points: InsightPoint[]; }
export interface InsightChapter { title: string; body: string; }
export interface InsightSummary { core: string; topics: InsightTopic[]; chapters: InsightChapter[]; }
export interface InsightQuote { text: string; tag: string; verified?: boolean; }
export interface InsightSprout { index: number; emoji: string; title: string; seed: string; body: string; aha: string; }
export interface InsightEchoItem { perspective: string; label: string; quote: string; source: string; }
export interface InsightEcho { seed_quote: string; seed_comment: string; items: InsightEchoItem[]; }
export interface InsightSprouts { intro: string; items: InsightSprout[]; echo?: InsightEcho; }
export interface ArticleInsights {
  source_id: string;
  summary: InsightSummary;
  quotes: InsightQuote[];
  actions: string[];
  sprouts: InsightSprouts;
  generated_at: string;
}
export async function kbGetInsights(agentId: string, sourceId: string): Promise<ArticleInsights | null> {
  const res = await apiFetch(`/api/agents/${agentId}/kb/sources/${sourceId}/insights`);
  if (!res.ok) return null;
  return res.json().catch(() => null);
}
export async function kbGenerateInsights(agentId: string, sourceId: string): Promise<ArticleInsights | { error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/kb/sources/${sourceId}/insights/generate`, {
    method: "POST", headers: { "Content-Type": "application/json" },
  });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json();
}

export async function generateWiki(agentId: string, sourceIds: string[], force?: boolean): Promise<{ status?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/wiki/generate`, {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ source_ids: sourceIds, force }),
  });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json();
}

// --- Wiki types + API (slice 4a REST handlers + slice 3 wiki pkg) ---
export interface WikiPage {
  id: string;
  agent_id: string;
  page_type: string;
  slug: string;
  title: string;
  body?: string;
  summary: string;
  source_ids: string[];
  tags: string[];
  created_at: string;
  updated_at: string;
  revision: number;
}
export interface WikiLink { src_page_id: string; dst_page_id: string; relation: string; weight: number; }
export interface WikiStats { page_counts: Record<string, number>; total_pages: number; total_edges: number; }
export interface WikiGraph { nodes: WikiPage[]; edges: WikiLink[] | null; }
export interface WikiAutoGenCfg { enabled: boolean; interval?: number; model?: string; maxTokens?: number; }

export async function getWikiStats(agentId: string): Promise<WikiStats> {
  const res = await apiFetch(`/api/agents/${agentId}/wiki/stats`);
  return res.json().catch(() => ({ page_counts: {}, total_pages: 0, total_edges: 0 }));
}
export async function listWikiPages(agentId: string, type?: string): Promise<{ pages: WikiPage[]; total: number }> {
  const qs = type ? `?type=${encodeURIComponent(type)}` : "";
  const res = await apiFetch(`/api/agents/${agentId}/wiki/pages${qs}`);
  if (!res.ok) return { pages: [], total: 0 };
  return res.json().catch(() => ({ pages: [], total: 0 }));
}
export async function getWikiPage(agentId: string, pageId: string): Promise<WikiPage> {
  const res = await apiFetch(`/api/agents/${agentId}/wiki/pages/${encodeURIComponent(pageId)}`);
  return res.json();
}
export async function deleteWikiPage(agentId: string, pageId: string): Promise<{ status?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/wiki/pages/${encodeURIComponent(pageId)}`, { method: "DELETE" });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json();
}
export async function getWikiGraph(agentId: string): Promise<WikiGraph> {
  const res = await apiFetch(`/api/agents/${agentId}/wiki/graph`);
  return res.json().catch(() => ({ nodes: [], edges: [] }));
}
export async function getWikiProgress(agentId: string): Promise<{ total?: number; done?: number; failed?: number; status: string; updated_at?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/wiki/progress`);
  return res.json().catch(() => ({ status: "idle" }));
}

export interface WikiAutogenStatus {
  enabled: boolean;
  last_run?: string;      // RFC3339, omitted when never run
  last_status?: string;   // "" | "ok" | "partial" | "error" | "no_provider" | "no_sources"
  last_error?: string;
  pending?: number;       // KB sources whose wiki_generated_at is NULL
}
export async function getWikiAutogenStatus(agentId: string): Promise<WikiAutogenStatus> {
  const res = await apiFetch(`/api/agents/${agentId}/wiki/autogen-status`);
  if (!res.ok) return { enabled: false };
  return res.json().catch(() => ({ enabled: false }));
}

// --- Agent memory (wiki auto-gen config). Backend GET/PUT /api/agents/{id}/memory
// lands in slice 4c (MemoryCfg.WikiAutoGen); until then these return {}
// on the 404 and the wiki page's auto-gen toggle stays at its default. ---
export interface AgentMemory { wikiAutoGen?: WikiAutoGenCfg; [k: string]: any; }
export async function getAgentMemory(agentId: string): Promise<{ memory?: AgentMemory }> {
  const res = await apiFetch(`/api/agents/${agentId}/memory`);
  if (!res.ok) return {};
  return res.json().catch(() => ({}));
}

export type RecallFeedbackStat = { lambda: number; ups: number; downs: number };
export type RecallTuningState = {
  ok?: boolean;
  mmr_lambda?: number;
  min_relevance?: number;
  total_recalls?: number;
  explored_recalls?: number;
  feedback_stats?: RecallFeedbackStat[];
  error?: string;
};
// getAgentRecallTuning reads the agent's bandit state (current MMR lambda,
// recall counts, per-lambda feedback) for the recall-tuning panel.
export async function getAgentRecallTuning(agentId: string): Promise<RecallTuningState> {
  const res = await apiFetch(`/api/agents/${agentId}/recall-tuning`);
  if (!res.ok) return { ok: false, error: `HTTP ${res.status}` };
  return res.json().catch(() => ({ ok: false }));
}

export type RecallTestHit = {
  id: number;
  summary: string;
  topic?: string;
  keywords?: string[];
  created_at?: string;
  importance?: number;
  access_count?: number;
};
export type RecallTestResult = {
  ok?: boolean;
  results?: RecallTestHit[];
  note?: string;
  error?: string;
};
// previewRecall runs a basic recall preview (FTS + scoring, no MMR) for
// the tuning panel's test box.
export async function previewRecall(
  agentId: string,
  query: string,
  limit?: number,
): Promise<RecallTestResult> {
  const res = await apiFetch(`/api/agents/${agentId}/recall-test`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ query, limit }),
  });
  if (!res.ok) return { ok: false, error: `HTTP ${res.status}` };
  return res.json().catch(() => ({ ok: false }));
}
// setAgentRecallTuning manually overrides the agent's MMR lambda (the bandit
// keeps exploring/tuning from this new starting point).
export async function setAgentRecallTuning(
  agentId: string,
  mmrLambda: number,
): Promise<{ ok?: boolean; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/recall-tuning`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ mmr_lambda: mmrLambda }),
  });
  if (!res.ok) return { ok: false, error: `HTTP ${res.status}` };
  return res.json().catch(() => ({ ok: false }));
}

// setAgentRecallMinRelevance sets the memory-recall relevance threshold
// (0..1; 0 = no filter). Drops hits whose similarity is below it.
export async function setAgentRecallMinRelevance(
  agentId: string,
  minRelevance: number,
): Promise<{ ok?: boolean; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/recall-tuning`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ min_relevance: minRelevance }),
  });
  if (!res.ok) return { ok: false, error: `HTTP ${res.status}` };
  return res.json().catch(() => ({ ok: false }));
}

export type RecallSummaryPreview = { id: number; summary: string; topic?: string };
export type RecallEventView = {
  recall_id: string;
  lambda: number;
  explored: boolean;
  created_at: string;
  summaries: RecallSummaryPreview[];
};
export async function getRecentRecalls(
  agentId: string,
): Promise<{ ok?: boolean; events?: RecallEventView[]; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/recall-events`);
  if (!res.ok) return { ok: false, error: `HTTP ${res.status}` };
  return res.json().catch(() => ({ ok: false }));
}
export async function sendRecallFeedback(
  recallId: string,
  up: boolean,
): Promise<{ ok?: boolean; upgraded?: boolean; lambda?: number; error?: string }> {
  const res = await apiFetch(`/api/chat/recall-feedback`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ recall_id: recallId, up }),
  });
  if (!res.ok) return { ok: false, error: `HTTP ${res.status}` };
  return res.json().catch(() => ({ ok: false }));
}
export async function setAgentMemory(agentId: string, memory: AgentMemory): Promise<{ ok?: boolean; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/memory`, {
    method: "PUT", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ memory }),
  });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json().catch(() => ({ ok: true }));
}

export async function getAgentVectorization(agentId: string): Promise<{ vectorization?: VectorizationConfig }> {
  const res = await apiFetch(`/api/agents/${agentId}/vectorization`);
  if (!res.ok) return {};
  return res.json().catch(() => ({}));
}

export async function setAgentVectorization(agentId: string, vectorization: VectorizationConfig): Promise<{ ok?: boolean; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/vectorization`, {
    method: "PUT", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ vectorization }),
  });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json().catch(() => ({ ok: true }));
}

// System-level vectorization defaults (inherited by agents without an override).
export async function getSystemVectorization(): Promise<{ vectorization?: VectorizationConfig }> {
  const res = await apiFetch(`/api/vectorization`);
  if (!res.ok) return {};
  return res.json().catch(() => ({}));
}

export async function setSystemVectorization(vectorization: VectorizationConfig): Promise<{ ok?: boolean; error?: string }> {
  const res = await apiFetch(`/api/vectorization`, {
    method: "PUT", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ vectorization }),
  });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json().catch(() => ({ ok: true }));
}

// --- Scheduled backup config (system-level SQLite snapshots) ---
export interface BackupConfig {
  enabled?: boolean;
  cronTime?: string; // "HH:MM" UTC+8
  maxKeep?: number;
}
export interface BackupInfo {
  name: string;
  size: number;    // bytes
  modified: number; // unix seconds
}
export async function getSystemBackup(): Promise<{ backup?: BackupConfig }> {
  const res = await apiFetch(`/api/backup`);
  if (!res.ok) return {};
  return res.json().catch(() => ({}));
}
export async function setSystemBackup(backup: BackupConfig): Promise<{ ok?: boolean; error?: string }> {
  const res = await apiFetch(`/api/backup`, {
    method: "PUT", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ backup }),
  });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json().catch(() => ({ ok: true }));
}
export async function listBackups(): Promise<{ backups?: BackupInfo[] }> {
  const res = await apiFetch(`/api/backup/list`);
  if (!res.ok) return {};
  return res.json().catch(() => ({}));
}
export async function backupNow(): Promise<{ ok?: boolean; name?: string; size?: number; error?: string }> {
  const res = await apiFetch(`/api/backup/now`, { method: "POST" });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json().catch(() => ({ ok: true }));
}
export async function deleteBackup(name: string): Promise<{ ok?: boolean; error?: string }> {
  const res = await apiFetch(`/api/backup?file=${encodeURIComponent(name)}`, { method: "DELETE" });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json().catch(() => ({ ok: true }));
}

// --- Privacy config (agent PII scrubbing settings) ---
export interface PrivacyConfig {
  piiScrubbing?: { enabled?: boolean; entropy?: boolean };
  [k: string]: any;
}
export async function getAgentPrivacy(agentId: string): Promise<{ privacy?: PrivacyConfig }> {
  const res = await apiFetch(`/api/agents/${agentId}/privacy`);
  if (!res.ok) return {};
  return res.json().catch(() => ({}));
}
export async function setAgentPrivacy(agentId: string, privacy: PrivacyConfig): Promise<{ ok?: boolean; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/privacy`, {
    method: "PUT", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ privacy }),
  });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json().catch(() => ({ ok: true }));
}

// --- Memory config types (agent memory settings page) ---
export interface MemoryEmbeddingConfig {
  enabled: boolean;
  provider?: string;
  model?: string;
  apiKey?: string;
  apiBase?: string;
  dim?: number;
  dimEnabled?: boolean;
}
export interface MemoryRerankerConfig {
  enabled: boolean;
  provider?: string;
  model?: string;
  apiKey?: string;
  apiBase?: string;
}
export interface MemoryConfig {
  embedding?: MemoryEmbeddingConfig;
  reranker?: MemoryRerankerConfig;
  settings?: { enabled?: boolean };
  summaryModel?: string;
  kbEmbedding?: boolean;
  wikiEmbedding?: boolean;
  [k: string]: any;
}
// VectorizationConfig is the split-out vector namespace (embedding /
// reranker / kbEmbedding / wikiEmbedding). Settings + summaryModel stay
// on MemoryConfig; these four moved to /api/agents/{id}/vectorization.
export interface VectorizationConfig {
  embedding?: MemoryEmbeddingConfig;
  reranker?: MemoryRerankerConfig;
  kbEmbedding?: boolean;
  wikiEmbedding?: boolean;
  wikiThreshold?: number;
  [k: string]: any;
}
export async function reindexAgentMemory(agentId: string): Promise<{ ok?: boolean; processed?: number; failed?: number; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/memory/reindex`, { method: "POST" });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json().catch(() => ({ ok: true }));
}

// reindexWikiEmbeddings re-vectorizes wiki pages for an agent. By default it
// only embeds pages that lack a vector (incremental backfill); pass force=true
// to clear and re-embed every page (model switch).
export async function reindexWikiEmbeddings(agentId: string, force?: boolean): Promise<{ ok?: boolean; processed?: number; failed?: number; error?: string }> {
  const url = `/api/agents/${agentId}/wiki/reindex-embed${force ? "?force=true" : ""}`;
  const res = await apiFetch(url, { method: "POST" });
  if (!res.ok) return { error: `HTTP ${res.status}` };
  return res.json().catch(() => ({ ok: true }));
}

// testEmbedding pings /v1/embeddings with the inline credentials from the
// form (not the saved row), so the operator can verify apiBase/apiKey/
// model before saving. Mirrors the Models page's testProvider flow.
export async function testEmbedding(req: {
  apiBase: string;
  apiKey: string;
  model: string;
  dim?: number;
  dimEnabled?: boolean;
}): Promise<{ ok: boolean; error?: string; dim?: number }> {
  const res = await apiFetch("/api/memory/test-embedding", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function testReranker(req: {
  apiBase: string;
  apiKey: string;
  model: string;
}): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch("/api/memory/test-reranker", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

// Fetch the raw agent.json for one agent (per-agent overrides only — not
// the merged/resolved config). Used by the per-agent Models and Skills
// admin pages.
export async function getAgentConfig(id: string): Promise<AgentFileConfig> {
  const res = await apiFetch(`/api/agents/${id}/config`);
  return res.json();
}

export async function deleteAgent(id: string) {
  const res = await apiFetch(`/api/agents/${id}`, {
    method: "DELETE",
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok || body?.ok === false) {
    throw new Error(body?.error || `Delete failed (${res.status})`);
  }
  return body;
}

// Skills
export async function getSkills(): Promise<SkillInfo[]> {
  const res = await apiFetch("/api/skills");
  return res.json();
}

export async function deleteSkill(name: string) {
  const res = await apiFetch(`/api/skills/${name}`, {
    method: "DELETE",
  });
  return res.json();
}

// Per-agent skills: list what's installed in an agent's own home/skills dir.
// Agent-scoped skills shadow global ones with the same name.
export async function getAgentSkills(agentId: string): Promise<SkillInfo[]> {
  const res = await apiFetch(`/api/agents/${encodeURIComponent(agentId)}/skills`);
  return res.json();
}

export async function deleteAgentSkill(agentId: string, name: string) {
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/skills/${encodeURIComponent(name)}`,
    { method: "DELETE" },
  );
  return res.json();
}

// Search results use skills.sh's shape; clawhub has a different shape but the
// admin UI only wires skills.sh (primary registry). Callers that want clawhub
// go through installSkill with source="clawhub".
export interface SkillSearchResult {
  id: string;       // "<owner>/<repo>/<skillId>"
  skillId: string;  // folder name — also the slug passed to installSkill
  name: string;
  source: string;   // "<owner>/<repo>"
  installs: number;
}

export async function searchSkills(query: string): Promise<SkillSearchResult[]> {
  if (!query.trim()) return [];
  const res = await apiFetch(`/api/skills/search?source=skillssh&q=${encodeURIComponent(query)}`);
  if (!res.ok) return [];
  const data = await res.json();
  return (data.results || []) as SkillSearchResult[];
}

export interface InstallSkillRequest {
  name: string;
  source?: "skillssh" | "clawhub" | "github" | "auto";
  repo?: string;
  agent?: string;  // omit for global install (admin only)
}

export interface InstallSkillResponse {
  ok: boolean;
  source?: string;
  name?: string;
  version?: string;
  installedAt?: string;
  files?: number;
  error?: string;
  warnings?: string[];
}

export async function installSkill(req: InstallSkillRequest): Promise<InstallSkillResponse> {
  const res = await apiFetch("/api/skills/install", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

// uploadSkill installs a skill from a user-supplied .zip file. The zip is
// extracted into <agent>/skills/<name>/ on the backend (or the global
// skills dir when agentId is empty — admin only). `name` overrides the
// inferred folder name; leave undefined to let the server pick (common
// top-level dir → falls back to filename without extension).
export async function uploadSkill(
  file: File,
  agentId?: string,
  name?: string,
): Promise<InstallSkillResponse> {
  const fd = new FormData();
  fd.append("file", file, file.name);
  if (name) fd.append("name", name);
  const qs = agentId ? `?agent=${encodeURIComponent(agentId)}` : "";
  const res = await apiFetch(`/api/skills/upload${qs}`, {
    method: "POST",
    body: fd,
  });
  return res.json();
}

// --- Tools (provider-backed capabilities: web_search, image_gen, tts, ...) ---

export interface ToolProviderCatalog {
  name: string;
  label: string;
  needsKey: boolean;
  needsUrl: boolean;
  models: string[];
}

export interface ToolCategoryCatalog {
  name: string;
  label: string;
  providers: ToolProviderCatalog[];
}

export interface ToolProviderSettings {
  apiKey?: string;
  endpoint?: string;
  options?: Record<string, string>;
}

export interface ToolCategorySettings {
  primary?: string;
  fallbacks?: string[];
  autoFallback?: boolean;
}

export interface ToolsConfig {
  categories: ToolCategoryCatalog[];
  toolProviders: Record<string, ToolProviderSettings>;
  tools: Record<string, ToolCategorySettings>;
}

export async function getTools(): Promise<ToolsConfig> {
  return dedupGet("/api/tools", async () => {
    const res = await apiFetch("/api/tools");
    return res.json();
  });
}

export async function saveTools(payload: {
  toolProviders: Record<string, ToolProviderSettings>;
  tools: Record<string, ToolCategorySettings>;
}): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch("/api/tools", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
  return res.json();
}

// probeToolProvider runs a minimal real call against a single provider with
// the form's inline credentials (not the saved config) so the operator can
// verify apiKey/endpoint/model before saving. Mirrors the Models page's
// testProvider flow.
export async function probeToolProvider(req: {
  category: string;
  provider: string;
  apiKey?: string;
  endpoint?: string;
  model?: string;
  options?: Record<string, string>;
}): Promise<{ ok: boolean; error?: string; message?: string }> {
  const res = await apiFetch("/api/tools/probe", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

// Plugins
export async function getPlugins(): Promise<PluginInfo[]> {
  const res = await apiFetch("/api/plugins");
  return res.json();
}

export async function updatePlugin(id: string, data: Partial<PluginInfo>) {
  const res = await apiFetch(`/api/plugins/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(data),
  });
  return res.json();
}

// Channels
export async function getChannels(): Promise<ChannelInfo[]> {
  const res = await apiFetch("/api/channels");
  return res.json();
}

// Cron Jobs
export async function getCronJobs(): Promise<CronJobInfo[]> {
  const res = await apiFetch("/api/cron");
  return res.json();
}

export async function createCronJob(job: Partial<CronJobInfo>) {
  const res = await apiFetch("/api/cron", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(job),
  });
  return res.json();
}

export async function updateCronJob(id: string, job: Partial<CronJobInfo>) {
  const res = await apiFetch(`/api/cron/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(job),
  });
  return res.json();
}

export async function deleteCronJob(id: string) {
  const res = await apiFetch(`/api/cron/${id}`, {
    method: "DELETE",
  });
  return res.json();
}

// --- Admin API: API keys ---

// APIKey is one entry returned by GET /v1/admin/apikeys. The `key` field is
// masked by the server for everyone except the create/rotate response, which
// returns the freshly-issued plaintext key under a separate `key` field.
export interface APIKey {
  id: string;
  name: string;
  key: string; // masked for list responses (e.g. "fc_abcd****wxyz")
  createdAt: string;
}

// Helper: pull a server-supplied {error} message out of a non-OK response so
// callers can surface the real reason (auth failure, duplicate id, etc.)
// instead of crashing on `.apikey` being undefined.
async function readError(res: Response, fallback: string): Promise<string> {
  try {
    const body = await res.json();
    if (body && typeof body.error === "string") return body.error;
  } catch {}
  return `${fallback} (HTTP ${res.status})`;
}

export async function listAPIKeys(): Promise<APIKey[]> {
  const res = await apiFetch("/v1/admin/apikeys");
  if (!res.ok) return [];
  const data = await res.json();
  return data.apikeys || [];
}

export async function createAPIKey(id: string, name: string): Promise<{ apikey: APIKey; key: string }> {
  const res = await apiFetch("/v1/admin/apikeys", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ id, name }),
  });
  if (!res.ok) throw new Error(await readError(res, "create API key failed"));
  const data = await res.json();
  if (!data.apikey || !data.key) throw new Error("malformed response from server");
  return data;
}

export async function deleteAPIKey(id: string): Promise<void> {
  const res = await apiFetch(`/v1/admin/apikeys/${id}`, { method: "DELETE" });
  if (!res.ok) throw new Error(await readError(res, "delete API key failed"));
}

export async function rotateAPIKey(id: string): Promise<string> {
  const res = await apiFetch(`/v1/admin/apikeys/${id}/rotate`, { method: "POST" });
  if (!res.ok) throw new Error(await readError(res, "rotate API key failed"));
  const data = await res.json();
  if (!data.key) throw new Error("malformed response from server");
  return data.key;
}

// --- Admin API: agent ↔ apikey bindings ---

// Map of agent id → apikey id. Empty value means agent is admin-only.
export type AgentBindings = Record<string, string>;

export async function listAgentBindings(): Promise<AgentBindings> {
  const res = await apiFetch("/api/agent-bindings");
  if (!res.ok) return {};
  const data = await res.json();
  return data.bindings || {};
}

// Pass apiKeyId="" to unbind (agent returns to admin-only access).
export async function bindAgent(agentId: string, apiKeyId: string): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/binding`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ apiKeyId }),
  });
  return res.json();
}

// --- Per-agent IM channels (Telegram, ...) ---

export interface AgentChannel {
  type: string;        // "telegram"
  accountId: string;   // bot username for Telegram
  botUsername?: string;
  botToken: string;    // server-masked
  enabled: boolean;
  sharedIdentity: boolean;
  useMarkdown?: boolean; // QQ: msg_type 2 (markdown) vs 0 (plain text)
  failureType?: string; // set when adapter gave up reconnecting; UI shows reconnect prompt
  updatedAt?: string;
}

// AgentCronJob mirrors store.CronJobRecord. Returned by GET
// /api/agents/{id}/cron — covers both jobs the agent scheduled itself
// via create_cron_job AND any seeded by other paths (config, future
// admin UI). lastRun / nextRun are RFC3339 strings or absent.
export interface AgentCronJob {
  id: string;
  agentId: string;
  name: string;
  type: string;        // "cron" | "interval" | "once"
  schedule: string;
  message: string;
  channel: string;
  chatId: string;
  accountId?: string;
  timezone: string;
  enabled: boolean;
  lastRun?: string;
  nextRun?: string;
  createdAt: string;
  // silent: background task — fires on an internal channel, no IM delivery.
  silent?: boolean;
}

// --- Regex Hooks ---

export interface RegexHook {
  id: string;
  agentId: string;
  name: string;
  pattern: string;
  cliCommand: string;
  sortOrder: number;
  continueOnMatch: boolean;
  enabled: boolean;
  showError: boolean;
  errorMessage: string;
  feedToLLM: boolean;
  createdAt: string;
  updatedAt: string;
}

export async function listRegexHooks(agentId: string): Promise<RegexHook[]> {
  const res = await apiFetch(`/api/agents/${agentId}/regex-hooks`);
  if (!res.ok) return [];
  const data = await res.json();
  return data.hooks || [];
}

export async function getRegexHook(
  agentId: string,
  hookId: string,
): Promise<RegexHook | null> {
  const res = await apiFetch(
    `/api/agents/${agentId}/regex-hooks/${encodeURIComponent(hookId)}`,
  );
  if (!res.ok) return null;
  return res.json();
}

export async function saveRegexHook(
  agentId: string,
  hook: Partial<RegexHook> & { name: string; pattern: string; cliCommand: string },
): Promise<{ ok: boolean; hook?: RegexHook; error?: string }> {
  const url = hook.id
    ? `/api/agents/${agentId}/regex-hooks/${encodeURIComponent(hook.id)}`
    : `/api/agents/${agentId}/regex-hooks`;
  const res = await apiFetch(url, {
    method: hook.id ? "PUT" : "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(hook),
  });
  return res.json();
}

export async function deleteRegexHook(
  agentId: string,
  hookId: string,
): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch(
    `/api/agents/${agentId}/regex-hooks/${encodeURIComponent(hookId)}`,
    { method: "DELETE" },
  );
  return res.json();
}

export async function reorderRegexHooks(
  agentId: string,
  hookIds: string[],
): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch(
    `/api/agents/${agentId}/regex-hooks/reorder`,
    {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ hookIds }),
    },
  );
  return res.json();
}

// --- Hook Scripts ---

export interface HookScript {
  name: string;
  size: number;
  modTime: string;
}

export async function listHookScripts(agentId: string): Promise<HookScript[]> {
  const res = await apiFetch(`/api/agents/${agentId}/regex-hooks/scripts`);
  if (!res.ok) return [];
  const data = await res.json();
  return data.scripts || [];
}

export async function uploadHookScript(
  agentId: string,
  file: File,
): Promise<{ ok: boolean; files?: { name: string; size: number }[]; error?: string }> {
  const form = new FormData();
  form.append("file", file);
  const res = await apiFetch(`/api/agents/${agentId}/regex-hooks/scripts`, {
    method: "POST",
    body: form,
  });
  return res.json();
}

export async function deleteHookScript(
  agentId: string,
  name: string,
): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch(
    `/api/agents/${agentId}/regex-hooks/scripts/${encodeURIComponent(name)}`,
    { method: "DELETE" },
  );
  return res.json();
}

export async function listAgentCronJobs(agentId: string): Promise<AgentCronJob[]> {
  const res = await apiFetch(`/api/agents/${agentId}/cron`);
  if (!res.ok) return [];
  const data = await res.json();
  return data.jobs || [];
}

export async function deleteAgentCronJob(
  agentId: string,
  jobId: string,
): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch(
    `/api/agents/${agentId}/cron/${encodeURIComponent(jobId)}`,
    { method: "DELETE" },
  );
  return res.json();
}

export async function toggleAgentCronJob(
  agentId: string,
  jobId: string,
  enabled: boolean,
): Promise<{ ok: boolean; job?: AgentCronJob; error?: string }> {
  const res = await apiFetch(
    `/api/agents/${agentId}/cron/${encodeURIComponent(jobId)}`,
    {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ enabled }),
    },
  );
  return res.json();
}

export async function setAgentCronJobSilent(
  agentId: string,
  jobId: string,
  silent: boolean,
): Promise<{ ok: boolean; job?: AgentCronJob; error?: string }> {
  const res = await apiFetch(
    `/api/agents/${agentId}/cron/${encodeURIComponent(jobId)}`,
    {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ silent }),
    },
  );
  return res.json();
}

export async function listAgentChannels(agentId: string): Promise<AgentChannel[]> {
  const res = await apiFetch(`/api/agents/${agentId}/channels`);
  if (!res.ok) return [];
  const data = await res.json();
  return data.channels || [];
}

export async function connectAgentTelegram(
  agentId: string,
  botToken: string,
): Promise<{ ok: boolean; botUsername?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/channels/telegram`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ botToken }),
  });
  return res.json();
}

export async function connectAgentDiscord(
  agentId: string,
  botToken: string,
): Promise<{ ok: boolean; botUsername?: string; botUserId?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/channels/discord`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ botToken }),
  });
  return res.json();
}

export async function connectAgentSlack(
  agentId: string,
  botToken: string,
  appToken: string,
): Promise<{ ok: boolean; teamName?: string; teamId?: string; botUserId?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/channels/slack`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ botToken, appToken }),
  });
  return res.json();
}

export async function startAgentWeChatLogin(
  agentId: string,
): Promise<{ sessionId?: string; qrCode?: string; qrCodeImg?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/channels/wechat/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({}),
  });
  return res.json();
}

export async function pollAgentWeChatLoginStatus(
  agentId: string,
  sessionId: string,
): Promise<{
  status?: "wait" | "scaned" | "confirmed" | "expired";
  connected?: boolean;
  accountId?: string;
  error?: string;
}> {
  const res = await apiFetch(
    `/api/agents/${agentId}/channels/wechat/login/status?session=${encodeURIComponent(sessionId)}`,
  );
  return res.json();
}

export async function connectAgentLINE(
  agentId: string,
  channelToken: string,
  channelSecret: string,
): Promise<{
  ok: boolean;
  botUserId?: string;
  botName?: string;
  basicId?: string;
  webhookUrl?: string;
  error?: string;
}> {
  const res = await apiFetch(`/api/agents/${agentId}/channels/line`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ channelToken, channelSecret }),
  });
  return res.json();
}

export async function connectAgentFeishu(
  agentId: string,
  appId: string,
  appSecret: string,
  verificationToken: string,
  encryptKey: string,
  useLongConn: boolean,
): Promise<{
  ok: boolean;
  appId?: string;
  botName?: string;
  botOpenId?: string;
  webhookUrl?: string;
  useLongConn?: boolean;
  error?: string;
}> {
  const res = await apiFetch(`/api/agents/${agentId}/channels/feishu`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      appId,
      appSecret,
      verificationToken,
      encryptKey,
      useLongConn,
    }),
  });
  return res.json();
}

export async function connectAgentQQ(
  agentId: string,
  appId: string,
  clientSecret: string,
  useMarkdown: boolean,
): Promise<{
  ok: boolean;
  appId?: string;
  useMarkdown?: boolean;
  error?: string;
}> {
  const res = await apiFetch(`/api/agents/${agentId}/channels/qq`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      appId,
      clientSecret,
      useMarkdown,
    }),
  });
  return res.json();
}

export async function disconnectAgentChannel(
  agentId: string,
  type: string,
  accountId: string,
): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch(
    `/api/agents/${agentId}/channels/${encodeURIComponent(type)}/${encodeURIComponent(accountId)}`,
    { method: "DELETE" },
  );
  return res.json();
}

// retryAgentChannel clears a failed channel account's FailureType and
// hot-restarts its adapter. Bound to the UI "Retry" button shown when
// failureType is set.
export async function retryAgentChannel(
  agentId: string,
  type: string,
  accountId: string,
): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch(
    `/api/agents/${agentId}/channels/${encodeURIComponent(type)}/${encodeURIComponent(accountId)}/retry`,
    { method: "POST" },
  );
  return res.json();
}

export async function updateAgentChannel(
  agentId: string,
  type: string,
  accountId: string,
  patch: { sharedIdentity?: boolean; useMarkdown?: boolean },
): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch(
    `/api/agents/${agentId}/channels/${encodeURIComponent(type)}/${encodeURIComponent(accountId)}`,
    { method: "PATCH", body: JSON.stringify(patch) },
  );
  return res.json();
}

// ---------- Admin: token usage ----------

export type TokenUsageRange = "24h" | "7d" | "30d";

export interface TokenUsageTotals {
  inputTokens: number;
  outputTokens: number;
  cacheReadTokens: number;
  cacheCreationTokens: number;
  requestCount: number;
}

export interface TokenUsageRank {
  key: string;
  tokens: number;
  inputTokens: number;
  outputTokens: number;
  requestCount: number;
}

export interface TokenUsageReport {
  range: TokenUsageRange;
  totals: TokenUsageTotals;
  topAgents: TokenUsageRank[];
  topUsers: TokenUsageRank[];
}

export async function adminGetTokenUsage(
  range: TokenUsageRange = "7d",
  limit = 10,
): Promise<TokenUsageReport> {
  const res = await apiFetch(`/api/usage?range=${range}&limit=${limit}`);
  return res.json();
}

export interface AgentTokenUsage {
  range: TokenUsageRange;
  agentId: string;
  sessions: TokenUsageRank[];
}

// Per-agent session-level usage, exposed in the agent settings dialog.
// Owner-gated server-side; chat viewers of a public agent get a 403.
export async function getAgentTokenUsage(
  agentId: string,
  range: TokenUsageRange = "7d",
  limit = 50,
): Promise<AgentTokenUsage> {
  const res = await apiFetch(
    `/api/agents/${agentId}/usage?range=${range}&limit=${limit}`,
  );
  return res.json();
}

// Build a same-origin URL to a workspace file. Deliberately carries NO bearer
// token: the web UI is same-origin and the auth middleware reads the session
// cookie, so <img src>, <a href>, and direct downloads authenticate by cookie
// like every other API call. (Putting `?token=<bearer>` in a URL leaked a full
// API credential via Referer, browser history, and reverse-proxy access logs.)
export function fileUrl(agentId: string, path: string, download = false): string {
  const encoded = path.split("/").map(encodeURIComponent).join("/");
  const params = new URLSearchParams();
  if (download) params.set("download", "1");
  const qs = params.toString();
  return `/api/agents/${agentId}/files/${encoded}${qs ? "?" + qs : ""}`;
}

// --- IM admin-identity claim. getAgentConfig (above) returns AgentFileConfig
// incl. admins; the helpers below drive the claim/unbind/rebind endpoints. ---
export async function createAgentIMClaim(
  agentId: string,
  channel: string,
): Promise<{ ok?: boolean; code?: string; channel?: string; expiresAt?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/im-claim`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ channel }),
  });
  return res.json();
}

export async function getAgentIMClaim(
  agentId: string,
  channel: string,
): Promise<{ active?: boolean; code?: string; expiresAt?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/im-claim/${channel}`);
  return res.json();
}

export async function unbindAgentIM(
  agentId: string,
  channel: string,
  platformId: string,
): Promise<{ remaining?: number; lastUnbind?: boolean; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/im-unbind`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ channel, platformId }),
  });
  return res.json();
}

export async function rebindAgentIM(
  agentId: string,
  channel: string,
): Promise<{ ok?: boolean; code?: string; channel?: string; expiresAt?: string; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/im-rebind`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ channel }),
  });
  return res.json();
}
