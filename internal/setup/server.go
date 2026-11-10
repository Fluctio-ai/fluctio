package setup

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	// Blank-import tzdata so handleUpdateConfig's prefs.timezone
	// validation (time.LoadLocation) works on zoneinfo-less deployments:
	// Windows binaries away from GOROOT and slim containers would
	// otherwise reject every valid IANA name and the timezone setting
	// would be unusable. Imported here because setup is linked into
	// every server binary.
	_ "time/tzdata"

	"github.com/fluctio-ai/fluctio/internal/agent"
	"github.com/fluctio-ai/fluctio/internal/agent/tools"
	"github.com/fluctio-ai/fluctio/internal/api"
	"github.com/fluctio-ai/fluctio/internal/auth"
	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/channels"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/maintenance"
	"github.com/fluctio-ai/fluctio/internal/pubimg"
	"github.com/fluctio-ai/fluctio/internal/push"
	"github.com/fluctio-ai/fluctio/internal/runtime"
	"github.com/fluctio-ai/fluctio/internal/session"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/taskqueue"
	"github.com/fluctio-ai/fluctio/internal/usage"
	"github.com/fluctio-ai/fluctio/internal/users"
	"github.com/fluctio-ai/fluctio/internal/workflow"
	"github.com/fluctio-ai/fluctio/internal/workspace"
)

// AgentHandle is the surface the web UI uses to talk to a running agent.
type AgentHandle interface {
	Name() string
	HandleWebChat(ctx context.Context, sessionId, projectIdHint, userID, text string, imageURLs []string, params map[string]any) string
	HandleWebChatStream(ctx context.Context, sessionId, projectIdHint, userID, text string, imageURLs []string, params map[string]any, events chan<- agent.ChatEvent) string
	// SteerWeb buffers a message into an in-flight turn for the session;
	// returns false when no turn is running (caller falls back to a
	// normal send).
	SteerWeb(sessionId, projectIDHint, text string) bool
	// RollbackPendingUserWeb drops the trailing unanswered user turn
	// (no assistant reply) from the session — in-memory working set,
	// sessions.messages JSONB, and the session_messages archive row.
	// Used by the "resend a failed message" flow so the LLM doesn't see
	// the failed original + the resend as two back-to-back user turns.
	// No-op (returns false) if the last message isn't a pending user.
	RollbackPendingUserWeb(sessionId, projectIDHint string) bool
	WebChatHistory(sessionId string, beforeSeq, limit int) ([]map[string]any, int, bool)
	WebChatSessions() []session.WebSession
	DeleteWebChatSession(sessionId string) error
	RenameWebChatSession(sessionId, title string) error
	// SetWebChatSessionChatOnly flips the chat's pure-conversation mode
	// (no tools sent to the LLM for turns in that session).
	SetWebChatSessionChatOnly(sessionId string, on bool) error
	// SetWebChatSessionPinned flips the chat's sidebar keep-at-top flag.
	SetWebChatSessionPinned(sessionId string, on bool) error
	// MoveWebChatSession reassigns the chat to a different project (or
	// detaches when projectID==""). Migrates workspace files between
	// the old and new scope dirs and releases any active sandbox so
	// the next turn cold-starts at the new bind-mount path.
	MoveWebChatSession(ctx context.Context, sessionId, projectID string) error
	ForkSession(sourceSessionKey string, forkSeq int) (string, error)
	ReloadWorkspaceFiles()
	// WriteSessionAttachments materializes user-uploaded bytes (data
	// URLs / HTTPS URLs) into the agent's session workspace so skills can
	// read them via /workspace/<filename>. Returns the relative filenames
	// in input order; per-item errors are skipped.
	WriteSessionAttachments(ctx context.Context, sessionID, projectID string, atts []agent.Attachment) []string
	// RegisteredTools returns the live tool registry projection — what
	// this agent currently has loaded (built-ins + MCP + plugin tools).
	// Used by the Tools tab to render the allowlist checkbox picker.
	RegisteredTools() []tools.ToolInfo
	// CompactionPreview returns the per-mode compaction threshold
	// estimates for the context-page UI. Mirrors agent.CompactionPreview
	// so handlers stay on the AgentHandle abstraction.
	CompactionPreview() agent.CompactionPreview
	// RunWorkflow manually triggers one of this agent's own workflows
	// (spec decision 14, manual-trigger path). owner is the calling user;
	// session is "" for a bare manual run. Returns the ExecutionResult, or
	// an error when the agent has no workflows / the id is unknown.
	RunWorkflow(ctx context.Context, id string, input map[string]any, owner, session string) (*workflow.ExecutionResult, error)
	// RunWorkflowStream is RunWorkflow with a progress-event sink for M4
	// node-level streaming (NodeStart / NodeComplete / Done events). A nil
	// sink behaves exactly like RunWorkflow.
	RunWorkflowStream(ctx context.Context, id string, input map[string]any, owner, session string, sink func(workflow.RunEvent)) (*workflow.ExecutionResult, error)
	// ResumeWorkflowStream resumes a waiting run (M6 form interaction) with
	// the user's form answers; formNode is the run's pending_form_node and
	// session the run's original session. A nil sink behaves like a plain
	// resume.
	ResumeWorkflowStream(ctx context.Context, id, runID, formNode string, formValues map[string]any, owner, session string, sink func(workflow.RunEvent)) (*workflow.ExecutionResult, error)
	// WorkflowsDir returns the directory holding this agent's workflow YAMLs.
	WorkflowsDir() string
	// ReloadWorkflows re-reads this agent's workflow dir and rebuilds its
	// Service, so edits take effect without a daemon restart.
	ReloadWorkflows()
}

// AgentProvider is implemented by gateway.UserSpace's agent manager — used
// by handlers that legitimately need to enumerate the *current caller's*
// agents (resolved through the user resolver, not from a global pool).
type AgentProvider interface {
	AllAgents() []AgentHandle
	AgentByID(id string) AgentHandle
	ReloadAgents() error
}

// Server hosts the web UI + admin API. Multi-user is unconditional —
// every request must resolve to a real users.id via the auth.Resolver.
type Server struct {
	port           int
	bind           string
	gatewayCfg     *config.GatewayCfg
	userResolver   api.UserResolver
	taskQueue      *taskqueue.Queue
	apiServer      *api.Server
	authResolver   *auth.Resolver
	accounts       *users.Accounts
	apikeys        *users.APIKeys
	dataStore      store.Store
	workspaceStore workspace.Store
	webChan        *channels.WebChannel
	pushClient     *push.APNSClient
	// chatEvents fans live agent chat events out to subscribed SSE
	// clients across browser tabs. Lazy-init on first use so older
	// callers that didn't wire it explicitly still work.
	chatEvents     *agent.EventHub
	chatEventsOnce sync.Once
	// turnCancels holds the cancel func of every in-flight web-chat turn
	// (handleChatStream / runTeamAgentTurn) so POST /api/chat/stop can
	// deliberately end one. Disconnects no longer kill turns — stop must
	// be an explicit call. Keyed agentID|sessionID; guarded by its own
	// mutex because streams and stop calls race.
	turnCancels   map[string]*turnCancelEntry
	turnCancelsMu sync.Mutex
	// configMu serializes the config read-modify-write handlers (PUT
	// /api/config, /api/tools, /api/plugins/{id}). They load the whole
	// namespaced config, patch it in memory, and sweep every namespace
	// back — without the lock two concurrent writes interleave as
	// last-writer-wins and silently drop each other's namespaces.
	configMu sync.Mutex
	// loginFails backs the /api/login brute-force limiter (see
	// login_limit.go). Lazily created; guarded by loginMu.
	loginMu    sync.Mutex
	loginFails map[string][]time.Time
	usage      usage.Meter
	startedAt  time.Time
	// runtimeMgr powers the coding-agent project runtime (live dev server
	// + preview). Optional: nil when the deployment hasn't wired a
	// sandbox-backed runtime, in which case the /runtime endpoints return
	// 503 instead of nil-panicking. Set via SetRuntimeManager at boot.
	runtimeMgr *runtime.Manager
	// maintenance coordinates online SQLite maintenance (idle-window
	// wait → backup → VACUUM). Nil (unwired) keeps the /api/maintenance
	// endpoints at 503. Set via SetMaintenance at boot.
	maintenance *maintenance.Coordinator
}

// SetMaintenance wires the DB maintenance coordinator. Call once at
// boot; leaving it unset disables the maintenance endpoints (they 503).
func (s *Server) SetMaintenance(m *maintenance.Coordinator) { s.maintenance = m }

// SetRuntimeManager wires the project runtime manager. Call once at boot
// after constructing the Server; leaving it unset disables the coding-
// agent preview endpoints (they 503).
func (s *Server) SetRuntimeManager(m *runtime.Manager) { s.runtimeMgr = m }

// NewServer creates a setup wizard server on the given port.
func NewServer(port int) *Server {
	return &Server{port: port, bind: "loopback", startedAt: time.Now()}
}

// SetGatewayConfig sets the gateway configuration for bind address and HTTP endpoints.
func (s *Server) SetGatewayConfig(cfg *config.GatewayCfg) {
	s.gatewayCfg = cfg
	if cfg.Bind != "" {
		s.bind = cfg.Bind
	}
	if cfg.Port > 0 {
		s.port = cfg.Port
	}
}

// SetTaskQueue sets the task queue for the tasks API endpoint.
func (s *Server) SetTaskQueue(tq *taskqueue.Queue) {
	s.taskQueue = tq
}

// SetAPIServer sets the OpenAI-compatible API server for /v1/* and /ws routes.
func (s *Server) SetAPIServer(apiSrv *api.Server) {
	s.apiServer = apiSrv
	// Billing endpoints (usage/quota) verify that an explicit user_id
	// names an app_user owned by the caller's key.
	if s.dataStore != nil {
		apiSrv.SetUserLookup(func(ctx context.Context, userID string) (*store.UserRecord, bool) {
			rec, err := s.dataStore.GetUser(ctx, userID)
			return rec, err == nil && rec != nil
		})
	}
}

// SetUserResolver sets the per-user agent routing resolver.
func (s *Server) SetUserResolver(resolver api.UserResolver) {
	s.userResolver = resolver
}

// SetStore sets the storage backend.
func (s *Server) SetStore(st store.Store) {
	s.dataStore = st
	if st != nil {
		s.accounts, _ = users.NewAccounts(st)
		s.apikeys, _ = users.NewAPIKeys(st)
	}
	if s.pushClient == nil {
		s.pushClient = push.NewAPNSClientFromEnv()
	}
}

// SetWorkspaceStore installs the blob store used for agent-generated artifacts.
func (s *Server) SetWorkspaceStore(ws workspace.Store) {
	s.workspaceStore = ws
}

// SetUsageMeter installs the per-tenant resource counter.
func (s *Server) SetUsageMeter(m usage.Meter) {
	s.usage = m
}

// SetAuth installs the auth resolver. Required.
func (s *Server) SetAuth(resolver *auth.Resolver) {
	s.authResolver = resolver
}

// SetWebChannel installs the in-process fan-out used by the SSE
// subscription endpoint. When set, /api/chat/subscribe holds an SSE
// stream open per (agent, session) pair and forwards every outbound
// message routed to channel="web" — this is what surfaces cron-fired
// agent replies live in the dashboard chat panel.
func (s *Server) SetWebChannel(wc *channels.WebChannel) {
	s.webChan = wc
	if wc != nil {
		wc.SetPushHandler(func(msg bus.OutboundMessage) {
			go s.handleWebPushOutbound(msg)
		})
	}
}

// chatEventHub returns the lazy-initialized hub. Centralized so every
// chat handler reaches the same instance — without this, the streaming
// handler's hub publish would never reach the subscribe handler.
// sync.Once guards the init: two concurrent first-callers (stream POST
// vs subscribe GET) would otherwise each build a hub, and the loser's
// subscribers would silently attach to a hub nothing publishes to.
func (s *Server) chatEventHub() *agent.EventHub {
	s.chatEventsOnce.Do(func() {
		s.chatEvents = agent.NewEventHub()
	})
	return s.chatEvents
}

// ChatEventHub exposes the hub so the gateway can attach a stream
// pipeline onto bus-fired web turns (cron / goal continuation /
// heartbeat / sub-agent), giving them the same SSE streaming a
// user-typed turn gets. Wraps chatEventHub's lazy-init.
func (s *Server) ChatEventHub() *agent.EventHub { return s.chatEventHub() }

// authMiddleware wraps the auth.Resolver's Middleware. Required for every
// authenticated route.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	if s.authResolver == nil {
		return func(w http.ResponseWriter, r *http.Request) {
			jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "auth not configured"})
		}
	}
	return s.authResolver.Middleware(next)
}

// optionalAuth is the bootstrap-friendly variant for endpoints reachable
// before login (status, login, onboard).
func (s *Server) optionalAuth(next http.HandlerFunc) http.HandlerFunc {
	if s.authResolver == nil {
		return next
	}
	return s.authResolver.Optional(next)
}

// requireSuperAdmin gates handlers behind platform-admin authority:
// either a super_admin session or a type=admin apikey. Despite the name,
// this accepts both — the apikey path is the only way a programmatic
// admin client can hit /api/admin/* without a browser cookie. Use
// auth.RequireSuperAdmin directly for the rare cases that need the
// session-only flavor.
func (s *Server) requireSuperAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.authMiddleware(auth.RequirePlatformAdmin(next))
}

// agentACL enforces the caller's per-agent ACL (auth.Identity.
// CanAccessAgent) for /api/agents/{id}/* routes whose handlers don't
// individually check it (kb / wiki / diary / cards). Session users and
// unrestricted apikeys always pass — single-user flatten semantics are
// unchanged; only an apikey scoped by AllowedAgents is restricted, and
// it gets a 404 so the agent's existence isn't disclosed.
func (s *Server) agentACL(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ident, ok := authIdentity(r); ok && !ident.CanAccessAgent(r.PathValue("id")) {
			jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		next(w, r)
	}
}

// Run starts the HTTP server and blocks until the context is canceled.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()

	// Health probes (unauthenticated).
	healthz := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /livez", healthz)
	mux.HandleFunc("GET /readyz", healthz)

	// Short-lived anonymous image URLs for vision endpoints that only
	// accept public http(s) URLs (Agnes etc.). Mounted only when
	// FLUCTIO_PUBLIC_BASE_URL is configured; tokens are random and expire.
	pubimg.Mount(mux)

	auth := s.authMiddleware
	opt := s.optionalAuth
	admin := s.requireSuperAdmin
	// agentAuth = auth + per-agent ACL for the kb/wiki/diary/cards config
	// surfaces (their handlers read/write per-agent rows without an
	// individual ACL check).
	agentAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return auth(s.agentACL(next))
	}

	// Bootstrap / login.
	mux.HandleFunc("GET /api/status", opt(s.handleStatus))
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", auth(s.handleLogout))
	mux.HandleFunc("GET /api/me", auth(s.handleMe))
	mux.HandleFunc("PUT /api/me", auth(s.handleUpdateMe))
	mux.HandleFunc("GET /api/me/avatar", auth(s.handleMyAvatar))
	mux.HandleFunc("POST /api/me/avatar", auth(s.handleUploadMyAvatar))
	mux.HandleFunc("POST /api/me/password", auth(s.handleChangeMyPassword))
	mux.HandleFunc("POST /api/push/devices", auth(s.handleSavePushDevice))
	mux.HandleFunc("DELETE /api/push/devices/{token}", auth(s.handleDeletePushDevice))
	// Notification queue (mobile direct delivery, no FCM): polling,
	// cursor ack, and the foreground SSE stream.
	mux.HandleFunc("GET /api/notifications", auth(s.handleNotificationsList))
	mux.HandleFunc("POST /api/notifications/ack", auth(s.handleNotificationsAck))
	mux.HandleFunc("GET /api/notifications/stream", auth(s.handleNotificationsStream))
	mux.HandleFunc("POST /api/test-provider", opt(s.handleTestProvider))
	mux.HandleFunc("POST /api/onboard", s.handleOnboard)
	mux.HandleFunc("GET /api/admin/chats", admin(s.handleAdminChats))

	// Per-user config (system_settings + scoped providers/channels).
	mux.HandleFunc("GET /api/config", auth(s.handleGetConfig))
	mux.HandleFunc("POST /api/config", auth(s.handleUpdateConfig))

	// Chat
	mux.HandleFunc("POST /api/chat", auth(s.handleChat))
	mux.HandleFunc("GET /api/agents/{agentID}/workflows", auth(s.handleWorkflowList))
	mux.HandleFunc("GET /api/agents/{agentID}/workflows/{wfID}", auth(s.handleWorkflowGet))
	mux.HandleFunc("PUT /api/agents/{agentID}/workflows/{wfID}", auth(s.handleWorkflowPut))
	mux.HandleFunc("DELETE /api/agents/{agentID}/workflows/{wfID}", auth(s.handleWorkflowDelete))
	mux.HandleFunc("POST /api/agents/{agentID}/workflows/{wfID}/run", auth(s.handleWorkflowRun))
	mux.HandleFunc("POST /api/agents/{agentID}/workflows/{wfID}/run/stream", auth(s.handleWorkflowRunStream))
	mux.HandleFunc("POST /api/agents/{agentID}/workflows/{wfID}/runs/{runID}/resume", auth(s.handleWorkflowRunResume))
	mux.HandleFunc("POST /api/agents/{agentID}/workflows/{wfID}/runs/{runID}/resume/stream", auth(s.handleWorkflowRunResumeStream))
	mux.HandleFunc("GET /api/agents/{agentID}/workflows/{wfID}/runs", auth(s.handleWorkflowRunsList))
	mux.HandleFunc("GET /api/agents/{agentID}/workflows/{wfID}/runs/{runID}", auth(s.handleWorkflowRunGet))
	mux.HandleFunc("DELETE /api/agents/{agentID}/workflows/{wfID}/runs/{runID}", auth(s.handleWorkflowRunDelete))
	mux.HandleFunc("DELETE /api/agents/{agentID}/workflows/{wfID}/runs", auth(s.handleWorkflowRunsBatchDelete))
	mux.HandleFunc("GET /api/agents/{agentID}/workflows/{wfID}/schedules", auth(s.handleWorkflowScheduleList))
	mux.HandleFunc("POST /api/agents/{agentID}/workflows/{wfID}/schedules", auth(s.handleWorkflowScheduleCreate))
	mux.HandleFunc("PATCH /api/agents/{agentID}/workflows/{wfID}/schedules/{schedID}", auth(s.handleWorkflowScheduleToggle))
	mux.HandleFunc("DELETE /api/agents/{agentID}/workflows/{wfID}/schedules/{schedID}", auth(s.handleWorkflowScheduleDelete))
	mux.HandleFunc("POST /api/chat/stream", auth(s.handleChatStream))
	mux.HandleFunc("POST /api/chat/team/stream", auth(s.handleTeamChatStream))
	mux.HandleFunc("POST /api/chat/steer", auth(s.handleChatSteer))
	mux.HandleFunc("POST /api/chat/stop", auth(s.handleChatStop))
	mux.HandleFunc("POST /api/chat/recall-feedback", auth(s.handleRecallFeedback))
	mux.HandleFunc("GET /api/chats", auth(s.handleChats))
	mux.HandleFunc("GET /api/chat/history", auth(s.handleChatHistory))
	mux.HandleFunc("GET /api/chat/todo", auth(s.handleChatTodo))
	mux.HandleFunc("GET /api/chat/sessions", auth(s.handleChatSessions))
	mux.HandleFunc("PUT /api/chat/sessions/{key}", auth(s.handleRenameSession))
	mux.HandleFunc("DELETE /api/chat/sessions/{key}", auth(s.handleDeleteSession))
	mux.HandleFunc("PATCH /api/chat/sessions/{key}/project", auth(s.handleMoveSessionProject))
	mux.HandleFunc("PATCH /api/chat/sessions/{key}/chat-only", auth(s.handleSetSessionChatOnly))
	mux.HandleFunc("PATCH /api/chat/sessions/{key}/pin", auth(s.handleSetSessionPinned))
	mux.HandleFunc("POST /api/chat/sessions/fork", auth(s.handleForkSession))
	// Raw session_events dump (debugging: "record exists but UI didn't
	// render it" vs "record gone"). Reads the same store the chat page
	// does, minus the history projection.
	mux.HandleFunc("GET /api/chat/sessions/{key}/export", auth(s.handleSessionExport))
	// Long-lived SSE subscription so cron-fired (and other async)
	// messages reach the open chat panel without a manual refresh.
	mux.HandleFunc("GET /api/chat/subscribe", auth(s.handleChatSubscribe))

	// Agents
	mux.HandleFunc("GET /api/agents", auth(s.handleListAgents))
	mux.HandleFunc("POST /api/agents", auth(s.handleCreateAgent))
	mux.HandleFunc("GET /api/agents/{id}", auth(s.handleGetAgent))
	mux.HandleFunc("PUT /api/agents/{id}", auth(s.handleUpdateAgent))
	mux.HandleFunc("GET /api/agents/{id}/config", auth(s.handleGetAgentConfig))
	mux.HandleFunc("GET /api/agents/{id}/tools/registered", auth(s.handleListAgentRegisteredTools))
	mux.HandleFunc("GET /api/agents/{id}/recall-tuning", auth(s.handleGetRecallTuning))
	mux.HandleFunc("PUT /api/agents/{id}/recall-tuning", auth(s.handlePutRecallTuning))
	mux.HandleFunc("GET /api/agents/{id}/recall-events", auth(s.handleListRecallEvents))
	mux.HandleFunc("POST /api/agents/{id}/recall-test", auth(s.handleRecallTest))
	mux.HandleFunc("DELETE /api/agents/{id}", auth(s.handleDeleteAgent))

	// IM claim: owner mints a verification code so an IM user can bind as admin via /claim <code>
	mux.HandleFunc("POST /api/agents/{id}/im-claim", auth(s.handleCreateIMClaim))
	mux.HandleFunc("GET /api/agents/{id}/im-claim/{channel}", auth(s.handleGetIMClaim))
	mux.HandleFunc("POST /api/agents/{id}/im-unbind", auth(s.handleUnbindIM))
	mux.HandleFunc("POST /api/agents/{id}/im-rebind", auth(s.handleRebindIM))

	mux.HandleFunc("GET /api/agents/{id}/files", auth(s.handleAgentFileList))
	mux.HandleFunc("GET /api/agents/{id}/files.zip", auth(s.handleAgentFilesZip))
	mux.HandleFunc("GET /api/agents/{id}/files/{path...}", auth(s.handleAgentFile))
	mux.HandleFunc("DELETE /api/agents/{id}/files/{path...}", auth(s.handleAgentFileDelete))
	mux.HandleFunc("POST /api/agents/{id}/files", auth(s.handleAgentFileUpload))
	// Self-hosted-only: opens the workspace dir in the operator's
	// native file browser (Finder/Explorer/xdg-open). Hosted
	// deployments 403 inside the handler — chatters there don't
	// own the daemon's filesystem.
	mux.HandleFunc("POST /api/agents/{id}/workspace/reveal", auth(s.handleAgentWorkspaceReveal))

	mux.HandleFunc("GET /api/agents/{id}/system-files/{name}", auth(s.handleGetAgentSystemFile))
	mux.HandleFunc("PUT /api/agents/{id}/system-files/{name}", auth(s.handlePutAgentSystemFile))
	mux.HandleFunc("DELETE /api/agents/{id}/system-files/{name}", auth(s.handleDeleteAgentSystemFile))

	// Per-agent projects: named workspace folders that group chats and
	// share files across all sessions inside them. POST .../sessions
	// is the "New chat in project" path — pre-creates the session row
	// stamped with project_id so the very first turn already routes
	// workspace IO to projects/<pid>/.
	mux.HandleFunc("GET /api/agents/{id}/projects", auth(s.handleListProjects))
	mux.HandleFunc("POST /api/agents/{id}/projects", auth(s.handleCreateProject))
	mux.HandleFunc("PATCH /api/agents/{id}/projects/{pid}", auth(s.handleUpdateProject))
	mux.HandleFunc("DELETE /api/agents/{id}/projects/{pid}", auth(s.handleDeleteProject))

	// Project runtime: the coding-agent "live app" layer on top of a
	// project — a long-lived dev-server sandbox + preview URL. The
	// upstream SaaS shell drives a project entirely through these.
	mux.HandleFunc("GET /api/agents/{id}/projects/{pid}/runtime", auth(s.handleGetRuntime))
	mux.HandleFunc("POST /api/agents/{id}/projects/{pid}/runtime/up", auth(s.handleRuntimeUp))
	mux.HandleFunc("POST /api/agents/{id}/projects/{pid}/runtime/sleep", auth(s.handleRuntimeSleep))
	mux.HandleFunc("POST /api/agents/{id}/projects/{pid}/runtime/wake", auth(s.handleRuntimeWake))
	mux.HandleFunc("DELETE /api/agents/{id}/projects/{pid}/runtime", auth(s.handleRuntimeStop))
	mux.HandleFunc("GET /api/agents/{id}/projects/{pid}/preview", auth(s.handleRuntimePreview))
	// Scope-flexible preview lookup (sessionId or projectId query param) —
	// lets the chat workspace panel surface an "open preview" entry for the
	// current chat, including loose chats that have no project.
	mux.HandleFunc("GET /api/agents/{id}/preview", auth(s.handleScopePreview))
	// Scope-flexible build/dev log tail — the preview panel polls this while
	// the app scaffolds so the user sees the live pnpm-install output.
	mux.HandleFunc("GET /api/agents/{id}/preview/logs", auth(s.handleScopePreviewLogs))
	// Files the agent changed vs the template baseline (git diff in the
	// running app) — lets the workspace tree show only this task's output.
	mux.HandleFunc("GET /api/agents/{id}/changed-files", auth(s.handleChangedFiles))
	mux.HandleFunc("GET /api/agents/{id}/projects/{pid}/runtime/logs", auth(s.handleRuntimeLogs))

	// Per-agent channels (IM bot bindings)
	mux.HandleFunc("GET /api/agents/{id}/channels", auth(s.handleListAgentChannels))
	mux.HandleFunc("POST /api/agents/{id}/channels/telegram", auth(s.handleConnectAgentTelegram))
	mux.HandleFunc("POST /api/agents/{id}/channels/discord", auth(s.handleConnectAgentDiscord))
	mux.HandleFunc("POST /api/agents/{id}/channels/slack", auth(s.handleConnectAgentSlack))
	mux.HandleFunc("POST /api/agents/{id}/channels/wechat/login", auth(s.handleStartAgentWeChatLogin))
	mux.HandleFunc("GET /api/agents/{id}/channels/wechat/login/status", auth(s.handleAgentWeChatLoginStatus))
	mux.HandleFunc("POST /api/agents/{id}/channels/line", auth(s.handleConnectAgentLINE))
	mux.HandleFunc("POST /api/agents/{id}/channels/feishu", auth(s.handleConnectAgentFeishu))
	mux.HandleFunc("POST /api/agents/{id}/channels/qq", auth(s.handleConnectAgentQQ))
	mux.HandleFunc("DELETE /api/agents/{id}/channels/{type}/{accountId}", auth(s.handleDisconnectAgentChannel))
	mux.HandleFunc("POST /api/agents/{id}/channels/{type}/{accountId}/retry", auth(s.handleRetryAgentChannel))
	mux.HandleFunc("PATCH /api/agents/{id}/channels/{type}/{accountId}", auth(s.handleUpdateAgentChannel))

	// Knowledge base: per-agent corpus of chunked text sources used for
	// retrieval (knowledgebase_search tool + wiki generation pipeline).
	mux.HandleFunc("GET /api/agents/{id}/kb/sources", auth(s.handleListKBSources))
	mux.HandleFunc("POST /api/agents/{id}/kb/ingest/text", agentAuth(s.handleKBIngestText))
	mux.HandleFunc("POST /api/agents/{id}/kb/ingest/url", agentAuth(s.handleKBIngestURL))
	mux.HandleFunc("DELETE /api/agents/{id}/kb/sources/{sourceId}", auth(s.handleDeleteKBSource))
	mux.HandleFunc("GET /api/agents/{id}/kb/sources/{sourceId}/entries", auth(s.handleListKBEntries))
	mux.HandleFunc("GET /api/agents/{id}/kb/sources/{sourceId}/insights", agentAuth(s.handleKBGetInsights))
	mux.HandleFunc("POST /api/agents/{id}/kb/sources/{sourceId}/insights/generate", agentAuth(s.handleKBGenerateInsights))
	mux.HandleFunc("GET /api/agents/{id}/kb/stats", auth(s.handleGetKBStats))
	mux.HandleFunc("POST /api/agents/{id}/kb/search", agentAuth(s.handleKBSearch))
	mux.HandleFunc("POST /api/agents/{id}/kb/mcp", agentAuth(s.handleKBMCP))
	mux.HandleFunc("POST /api/agents/{id}/kb/flash", agentAuth(s.handleKBSaveFlash))
	mux.HandleFunc("PATCH /api/agents/{id}/kb/flash/{sourceId}", agentAuth(s.handleKBUpdateFlash))
	mux.HandleFunc("POST /api/agents/{id}/kb/todo", agentAuth(s.handleKBSaveTodo))
	mux.HandleFunc("PATCH /api/agents/{id}/kb/todos/{sourceId}", agentAuth(s.handleKBUpdateTodo))
	mux.HandleFunc("GET /api/agents/{id}/kb/todos", agentAuth(s.handleKBListTodos))
	mux.HandleFunc("GET /api/agents/{id}/kb/bookmarks", agentAuth(s.handleKBListBookmarks))
	mux.HandleFunc("POST /api/agents/{id}/kb/bookmarks", agentAuth(s.handleKBSaveBookmark))
	mux.HandleFunc("DELETE /api/agents/{id}/kb/bookmarks/{bookmarkId}", agentAuth(s.handleKBDeleteBookmark))
	mux.HandleFunc("PATCH /api/agents/{id}/kb/bookmarks/{bookmarkId}", agentAuth(s.handleKBUpdateBookmark))
	mux.HandleFunc("POST /api/agents/{id}/kb/bookmarks/{bookmarkId}/promote", agentAuth(s.handleKBPromoteBookmark))
	mux.HandleFunc("GET /api/agents/{id}/kb/notes", agentAuth(s.handleKBListNotes))
	mux.HandleFunc("POST /api/agents/{id}/kb/notes", agentAuth(s.handleKBSaveNote))
	mux.HandleFunc("DELETE /api/agents/{id}/kb/notes/{noteId}", agentAuth(s.handleKBDeleteNote))
	mux.HandleFunc("GET /api/agents/{id}/kb/notes/{noteId}/attachments", agentAuth(s.handleKBNoteAttachments))
	mux.HandleFunc("POST /api/agents/{id}/kb/notes/{noteId}/attachments", agentAuth(s.handleKBNoteUpload))
	mux.HandleFunc("DELETE /api/agents/{id}/kb/notes/{noteId}/attachments/{attId}", agentAuth(s.handleKBNoteDeleteAttachment))
	mux.HandleFunc("GET /api/agents/{id}/kb/cards", agentAuth(s.handleKBListCards))
	mux.HandleFunc("GET /api/agents/{id}/kb/cards/stats", agentAuth(s.handleKBCardStats))
	mux.HandleFunc("POST /api/agents/{id}/kb/cards", agentAuth(s.handleKBSaveCard))
	mux.HandleFunc("GET /api/agents/{id}/kb/cards/{cardId}", agentAuth(s.handleKBGetCard))
	mux.HandleFunc("PATCH /api/agents/{id}/kb/cards/{cardId}", agentAuth(s.handleKBUpdateCard))
	mux.HandleFunc("DELETE /api/agents/{id}/kb/cards/{cardId}", agentAuth(s.handleKBDeleteCard))
	mux.HandleFunc("POST /api/agents/{id}/kb/cards/{cardId}/review", agentAuth(s.handleKBReviewCard))
	mux.HandleFunc("POST /api/agents/{id}/kb/cards/{cardId}/archive", agentAuth(s.handleKBCardArchive))
	mux.HandleFunc("POST /api/agents/{id}/kb/cards/{cardId}/restore", agentAuth(s.handleKBCardRestore))
	mux.HandleFunc("POST /api/agents/{id}/kb/cards/generate", agentAuth(s.handleKBCardsGenerate))
	mux.HandleFunc("GET /api/agents/{id}/kb/pending", agentAuth(s.handleKBListPending))
	mux.HandleFunc("POST /api/agents/{id}/kb/pending/{pendingId}/resolve", agentAuth(s.handleKBResolvePending))

	// Wiki: LLM-generated structured pages derived from KB sources.
	mux.HandleFunc("GET /api/agents/{id}/wiki/stats", agentAuth(s.handleWikiStats))
	mux.HandleFunc("GET /api/agents/{id}/wiki/pages", agentAuth(s.handleWikiListPages))
	mux.HandleFunc("GET /api/agents/{id}/wiki/pages/{pageId}", agentAuth(s.handleWikiGetPage))
	mux.HandleFunc("GET /api/agents/{id}/wiki/graph", agentAuth(s.handleWikiGraph))
	mux.HandleFunc("DELETE /api/agents/{id}/wiki/pages/{pageId}", agentAuth(s.handleWikiDeletePage))
	mux.HandleFunc("PUT /api/agents/{id}/wiki/pages/{pageId}", agentAuth(s.handleWikiUpdatePage))
	mux.HandleFunc("POST /api/agents/{id}/wiki/generate", agentAuth(s.handleWikiGenerate))
	mux.HandleFunc("POST /api/agents/{id}/wiki/reindex-embed", agentAuth(s.handleWikiReindexEmbed))
	mux.HandleFunc("GET /api/agents/{id}/wiki/progress", agentAuth(s.handleWikiProgress))
	mux.HandleFunc("GET /api/agents/{id}/wiki/autogen-status", agentAuth(s.handleWikiAutogenStatus))

	// Daily diary: per-agent themed daily-diary entries distilled from
	// conversation_summaries, with a "you might have missed" section.
	mux.HandleFunc("GET /api/agents/{id}/diary", agentAuth(s.handleDiaryList))
	mux.HandleFunc("GET /api/agents/{id}/diary/shelved", agentAuth(s.handleDiaryShelved))
	mux.HandleFunc("PATCH /api/agents/{id}/diary/shelved/{cid}", agentAuth(s.handleDiaryShelvedSettle))
	mux.HandleFunc("GET /api/agents/{id}/diary/{date}", agentAuth(s.handleDiaryGet))
	mux.HandleFunc("DELETE /api/agents/{id}/diary/{date}", agentAuth(s.handleDiaryDelete))
	mux.HandleFunc("POST /api/agents/{id}/diary/generate", agentAuth(s.handleDiaryGenerate))

	// Feishu (飞书) event webhook. UNAUTHENTICATED — Feishu posts here
	// without a fluctio bearer token. Per-event security comes from
	// the verification_token validated inside the adapter against the
	// payload's header.token. The {appId} path segment scopes the
	// receive to one registered channel.
	mux.HandleFunc("POST /api/feishu/webhook/{appId}", s.handleFeishuWebhook)

	// LINE Messaging API event webhook. UNAUTHENTICATED at the fluctio
	// layer — per-event security is HMAC-SHA256(channel_secret, body)
	// validated by the adapter against the `x-line-signature` header.
	// The {accountId} path segment is the bot's userId, scoping the
	// receive to one registered channel.
	mux.HandleFunc("POST /api/line/webhook/{accountId}", s.handleLINEWebhook)

	// Skills
	mux.HandleFunc("GET /api/skills", auth(s.handleListSkills))
	mux.HandleFunc("GET /api/skills/{name}/manifest", auth(s.handleGetSkillManifest))
	mux.HandleFunc("GET /api/skills/search", auth(s.handleSearchSkills))
	mux.HandleFunc("POST /api/skills/install", auth(s.handleInstallSkill))
	mux.HandleFunc("POST /api/skills/upload", auth(s.handleUploadSkill))
	mux.HandleFunc("DELETE /api/skills/{name}", admin(s.handleDeleteSkill))
	mux.HandleFunc("GET /api/agents/{id}/skills", auth(s.handleListAgentSkills))
	mux.HandleFunc("GET /api/agents/{id}/skills/{name}/manifest", auth(s.handleGetAgentSkillManifest))
	mux.HandleFunc("DELETE /api/agents/{id}/skills/{name}", auth(s.handleDeleteAgentSkill))
	mux.HandleFunc("POST /api/agents/{id}/skills/reload", auth(s.handleReloadAgentSkills))

	// Cross-platform hot reload for the CLI: agent/provider/channel/cron
	// CRUD writes the store directly and can't deliver SIGHUP on Windows,
	// so the CLI posts here with a temporary admin API key.
	mux.HandleFunc("POST /api/admin/reload", admin(s.handleAdminReload))

	// Plugins (super_admin only).
	mux.HandleFunc("GET /api/plugins", admin(s.handleListPlugins))
	mux.HandleFunc("PUT /api/plugins/{id}", admin(s.handleUpdatePlugin))
	// Hook plugin discovery — read-only metadata for the per-agent
	// Plugins toggle on the Context page. Agent owners (not just
	// admins) need this to know what plugins they can enable.
	mux.HandleFunc("GET /api/plugins/hook", auth(s.handleListHookPlugins))

	// Tools (super_admin only).
	mux.HandleFunc("GET /api/tools", admin(s.handleGetTools))
	mux.HandleFunc("PUT /api/tools", admin(s.handleSaveTools))
	mux.HandleFunc("POST /api/tools/probe", admin(s.handleToolProbe))

	// Channels (read-only list of registered channel adapters at runtime)
	mux.HandleFunc("GET /api/channels", auth(s.handleListChannels))

	// Scoped CRUD: providers + channels at system / user / agent scope.
	mux.HandleFunc("GET /api/providers", auth(s.handleListProviders))
	mux.HandleFunc("POST /api/providers", auth(s.handleCreateProvider))
	mux.HandleFunc("PUT /api/providers/{id}", auth(s.handleUpdateProvider))
	mux.HandleFunc("DELETE /api/providers/{id}", auth(s.handleDeleteProvider))
	mux.HandleFunc("POST /api/providers/{id}/test", auth(s.handleTestStoredProvider))
	// Diagnostic error reports (manual trigger + download). See
	// specs/2026-07-22-web-ui-llm-error-report.md.
	// Diag reports are written to one global directory and mix failure
	// details from every agent/session — that's platform-admin data, so
	// all four routes are gated like the other admin surfaces.
	mux.HandleFunc("POST /api/diag/reports", admin(s.handleDiagReportGenerate))
	mux.HandleFunc("GET /api/diag/reports", admin(s.handleDiagReportList))
	mux.HandleFunc("GET /api/diag/reports/{name}", admin(s.handleDiagReportDownload))
	mux.HandleFunc("DELETE /api/diag/reports/{name}", admin(s.handleDiagReportDelete))
	mux.HandleFunc("GET /api/scoped-channels", auth(s.handleListScopedChannels))
	mux.HandleFunc("POST /api/scoped-channels", auth(s.handleCreateScopedChannel))
	mux.HandleFunc("PUT /api/scoped-channels/{id}", auth(s.handleUpdateScopedChannel))
	mux.HandleFunc("DELETE /api/scoped-channels/{id}", auth(s.handleDeleteScopedChannel))

	// Built-in model contextWindow table (for models page autocomplete).
	mux.HandleFunc("GET /api/models/builtin", auth(s.handleBuiltinModels))

	// Fetch live model list from the agent's bound upstream provider.
	// Each id is enriched with a contextWindow via LookupModelMeta.
	// Upstream unreachable / unsupported → 501.
	mux.HandleFunc("POST /api/agents/{id}/models/fetch", auth(s.handleFetchProviderModels))

	// Fetch live model list from a provider config supplied in the body
	// (global /models page). Stored-key path resolves via providerId.
	mux.HandleFunc("POST /api/models/fetch", auth(s.handleFetchModelsByConfig))

	// Compaction threshold preview: returns contextWindow/maxTokens +
	// per-mode (conservative/balanced/aggressive) threshold estimates so
	// the context-page UI can show what each mode does before applying.
	mux.HandleFunc("GET /api/agents/{id}/compaction/preview", auth(s.handleCompactionPreview))

	// Per-agent cron jobs (DB-backed, includes anything the agent
	// scheduled itself via create_cron_job at runtime). The legacy
	// /api/cron set that read the flat fluctio.json catalog was removed —
	// it wrote to a store neither the scheduler nor the dashboard read.

	// Per-agent cron jobs (DB-backed, includes anything the agent
	// scheduled itself via create_cron_job at runtime).
	mux.HandleFunc("GET /api/agents/{id}/cron", auth(s.handleListAgentCronJobs))
	mux.HandleFunc("DELETE /api/agents/{id}/cron/{jobId}", auth(s.handleDeleteAgentCronJob))
	mux.HandleFunc("PUT /api/agents/{id}/cron/{jobId}", auth(s.handleToggleAgentCronJob))

	// Regex hooks (per-agent message interception)
	mux.HandleFunc("GET /api/agents/{id}/regex-hooks", auth(s.handleListRegexHooks))
	mux.HandleFunc("GET /api/agents/{id}/regex-hooks/{hookId}", auth(s.handleGetRegexHook))
	mux.HandleFunc("POST /api/agents/{id}/regex-hooks", auth(s.handleSaveRegexHook))
	mux.HandleFunc("PUT /api/agents/{id}/regex-hooks/{hookId}", auth(s.handleSaveRegexHook))
	mux.HandleFunc("DELETE /api/agents/{id}/regex-hooks/{hookId}", auth(s.handleDeleteRegexHook))
	mux.HandleFunc("PUT /api/agents/{id}/regex-hooks/reorder", auth(s.handleReorderRegexHooks))
	// Hook script management (upload / list / delete CLI scripts)
	mux.HandleFunc("GET /api/agents/{id}/regex-hooks/scripts", auth(s.handleListHookScripts))
	mux.HandleFunc("POST /api/agents/{id}/regex-hooks/scripts", auth(s.handleUploadHookScript))
	mux.HandleFunc("DELETE /api/agents/{id}/regex-hooks/scripts/{name}", auth(s.handleDeleteHookScript))

	// Memory (conversation-summary recall: embedding/reranker config test
	// + per-agent force re-vectorize). The two test probes take a
	// client-supplied apiBase and have the server fetch it — admin-only so
	// they can't double as an authenticated port-scan of the private
	// network. Self-hosted setups (localhost ollama) still work: the
	// single-user owner is a super_admin.
	mux.HandleFunc("POST /api/memory/test-embedding", admin(s.handleTestEmbedding))
	mux.HandleFunc("POST /api/memory/test-reranker", admin(s.handleTestReranker))
	mux.HandleFunc("GET /api/agents/{id}/memory", auth(s.handleGetAgentMemory))
	mux.HandleFunc("PUT /api/agents/{id}/memory", auth(s.handleUpdateAgentMemory))
	mux.HandleFunc("POST /api/agents/{id}/memory/reindex", auth(s.handleReindexAgentMemory))
	mux.HandleFunc("GET /api/agents/{id}/vectorization", auth(s.handleGetAgentVectorization))
	mux.HandleFunc("PUT /api/agents/{id}/vectorization", auth(s.handleUpdateAgentVectorization))
	mux.HandleFunc("GET /api/vectorization", auth(s.handleGetSystemVectorization))
	mux.HandleFunc("PUT /api/vectorization", auth(s.handleUpdateSystemVectorization))
	mux.HandleFunc("GET /api/agents/{id}/privacy", auth(s.handleGetAgentPrivacy))
	mux.HandleFunc("PUT /api/agents/{id}/privacy", auth(s.handleUpdateAgentPrivacy))
	mux.HandleFunc("GET /api/privacy", auth(s.handleGetSystemPrivacy))
	mux.HandleFunc("PUT /api/privacy", auth(s.handleUpdateSystemPrivacy))
	// Scheduled SQLite backup: config + manual list/now/download/delete.
	mux.HandleFunc("GET /api/backup", auth(s.handleGetSystemBackup))
	mux.HandleFunc("PUT /api/backup", auth(s.handleUpdateSystemBackup))
	mux.HandleFunc("DELETE /api/backup", auth(s.handleDeleteBackup))
	mux.HandleFunc("GET /api/backup/list", auth(s.handleListBackups))
	mux.HandleFunc("POST /api/backup/now", auth(s.handleBackupNow))
	// Snapshot download = the entire database: provider API keys, chat
	// transcripts, the user table. Plain auth let any logged-in account
	// walk off with the full store — super-admin only.
	mux.HandleFunc("GET /api/backup/download", admin(s.handleDownloadBackup))
	// Online SQLite maintenance: staged backup→VACUUM coordinator.
	// Status is readable by any admin-session user; the vacuum trigger
	// is super-admin-only because it queues all DB traffic briefly.
	mux.HandleFunc("GET /api/maintenance/status", admin(s.handleMaintenanceStatus))
	mux.HandleFunc("POST /api/maintenance/vacuum", admin(s.handleMaintenanceVacuum))

	// Tasks
	mux.HandleFunc("GET /api/tasks", admin(s.handleListTasks))

	// Apikeys (per-user, with agent multi-select).
	mux.HandleFunc("GET /api/apikeys", auth(s.handleListAPIKeys))
	mux.HandleFunc("POST /api/apikeys", auth(s.handleCreateAPIKey))
	mux.HandleFunc("DELETE /api/apikeys/{id}", auth(s.handleDeleteAPIKey))
	mux.HandleFunc("POST /api/apikeys/{id}/rotate", auth(s.handleRotateAPIKey))

	// Users — flat resource paths. Top-level CRUD is admin-only;
	// nested {id}/apikeys + {id}/agents accept admin-or-self
	// (gated in-handler via requireUserOrAdmin).
	mux.HandleFunc("POST /api/users/{id}/apikeys", auth(s.handleCreateUserAPIKey))
	mux.HandleFunc("GET /api/users/{id}/agents", auth(s.handleListUserAgents))
	mux.HandleFunc("POST /api/users/{id}/agents", auth(s.handleCreateUserAgent))
	// Cross-tenant agent list moved into /api/agents?all=true (admin-only
	// when the param is set). /api/usage replaces /api/admin/usage.
	mux.HandleFunc("GET /api/usage", admin(s.handleGetUsage))
	// Per-agent usage: owner-gated (or super_admin) inside the handler
	// itself via requireAgentOwner, so we use the plain auth wrapper here.
	mux.HandleFunc("GET /api/agents/{id}/usage", auth(s.handleGetAgentUsage))

	// OpenAI-compatible API and WebSocket gateway.
	if s.apiServer != nil {
		s.apiServer.RegisterRoutes(mux)
	}

	// Static UI files. The build pipeline emits the Next.js static export
	// directly into the embed root (web/ — index.html + agents/default/...
	// live at the web/ path in the embed), so fs.Sub(webFS, "web") is the
	// export root, NOT web/out (that subdir is empty in the embed).
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		return fmt.Errorf("setup: embed sub: %w", err)
	}
	// Unknown /api/* and /v1/* paths must NOT fall through to the SPA
	// handler below: it would answer 200 + index.html for any method,
	// turning API typos, probes, and regressions into false-positive
	// successes. Registered before the "/" catch-all.
	apiNotFound := func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
	}
	mux.HandleFunc("/api/", apiNotFound)
	mux.HandleFunc("/v1/", apiNotFound)
	mux.Handle("/", spaHandler{fs: webRoot})

	var addr string
	if s.bind == "all" {
		addr = fmt.Sprintf("0.0.0.0:%d", s.port)
	} else {
		addr = fmt.Sprintf("127.0.0.1:%d", s.port)
	}
	// ReadHeaderTimeout stops slowloris-style header dribbling without
	// touching the body deadline (SSE/WS need unbounded read time).
	// IdleTimeout reaps keep-alive connections idle past 2 minutes.
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("setup: listen %s: %w", addr, err)
	}
	slog.Info("web UI running", "url", fmt.Sprintf("http://localhost:%d", s.port))
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// spaHandler serves the embedded Next.js UI with SPA-style fallback.
type spaHandler struct {
	fs fs.FS
}

func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path != "/" && strings.HasSuffix(path, "/") {
		path = strings.TrimSuffix(path, "/")
	}
	// Trim BOTH slashes: the leading one (request path always starts with
	// "/") and any trailing one. A trailing slash on a directory-style
	// URL like /agents/<id>/chat/ used to leave fsPath ending in "chat/",
	// which split into ["chat", ""] — and the dynamic-segment fallback
	// below then mistook the empty tail segment for a dynamic value,
	// substituting "_" and serving the chat/_/index.html session
	// placeholder instead of chat/index.html. That wrong placeholder made
	// Next's App Router hard-reload on every fresh-chat navigation
	// (project/<pid>/ was unaffected because pid is always non-empty).
	// Trimming the trailing slash collapses "chat/" back to "chat" so the
	// dirFallback (agents/default/chat/index.html) is served correctly.
	fsPath := strings.Trim(path, "/")
	if fsPath == "" {
		fsPath = "."
	}
	// Next 16 RSC flight URLs are flat (dot-separated, e.g.
	// overview/__next.overview.__PAGE__.txt) but the built files are
	// nested (overview/__next.overview/__PAGE__.txt). Convert any
	// __next.*.txt flat URL to its nested file UP FRONT so the real RSC
	// payload is served app-wide. Without this, the generic fallbacks
	// below serve index.html (HTML) for a .txt RSC request, the App
	// Router caches HTML-as-RSC, and the next soft navigation to that
	// route hard-reloads. (agent subroutes under /agents/<id>/ also hit
	// the default-fallback branch below because <id> has no built tree.)
	if idx := strings.Index(fsPath, "__next."); idx >= 0 && strings.HasSuffix(fsPath, ".txt") {
		prefix := fsPath[:idx]
		core := strings.TrimPrefix(strings.TrimSuffix(fsPath[idx:], ".txt"), "__next.")
		if segs := strings.Split(core, "."); len(segs) > 1 {
			nested := prefix + "__next." + segs[0] + "/" + strings.Join(segs[1:], "/") + ".txt"
			if f, err := h.fs.Open(nested); err == nil {
				stat, statErr := f.Stat()
				f.Close()
				if statErr == nil && !stat.IsDir() {
					http.ServeFileFS(w, r, h.fs, nested)
					return
				}
			}
		}
	}
	if f, err := h.fs.Open(fsPath); err == nil {
		stat, statErr := f.Stat()
		f.Close()
		if statErr == nil && !stat.IsDir() {
			http.ServeFileFS(w, r, h.fs, fsPath)
			return
		}
	}
	var indexPath string
	if fsPath == "." {
		indexPath = "index.html"
	} else {
		indexPath = fsPath + "/index.html"
	}
	if f, err := h.fs.Open(indexPath); err == nil {
		f.Close()
		http.ServeFileFS(w, r, h.fs, indexPath)
		return
	}
	if strings.HasPrefix(fsPath, "agents/") {
		parts := strings.SplitN(fsPath, "/", 3)
		if len(parts) >= 3 && parts[1] != "default" {
			// Next 16 RSC flight URLs are flat (dot-separated, e.g.
			// chat/__next.agents.$d$id.chat.__PAGE__.txt) but the built
			// files are nested (chat/__next.agents/$d$id/chat/__PAGE__.txt).
			// A flat directFallback misses the nested file, so for URLs
			// containing a __next.*.txt tail convert flat→nested and serve
			// the real RSC payload — otherwise the dynamic-segment fallback
			// below mis-substitutes and serves the session-placeholder
			// HTML, which makes the App Router hard-reload on fresh-chat
			// navigation (session routes are unaffected because they're not
			// prefetched on a wiki/knowledge page).
			if idx := strings.Index(parts[2], "__next."); idx >= 0 && strings.HasSuffix(parts[2], ".txt") {
				prefix := parts[2][:idx]
				core := strings.TrimPrefix(strings.TrimSuffix(parts[2][idx:], ".txt"), "__next.")
				if segs := strings.Split(core, "."); len(segs) > 1 {
					nestedPath := "agents/default/" + prefix + "__next." + segs[0] + "/" + strings.Join(segs[1:], "/") + ".txt"
					if f, err := h.fs.Open(nestedPath); err == nil {
						stat, statErr := f.Stat()
						f.Close()
						if statErr == nil && !stat.IsDir() {
							http.ServeFileFS(w, r, h.fs, nestedPath)
							return
						}
					}
				}
			}
			directFallback := "agents/default/" + parts[2]
			if f, err := h.fs.Open(directFallback); err == nil {
				stat, statErr := f.Stat()
				f.Close()
				if statErr == nil && !stat.IsDir() {
					http.ServeFileFS(w, r, h.fs, directFallback)
					return
				}
			}
			dirFallback := "agents/default/" + parts[2] + "/index.html"
			if f, err := h.fs.Open(dirFallback); err == nil {
				f.Close()
				http.ServeFileFS(w, r, h.fs, dirFallback)
				return
			}
			// Nested dynamic segment fallback: routes like
			// agents/[id]/chat/[session] and agents/[id]/project/[pid]
			// emit a single placeholder ("_") at build time. Substitute
			// "_" for any segment that sits immediately under a known
			// dynamic-parent (chat, project), regardless of what
			// follows. This covers BOTH the page HTML
			//   /chat/<sid>/                            → /chat/_/index.html
			// AND the per-route RSC payloads Next 16 fetches during
			// client-side navigation:
			//   /chat/<sid>/index.txt                   → /chat/_/index.txt
			//   /chat/<sid>/__next.agents.$d$id.chat.$d$session.__PAGE__.txt
			//   …
			// Without this, App Router's RSC fetch on a sidebar click
			// gets a 404 (or the root index.html), gives up on soft
			// navigation, and falls back to window.location — which
			// flickers the page and tears down any in-flight stream.
			// Add new dynamic routes to dynamicParents below as they
			// get introduced.
			dynamicParents := map[string]bool{"chat": true, "project": true}
			sub := strings.Split(parts[2], "/")
			substituted := false
			for i := 0; i < len(sub)-1; i++ {
				if dynamicParents[sub[i]] && sub[i+1] != "_" {
					sub[i+1] = "_"
					substituted = true
				}
			}
			if substituted {
				placeholder := "agents/default/" + strings.Join(sub, "/")
				if f, err := h.fs.Open(placeholder); err == nil {
					stat, statErr := f.Stat()
					f.Close()
					if statErr == nil && !stat.IsDir() {
						http.ServeFileFS(w, r, h.fs, placeholder)
						return
					}
				}
				placeholderIndex := placeholder + "/index.html"
				if f, err := h.fs.Open(placeholderIndex); err == nil {
					f.Close()
					http.ServeFileFS(w, r, h.fs, placeholderIndex)
					return
				}
			}
		}
	}
	htmlPath := fsPath + ".html"
	if f, err := h.fs.Open(htmlPath); err == nil {
		f.Close()
		http.ServeFileFS(w, r, h.fs, htmlPath)
		return
	}
	http.ServeFileFS(w, r, h.fs, "index.html")
}
