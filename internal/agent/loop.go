package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codeany-ai/open-agent-sdk-go/costtracker"

	"github.com/fluctio-ai/fluctio/internal/agent/goal"
	"github.com/fluctio-ai/fluctio/internal/agent/tools"
	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/channels"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/embedding"
	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/mcp"
	"github.com/fluctio-ai/fluctio/internal/privacy"
	"github.com/fluctio-ai/fluctio/internal/provider"
	coderuntime "github.com/fluctio-ai/fluctio/internal/runtime"
	"github.com/fluctio-ai/fluctio/internal/sandbox"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/session"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/toolproviders"
	"github.com/fluctio-ai/fluctio/internal/usage"
	"github.com/fluctio-ai/fluctio/internal/workspace"
	"github.com/fluctio-ai/fluctio/internal/workflow"
)

// Agent is the ReAct agent loop.
type Agent struct {
	id         string
	name       string
	provider   provider.Provider
	registry   *tools.Registry
	sessions   *session.Manager
	memory     *Memory
	ctxBuilder *ContextBuilder
	mcpMgr     *mcp.Manager
	// mcpServers holds the agent's MCP server config; the MCP manager is
	// built/rebuilt per-session in bindSession (cwd = scopeDir) because a
	// stdio subprocess's working dir can't change after start.
	mcpServers    map[string]config.MCPServerConfig
	mcpSessionDir string
	hooks         *HookRegistry
	model         string
	// summaryModel overrides model for conversation-summary extraction;
	// empty falls back to model. Set by the manager from memory config.
	summaryModel string
	// embedder generates vectors for conversation-summary recall; nil
	// (or !Available) → vector path skipped, FTS-only recall.
	embedder embedding.Embedder
	// authGate enforces the per-agent write/exec authorization policy
	// (hardline commands + workspace boundary + dangerous-pattern tier).
	// nil when the agent has no relational store; evaluateCall is a no-op
	// (allow) in that case.
	authGate *authGate
	// authMode is the session-authorization mode (ask/auto/yolo). Empty
	// defaults to ask. /yolo /auto slash commands flip it at runtime.
	authMode             string
	maxTokens            int
	temperature          float64
	maxToolIterations    int
	maxParallelToolCalls int // 0 = unlimited
	thinking             string
	// promptMode is kept on Agent so ReloadWorkspaceFiles can re-apply it
	// when it rebuilds ctxBuilder — without this, every skill install /
	// dashboard reload silently drops the agent back to agent-mode prompt
	// even after the operator explicitly chose chatbot/customize.
	// PromptMode also drives the per-turn tool filter via
	// builtinAllowForMode below.
	promptMode    string
	homePath      string // agent's home: SOUL.md, sessions, memory, skills
	workspacePath string // working dir where agent creates user files
	homeDir       string // Fluctio root, ~/.fluctio
	ownerUserID   string // the user that owns this agent (for hook namespacing)
	// admins is the per-channel allowlist of chatters who can run write-
	// mode slash commands (/new /undo /retry /compact /model /personality).
	// Keyed by channel name (e.g. "discord" → ["123...", "456..."]). Empty
	// or absent → no gate, anyone can run the command (legacy default).
	admins            map[string][]string
	skillsCfg         config.SkillsConfig
	globalSkillsCfg   config.SkillsCfg
	messageBus        *bus.MessageBus
	subAgentSpawner   tools.SubAgentSpawner
	ftsStore          *store.FTSStore
	piiScrubEnabled   bool
	piiEntropyEnabled bool
	memoryCfg         config.MemoryCfg
	// splitReplies is the per-agent multi-bubble toggle. Gates the
	// per-turn system-prompt hint that advertises SplitMessageMarker
	// to the LLM (see renderChannelHints) AND stamps
	// OutboundMessage.AllowSplit so the dispatcher splits the reply at
	// the marker before handing each chunk to the channel adapter.
	// Per-agent only — there's no system-level fallback.
	splitReplies bool
	// memoryStore is the optional Store-backed source of identity files
	// (SOUL.md, IDENTITY.md, ...). Kept on the Agent so ReloadWorkspaceFiles
	// can rewire a fresh ContextBuilder to keep reading from the Store
	// instead of silently falling back to pod-local filesystem.
	memoryStore MemoryStore
	// displayName mirrors agents.name (the operator-given name). Stamped
	// on the ContextBuilder for the IDENTITY.md fallback line — kept on
	// Agent too so ReloadWorkspaceFiles can re-apply after rebuilding
	// the ContextBuilder from scratch.
	displayName string
	// language is the agent's default UI language for slash-command
	// replies when the inbound source carries none (IM channels, cron).
	// Loaded from agent.json's "language" field; empty → slashT's own
	// default (Chinese). HandleMessage falls back to this after popLang
	// when msg.Lang is empty.
	language string
	// singleUser marks this as a single-user install (owner.json present
	// under the Fluctio home). When true, every chatter is treated as the
	// operator for the identity-file confidentiality gate — so IM
	// chatters can read/write IDENTITY.md / SOUL.md and a name they give
	// the agent in conversation actually persists. Multi-user installs
	// keep the gate closed: non-owner chatters stay blocked from the
	// operator's private configuration.
	singleUser bool
	// dataStore is the full relational Store (when wired by the
	// manager). Used for per-turn durable lookups that can't go through
	// the narrower MemoryStore — currently just the autoPersist gate
	// counting (chatter, agent) user-message rows so the cadence
	// survives daemon restarts / UserSpace invalidations / idle
	// evictions that all reset the in-memory turnCount.
	dataStore store.Store
	// workspaceStore is optional; when set, SkillsLoader hydrates per-agent
	// and global skill dirs from the object store on every turn so skills
	// uploaded post-boot or on a sibling replica become visible here.
	workspaceStore workspace.Store
	skillsLearner  *SkillsLearner
	// turnCount is atomic: taskqueue serializes per chat but the global
	// maxConcurrent lets turns on different chats of the SAME agent run
	// in parallel — runPostTurn's increment and the auto-persist gate's
	// read would otherwise be a data race.
	turnCount atomic.Int64
	engine    *sdkEngine
	costTracker    *costtracker.Tracker
	agentID        string
	// meter is the admin-level token meter. Non-nil only when the
	// gateway wires it in via SetMeter at boot — local-only dev runs
	// leave it nil and metering becomes a no-op via meterTokens().
	meter usage.Meter
	// quotaStore is the per-user billing quota store. When set, the
	// agent loop checks the owner's quota before processing a turn.
	// Nil means no quota enforcement (unlimited).
	quotaStore usage.QuotaStore
	// sandboxPool is the per-user (agent + session) sandbox pool. Set
	// once at boot/hot-reload by attachSandboxToAgents; bindSession
	// pulls a session-scoped executor from it at the top of every turn
	// so concurrent sessions of the same agent get isolated containers
	// + isolated /workspace mounts.
	sandboxPool sandbox.ExecutorPool

	// goalStore is the /goal feature's per-Agent state. Wired by
	// WireGoals; nil on agents whose Manager didn't provide a data
	// store (legacy single-user installs). When nil, the goal tools
	// and hook are simply not registered, so a missing store silently
	// degrades to "feature off" rather than crashing.
	goalStore goal.Store

	// kbStore is the agent's knowledge-base store. Wired by the manager
	// alongside RegisterKBTools; nil on agents without a relational store,
	// in which case /bookmark degrades to a "feature off" message. Used by
	// the /bookmark slash command so it can save straight to the bookmark
	// table without going through the LLM.
	kbStore *kb.KBStore

	// projectRuntime, when non-nil, turns this agent into a coding agent:
	// it can scaffold a project from a template, boot a dev server, and
	// hand back a preview URL via the start_app_preview / app_preview_logs
	// tools. Wired by attachProjectRuntimeToAgents at boot. Nil for
	// ordinary agents, which then never see those tools and keep their
	// per-chat file isolation. See SetProjectRuntime.
	projectRuntime *coderuntime.Manager

	// workflowSvc, when non-nil, holds this agent's own workflows (the
	// gateway loads them from this agent's home/workflows directory at boot)
	// and backs the per-workflow tools registered by SetWorkflowService. Nil
	// = this agent has no workflows. See SetWorkflowService.
	workflowSvc *workflow.Service

	// contextWindow is the agent's active model's context-window size
	// (tokens), injected by the manager from Phase 1's ResolvedAgent.
	// 0 = unknown → compaction falls back to the legacy flat threshold.
	contextWindow int
	// compactionThreshold is an operator-set fixed threshold (tokens).
	// 0 = unset → the loop computes a dynamic threshold from
	// contextWindow and compactionMode (Phase 2 Task 3).
	compactionThreshold int
	// compactionMode selects the margin aggressiveness for the dynamic
	// threshold: "conservative" (30%), "balanced"/"" (15%, default),
	// or "aggressive" (10%). See modeMarginPct.
	compactionMode string
}

// compactionThresholdNow computes the effective compaction threshold for
// the current turn, factoring in the live system-prompt token count.
// Delegates to computeThreshold (priority: manual > dynamic > fallback).
func (a *Agent) compactionThresholdNow(systemPrompt string) int {
	sysTokens := EstimateTokens([]provider.Message{{Role: "system", Content: systemPrompt}})
	return computeThreshold(a.compactionThreshold, a.contextWindow, sysTokens, a.maxTokens, a.compactionMode)
}

// CompactionPreview is the JSON-serialisable projection of the agent's
// compaction geometry, surfaced to the context-page UI so the operator
// can see what each mode would actually threshold at before picking one.
// SystemPromptTokens is 0 in v1 (we don't build the system prompt here),
// so the per-mode threshold is the maximal value; the live loop subtracts
// the real system-prompt token count via compactionThresholdNow.
type CompactionPreview struct {
	ContextWindow      int            `json:"contextWindow"`
	MaxTokens          int            `json:"maxTokens"`
	SystemPromptTokens int            `json:"systemPromptTokens"`
	Modes              map[string]int `json:"modes"` // conservative/balanced/aggressive → threshold
	ManualThreshold    int            `json:"manualThreshold,omitempty"`
	// CompactionMode is the agent's currently-saved mode so the UI can
	// pre-select the right radio on page load. "" = balanced (default).
	CompactionMode string `json:"compactionMode,omitempty"`
}

// CompactionPreview returns the per-mode threshold estimates for this
// agent. sysTokens=0 (v1 simplification — the live loop factors the real
// system prompt in via compactionThresholdNow). Floor 1000 matches
// computeThreshold so the preview can never show a value the loop would
// never actually use.
func (a *Agent) CompactionPreview() CompactionPreview {
	const sysTokens = 0
	modes := map[string]int{}
	for _, m := range []string{"conservative", "balanced", "aggressive"} {
		margin := a.contextWindow * modeMarginPct(m) / 100
		t := a.contextWindow - sysTokens - a.maxTokens - margin
		if t < 1000 {
			t = 1000
		}
		modes[m] = t
	}
	return CompactionPreview{
		ContextWindow:      a.contextWindow,
		MaxTokens:          a.maxTokens,
		SystemPromptTokens: sysTokens,
		Modes:              modes,
		ManualThreshold:    a.compactionThreshold,
		CompactionMode:     a.compactionMode,
	}
}

// SetSandboxPool wires the per-(agent,session) executor pool. Called by
// attachSandboxToAgents on boot and by hot-reload's reloadSandbox after
// onboarding flips sandbox on. The pool is consulted by bindSession at
// the start of every chat turn — there's no eager Get at boot anymore
// because session IDs only exist once a chat starts.
//
// Also flips the context builder's sandbox flag so the system prompt's
// "Working Directory" / filesystem-layout description matches reality.
// Without this, an agent whose rc.Sandbox.Enabled=false but who got a
// pool reference (attachSandboxToAgents wires the pool to ALL agents
// once any one of them wants sandbox) ends up with exec routed through
// the container while the prompt still advertises host paths — model
// dutifully writes `/Users/.../workspaces/<id>/foo` which 404s inside
// the container. The two states must agree.
func (a *Agent) SetSandboxPool(p sandbox.ExecutorPool) {
	a.sandboxPool = p
	if a.ctxBuilder != nil {
		a.ctxBuilder.sandboxEnabled = p != nil
	}
	if a.authGate != nil {
		a.authGate.setSandboxed(p != nil)
	}
	// Tell the tool registry sandbox is required so its host-shell exec
	// fallback refuses to run when bindSession can't bind an executor.
	// The two states (system prompt advertising /workspace + /skills,
	// exec actually using sandbox) must agree — without this, a Docker
	// daemon hiccup turns into "sh: python: command not found" on the
	// host instead of a clear "sandbox required but unavailable" error.
	if a.registry != nil {
		a.registry.SetSandboxRequired(p != nil)
	}
}

// bindSession wires per-turn session state into the tool registry: the
// session-scoped sandbox executor (when a pool is configured), the
// sessionID workspace.Store calls use to namespace artifacts, and the
// (channel, accountID, chatID) bus address so deferred-work tools (create_cron_job)
// can stamp it onto persisted rows for later replay. Called at the top
// of HandleMessage / HandleMessageStream before any tool runs.
//
// Mutating the shared registry across concurrent chats would race, but
// the current invariant is one chat-in-flight per agent — the gateway
// serializes per-agent turns. Documenting it here in case that changes.
func (a *Agent) bindSession(ctx context.Context, channel, accountID, chatID, sessionKey, projectID string) {
	a.registry.SetSessionID(sessionKey)
	a.registry.SetProjectID(projectID)
	// Scope the on-disk user root to this session so host-mode file tools
	// (write_file/read_file/edit_file/list_dir via rootForPath) and host
	// exec land in sessions/<sessionKey>/ — per-session, so a /new starts
	// a fresh empty file set instead of inheriting the prior session's
	// files — or projects/<pid>/ (shared across the project's chats). The
	// session branch keys on session_key, NOT the channel chat_id; chat_id
	// is kept separately (SetMessageContext below) for delivery addressing.
	// Matches runtime.scopeFor and docker's per-session /workspace/<sid>.
	scopeDir := ""
	if a.workspacePath != "" && (sessionKey != "" || projectID != "") {
		seg := "sessions/" + sessionKey
		if projectID != "" {
			seg = "projects/" + projectID
		}
		scopeDir = filepath.Join(a.workspacePath, seg)
		if err := os.MkdirAll(scopeDir, 0o755); err != nil {
			slog.Warn("bindSession: mkdir scope dir failed",
				"agent", a.name, "dir", scopeDir, "error", err)
		} else {
			a.registry.SetUserRoot(scopeDir)
		}
	}

	// MCP per-session: a stdio subprocess's cwd is fixed at start, so
	// when the chat's scope changes we rebuild the manager (Close old,
	// start new with cwd = scopeDir) so MCP tools (Playwright screenshots,
	// file-backed MCP servers) land in this session's dir instead of the
	// gateway's launch dir. Re-registering overwrites the old closures;
	// the loop serializes per-agent turns so the swap can't race an
	// in-flight MCP call.
	if len(a.mcpServers) > 0 && scopeDir != "" && a.mcpSessionDir != scopeDir {
		if a.mcpMgr != nil {
			a.mcpMgr.Close()
		}
		a.mcpMgr = mcp.NewManager(a.mcpServers, scopeDir)
		a.mcpSessionDir = scopeDir
		// Build server prefix lookup once per rebuild so each tool def can
		// be attributed back to its server config (needed to read Effect /
		// ToolEffects). mcp.Manager.toolMap is unexported and the brief
		// says don't touch the mcp pkg, so reverse-resolve via the same
		// sanitization the mcp package applies when constructing prefixed
		// tool names. Iterating a.mcpServers (config map) keeps this stable
		// even if the manager silently skipped a server that failed to
		// connect — its prefix just won't match any tool.
		for _, td := range a.mcpMgr.ToolDefs() {
			toolName := td.Name
			srvName, srvCfg := lookupMCPServer(toolName, a.mcpServers)
			effect := mcpDefaultEffect(srvName, toolName, srvCfg)
			a.registry.RegisterFromWithEffect(toolName, td.Description, td.InputSchema,
				func(ctx context.Context, args json.RawMessage) (string, error) {
					return a.mcpMgr.CallTool(ctx, toolName, args)
				}, tools.SourceMCP, effect)
		}
		if a.mcpMgr.HasTools() {
			slog.Info("registered MCP tools (scoped)",
				"agent", a.name, "scope", scopeDir)
		}
	}
	// Surface MCP server names + their cwd to the runtime_context prompt
	// module so the model knows which external tool servers are live and
	// where they drop artifacts (may differ from the visible workspace).
	// Placed after the per-session MCP rebuild so mcpSessionDir is current.
	a.ctxBuilder.mcpServerSummary = summarizeMCPServers(a.mcpServers, a.mcpSessionDir)
	// Coding agents (those with a project runtime wired) treat a project
	// as ONE shared app tree: file tools address the project root so the
	// agent's edits land where the dev server serves. Only when actually
	// inside a project; loose chats and non-coding agents are unaffected.
	a.registry.SetCodingRootScope(a.projectRuntime != nil && projectID != "")
	// If this scope already has a running app (a runtime record exists),
	// redirect file tools into its app subfolder so edits keep landing
	// where the dev server serves — across turns, not just the turn that
	// called start_app_preview. EffectiveUserID is the owner here
	// (chatter is bound later), which is correct for the web-direct case.
	a.registry.SetCodingSubdir("")
	if a.projectRuntime != nil {
		if uid := a.registry.EffectiveUserID(); uid != "" {
			if _, err := a.projectRuntime.Get(ctx, uid, a.name, projectID, sessionKey); err == nil {
				a.registry.SetCodingSubdir(coderuntime.AppSubdir)
			}
		}
	}
	a.registry.SetMessageContext(channel, accountID, chatID)
	if a.sandboxPool == nil {
		return
	}
	ex, err := a.sandboxPool.Get(ctx, a.name, projectID, sessionKey)
	if err != nil {
		// Error level (not warn) — when sandbox is required and we
		// can't bind, the next exec call will refuse with the
		// "sandboxRequired but no executor" message; log here so the
		// upstream cause (docker daemon down, image pull failed, …) is
		// captured next to the user-facing error.
		slog.Error("sandbox executor unavailable; exec will refuse host fallback",
			"agent", a.name, "session", sessionKey, "error", err)
		return
	}
	a.registry.SetExecutor(ex)
}

// summarizeMCPServers renders a compact, human-readable digest of the MCP
// servers bound to this session: "name (cwd: <dir>), name2 (cwd: <dir>)".
// The sessionDir is the cwd the MCP manager was started with (set by
// bindSession when it rebuilds the per-session MCP manager); servers fall
// back to "<unset>" when no session scope is wired yet (e.g. gateway boot
// before any chat). Returns "" when no MCP server is configured, which the
// runtime_context prompt module collapses to "（无）".
func summarizeMCPServers(servers map[string]config.MCPServerConfig, sessionDir string) string {
	if len(servers) == 0 {
		return ""
	}
	var parts []string
	cwd := sessionDir
	if cwd == "" {
		cwd = "<unset>"
	}
	for name := range servers {
		parts = append(parts, fmt.Sprintf("%s (cwd: %s)", name, cwd))
	}
	return strings.Join(parts, ", ")
}

// NewAgent creates a new Agent from a resolved config.
// bgProvider returns a provider whose outgoing messages are PII-scrubbed
// when privacy scrubbing is enabled. For background LLM pipelines
// (compaction, topic summaries, auto-persist, insights) that consume raw
// conversation content outside the interactive loop's scrub point —
// without it they silently bypassed the privacy.piiScrubbing switch.
// Returns the raw provider when scrubbing is off.
func (a *Agent) bgProvider() provider.Provider {
	return privacy.WrapProvider(a.provider, privacy.Options{Entropy: a.piiEntropyEnabled}, a.piiScrubEnabled)
}

func NewAgent(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, homeDir string) *Agent {
	return NewAgentWithSkillsCfg(rc, prov, mb, homeDir, config.SkillsCfg{})
}

// NewAgentWithFullCfg creates a new Agent with full config support (memory, privacy, skills learner).
func NewAgentWithFullCfg(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, homeDir string, fullCfg *config.Config) *Agent {
	ag := NewAgentWithSkillsCfg(rc, prov, mb, homeDir, fullCfg.Skills)
	ag.memoryCfg = fullCfg.Memory
	ag.piiScrubEnabled = fullCfg.Privacy.PIIScrubbing.Enabled
	ag.piiEntropyEnabled = fullCfg.Privacy.PIIScrubbing.Entropy
	if ag.skillsLearner != nil {
		ag.skillsLearner.SetPIIScrub(ag.piiScrubEnabled, ag.piiEntropyEnabled)
	}
	// splitReplies is plumbed inside NewAgentWithSkillsCfg so foreign-
	// attached agents also pick up the toggle; don't re-stamp here.

	// Set up FTS store if configured
	if fullCfg.Memory.FTS.Enabled {
		dbPath := fullCfg.Memory.FTS.DBPath
		if dbPath == "" {
			dbPath = rc.Home + "/memory/fts.db"
		}
		if fts, err := store.NewFTSStore(dbPath); err == nil {
			if err := fts.Init(); err == nil {
				ag.ftsStore = fts
				slog.Info("FTS5 search enabled", "agent", rc.ID, "db", dbPath)
			} else {
				slog.Warn("FTS5 init failed, falling back to file scan", "error", err)
			}
		} else {
			slog.Warn("FTS5 store open failed, falling back to file scan", "error", err)
		}
	}

	// Set up skills learner if configured
	if fullCfg.SkillsLearner.Enabled {
		model := fullCfg.SkillsLearner.Model
		if model == "" {
			model = rc.Model
		}
		learnerLoader := NewSkillsLoaderWithGlobal(homeDir, rc.Home, rc.Skills, fullCfg.Skills)
		learnerLoader.agentID = rc.ID
		ag.skillsLearner = NewSkillsLearner(rc.Home, rc.Home, prov, model, learnerLoader.AllSkillDirs()...)
		if fullCfg.SkillsLearner.MinToolCalls > 0 {
			ag.skillsLearner.minToolCalls = fullCfg.SkillsLearner.MinToolCalls
		}
	}

	// Set memory auto-persist defaults
	if ag.memoryCfg.AutoPersist.EveryNTurns == 0 {
		ag.memoryCfg.AutoPersist.EveryNTurns = 5
	}

	return ag
}

// NewAgentWithSkillsCfg creates a new Agent with global skills config for env injection.
func NewAgentWithSkillsCfg(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, homeDir string, globalSkillsCfg config.SkillsCfg) *Agent {
	workspace := rc.Workspace
	if workspace == "" {
		// Fallback for callers (tests, legacy configs) that don't populate
		// Workspace — use the agent's home as a single-dir fallback.
		workspace = rc.Home
	}
	// Ensure the workspace dir exists so the first write_file doesn't fail.
	if workspace != "" {
		_ = os.MkdirAll(workspace, 0o755)
	}

	memory := NewMemory(rc.Home)
	registry := tools.NewRegistry(rc.Home, workspace)
	// message tool is re-registered AFTER the Agent struct is built (see
	// below) so its outbound-side closure can read agent.splitReplies
	// at send time. The registerBuiltins pass inside NewRegistry already
	// stamped a placeholder; tools.RegisterMessage replaces it.
	tools.RegisterMemorySearch(registry, rc.Home)
	tools.RegisterMemoryFetch(registry) // layered injection: detail fetch for memory_search's compact index
	tools.RegisterWebFetch(registry)

	// Load skills with OpenClaw compatibility. We can't hydrate from OSS
	// here — the Agent isn't constructed yet and the manager hasn't wired
	// workspaceStore. The manager will call ReloadWorkspaceFiles after
	// wiring to refresh the summary with OSS-hosted skills, and runOnce
	// re-hydrates on every turn to pick up later uploads.
	loader := NewSkillsLoaderWithGlobal(homeDir, rc.Home, rc.Skills, globalSkillsCfg)
	loader.agentID = rc.ID
	skills := loader.LoadSkills()
	skillsSummary := loader.BuildSkillsSummary(skills)

	// Set up skill env injection for exec tool. Pass an sbCfg carrying
	// just the Enabled flag so the host-mode closure (used until
	// bindSession swaps in a sandboxed executor on session start) knows
	// sandbox was REQUIRED for this agent — without that signal an
	// executor-pool failure would silently fall through to /bin/sh on the
	// host, defeating the security boundary the user asked for.
	skillDirs := loader.AllSkillDirs()
	tools.RegisterLoadSkill(registry, skillDirs, buildSkillGate(skills))
	// Wire the agent-private skill install tools so chat-initiated
	// installs land in agents/<id>/agent/skills instead of the
	// chatter's workspace. onReload=nil: SkillsLoader re-scans this
	// dir on every turn, so the new skill is picked up next turn.
	tools.RegisterSkillInstall(registry, filepath.Join(rc.Home, "skills"), nil)
	// Phase 4 write-approval gate: skill_manage writes to skills-pending/
	// (NOT live). The parser closure re-uses parseFrontmatterFromBytes +
	// CheckGating so the tool result can echo the same gating verdict the
	// system-prompt catalog and load_skill already use. Approval happens
	// out-of-band (CLI in P4 Task 2), not in-process.
	tools.RegisterSkillManage(registry, rc.Home, "fluctio skill approve",
		func(b []byte) *tools.SkillManifest {
			fm := parseFrontmatterFromBytes(b)
			if fm == nil {
				return nil
			}
			var meta *SkillMetadata
			if fm.Metadata.Kind != 0 {
				meta = parseMetadata(&fm.Metadata)
			}
			gated, reason := CheckGating(meta)
			return &tools.SkillManifest{
				Name:        fm.Name,
				Description: fm.Description,
				Gated:       gated,
				GateReason:  reason,
			}
		})
	var sbCfg *tools.SandboxConfig
	if rc.Sandbox.Enabled {
		sbCfg = &tools.SandboxConfig{Enabled: true}
	}
	tools.RegisterExecWithSkillEnv(registry, sbCfg, loader.SkillEnvVars, skillDirs)

	if len(skills) > 0 {
		slog.Info("loaded skills", "agent", rc.ID, "count", len(skills))
	}

	// Set up hooks with logging
	hooks := NewHookRegistry()
	hooks.Register(BeforeModelCall, LoggingHook())
	hooks.Register(AfterModelCall, LoggingHook())
	hooks.Register(BeforeToolCall, LoggingHook())
	hooks.Register(AfterToolCall, LoggingHook())

	eng := newSDKEngine(rc.ID)
	// single-user install (owner.json present under the Fluctio home)
	// loosens the identity-file confidentiality gate so IM chatters can
	// read/write IDENTITY.md — e.g. to persist a name they gave the
	// agent. Multi-user installs keep the gate: non-owner chatters stay
	// blocked from the operator's private configuration.
	singleUserOwner, _ := config.LoadOwnerFile(homeDir)

	ag := &Agent{
		name:                 rc.ID,
		provider:             prov,
		registry:             registry,
		sessions:             session.NewManager(rc.Home + "/sessions"),
		memory:               memory,
		ctxBuilder:           newContextBuilderWithSandbox(rc.Home, workspace, memory, skillsSummary, rc.Thinking, rc.Guidance, rc.Sandbox.Enabled, rc.Sandbox.Backend, rc.PromptMode),
		hooks:                hooks,
		model:                rc.Model,
		maxTokens:            rc.MaxTokens,
		temperature:          rc.Temperature,
		maxToolIterations:    rc.MaxToolIterations,
		maxParallelToolCalls: rc.MaxParallelToolCalls,
		thinking:             rc.Thinking,
		promptMode:           rc.PromptMode,
		language:             rc.Language,
		singleUser:           singleUserOwner != nil,
		homePath:             rc.Home,
		workspacePath:        workspace,
		homeDir:              homeDir,
		admins:               rc.Admins,
		skillsCfg:            rc.Skills,
		globalSkillsCfg:      globalSkillsCfg,
		messageBus:           mb,
		engine:               eng,
		costTracker:          eng.costTracker,
	}
	slog.Info("diag: agent built", "id", rc.ID, "singleUser", ag.singleUser, "homeDir", homeDir)

	// authGate enforces the per-agent write/exec policy (hardline command
	// floor + workspace boundary + dangerous-pattern tier). Built once
	// from the agent root + workspace; policy.json reloads on agent reload.
	ag.authGate = newAuthGate(filepath.Dir(rc.Home), workspace)

	// Multi-bubble split-replies: per-agent only — system-level toggle
	// was removed since "every agent splits the same way" is rarely
	// what an operator wants for a deployment running multiple personas.
	// nil override = off (default); non-nil = explicit value. Plumbed at
	// this layer (not just NewAgentWithFullCfg) so foreign-attached
	// agents — chatters reaching an agent they don't own via a channel
	// binding — also pick up the toggle. Without this the wechat
	// dispatcher hint never reaches the LLM for non-owner chatters and
	// the model falls back to markdown `---` separators that render as
	// one bubble.
	if rc.SplitReplies != nil {
		ag.splitReplies = *rc.SplitReplies
	}
	// Stamp the operator-given display name onto the context builder
	// so an empty IDENTITY.md doesn't leak the base-model identity
	// ("I am Claude") through to chatters — the system prompt's
	// identity-fallback line uses this. Also keep on the Agent so
	// ReloadWorkspaceFiles (which rebuilds the ContextBuilder from
	// scratch) can re-apply it instead of losing the value.
	ag.displayName = rc.DisplayName
	ag.ctxBuilder.SetDisplayName(rc.DisplayName)
	// Auto-persist memory toggle — per-agent override. The manager
	// today only ever calls NewAgentWithSkillsCfg (not the unused
	// NewAgentWithFullCfg), which means the system/user `memory`
	// configs row is effectively dead in production — per-agent
	// agents.defaults.autoPersist is the only working path. Set
	// EveryNTurns default here too so the modulo check at the
	// runPostTurn site doesn't panic when an operator enables
	// AutoPersist without specifying a cadence.
	if rc.AutoPersist != nil {
		ag.memoryCfg.AutoPersist.Enabled = *rc.AutoPersist
	}
	if ag.memoryCfg.AutoPersist.EveryNTurns == 0 {
		ag.memoryCfg.AutoPersist.EveryNTurns = 5
	}

	// message tool — registered HERE (post-Agent) so the closure can read
	// ag.splitReplies at every send. Per-agent setting can flip at
	// runtime (UpdateConfig); the getter pulls the current value each
	// time rather than capturing a stale snapshot.
	tools.RegisterMessage(registry, mb, func() bool { return ag.splitReplies })

	// delegate_task lets the parent agent fan a bounded subtask out to a
	// fresh sub-agent context (own iteration budget, isolated messages).
	// Registered after ag is built because the tool callback closes over
	// ag.RunSubagent — couldn't wire it inside RegisterExecWithSkillEnv's
	// pre-Agent block. Self-disables when runner is nil.
	tools.RegisterDelegateTask(registry, ag)

	// MCP server config is stashed for bindSession, which builds (and
	// rebuilds) the MCP manager per-session with cwd = scopeDir. A stdio
	// subprocess's working dir can't change after start, so per-session
	// scope — Playwright screenshots landing in sessions/<sid>/ instead
	// of the gateway's launch dir — requires restarting the MCP server
	// whenever the chat's scope changes.
	ag.mcpServers = rc.MCPServers

	return ag
}

func newContextBuilderWithThinking(home string, memory *Memory, skillsSummary string, thinking string, guidance string) *ContextBuilder {
	cb := NewContextBuilder(home, memory, skillsSummary)
	if thinking != "" {
		cb.SetThinking(thinking)
	}
	cb.SetGuidance(guidance)
	return cb
}

func newContextBuilderWithSandbox(home, workspace string, memory *Memory, skillsSummary string, thinking string, guidance string, sandboxEnabled bool, sandboxBackend string, promptMode string) *ContextBuilder {
	cb := newContextBuilderWithThinking(home, memory, skillsSummary, thinking, guidance)
	cb.SetWorkspace(workspace)
	cb.sandboxEnabled = sandboxEnabled
	cb.sandboxBackend = sandboxBackend
	cb.SetPromptMode(promptMode)
	return cb
}

// Name returns the agent's name.
func (a *Agent) Name() string {
	return a.name
}

// ID returns the agent's stable identifier (e.g. "agt_…"). Set by the
// Manager when the agent is registered; empty for standalone agents never
// registered through the manager.
func (a *Agent) ID() string {
	return a.id
}

// HandleWebChat handles a chat message from the web UI with a session ID.
// imageURLs and params mirror the streaming variant so non-streaming
// callers (third-party apps hitting POST /api/chat) get the same
// vision + per-turn-params support as the SSE path.
//
// projectIDHint is the chat's "owning project" as carried in the URL
// (`?project=<pid>`) or chat request body. It only matters on the very
// first turn of a brand-new session: once the row exists, project_id
// stamped on it is authoritative and the hint is ignored.
func (a *Agent) HandleWebChat(ctx context.Context, sessionId, projectIDHint, userID, text string, imageURLs []string, params map[string]any) string {
	if sessionId == "" {
		sessionId = "web-ui"
	}
	if userID == "" {
		// Backward compat for unauth'd / legacy callers: keep the
		// sentinel so the per-user skills mount lands at a stable shared
		// dir instead of trying to mkdir <base>/users//skills/ (which
		// docker would happily mount over the user's whole home dir).
		userID = "web-user"
	}
	channel, accountID, chatID, projectID := a.recoverWebTriple(sessionId)
	if projectID == "" {
		projectID = projectIDHint
	}
	msg := bus.InboundMessage{
		Channel:   channel,
		AccountID: accountID,
		ChatID:    chatID,
		ProjectID: projectID,
		UserID:    userID,
		Text:      text,
		PeerKind:  "dm",
		PhotoURLs: imageURLs,
		Params:    params,
	}
	return a.HandleMessage(ctx, msg)
}

// HandleWebChatStream handles a web chat message with real-time event streaming.
// imageURLs carries any user-attached images (data URLs or fetchable HTTPS
// links) so vision-capable models receive them as image_url content parts on
// the user message. projectIDHint mirrors HandleWebChat's parameter — see
// that doc.
func (a *Agent) HandleWebChatStream(ctx context.Context, sessionId, projectIDHint, userID, text string, imageURLs []string, params map[string]any, events chan<- ChatEvent) string {
	if sessionId == "" {
		sessionId = "web-ui"
	}
	if userID == "" {
		userID = "web-user"
	}
	ctx = ContextWithChatEvents(ctx, events)
	channel, accountID, chatID, projectID := a.recoverWebTriple(sessionId)
	if projectID == "" {
		projectID = projectIDHint
	}
	msg := bus.InboundMessage{
		Channel:   channel,
		AccountID: accountID,
		ChatID:    chatID,
		ProjectID: projectID,
		UserID:    userID,
		Text:      text,
		PeerKind:  "dm",
		PhotoURLs: imageURLs,
		Params:    params,
	}
	return a.HandleMessage(ctx, msg)
}

// RollbackPendingUserWeb removes the trailing unanswered user turn from
// the web session named by sessionId — both the in-memory working set
// (and its sessions.messages JSONB mirror) and the session_messages
// archive row. The POST /api/chat/stream handler calls this BEFORE
// HandleWebChatStream appends the fresh user turn, so the LLM sees one
// copy of the reworded question instead of the failed original + the
// resend as two back-to-back user turns. Returns false if the last
// message isn't an unanswered user turn (no-op). Session resolution
// mirrors HandleWebChatStream so we land on the same *Session pointer.
func (a *Agent) RollbackPendingUserWeb(sessionId, projectIDHint string) bool {
	if sessionId == "" {
		sessionId = "web-ui"
	}
	channel, accountID, chatID, projectID := a.recoverWebTriple(sessionId)
	if projectID == "" {
		projectID = projectIDHint
	}
	sess := a.sessions.Get(channel, accountID, chatID, projectID)
	if sess == nil {
		return false
	}
	if !sess.RollbackLastUser() {
		return false
	}
	if a.dataStore != nil {
		if _, err := a.dataStore.DeleteLastUserMessage(context.Background(), sess.AgentID(), sess.Key()); err != nil {
			slog.Warn("rollback: delete last user archive row", "agent", a.name, "session", sess.Key(), "error", err)
		}
	}
	return true
}

// SteerWeb buffers a steering message for an in-flight web turn on the
// given session. Returns true if a turn was active and the message was
// buffered (the running loop will fold it in between tool rounds and
// emit a "steer" event on the existing SSE), false if no turn is
// running — in which case the caller should fall back to a normal send.
// Session resolution mirrors HandleWebChatStream exactly so we land on
// the same *session.Session pointer the running turn holds.
func (a *Agent) SteerWeb(sessionId, projectIDHint, text string) bool {
	if sessionId == "" {
		sessionId = "web-ui"
	}
	channel, accountID, chatID, projectID := a.recoverWebTriple(sessionId)
	if projectID == "" {
		projectID = projectIDHint
	}
	sess := a.sessions.Get(channel, accountID, chatID, projectID)
	return sess.PushSteerIfActive(provider.Message{
		Role:      "user",
		Content:   text,
		Timestamp: time.Now().UnixMilli(),
	})
}

// SteerInbound buffers a steering message for an in-flight turn keyed by
// the inbound message's (channel, accountID, chatID, projectID) — the
// SAME fields HandleMessage resolves the session with (NOT the
// taskqueue's per-agent accountID), so the pointer matches the running
// turn. `text` is the already-formatted body the Submit path would have
// delivered (e.g. the group `\[name\]:` prefix). Returns false when no
// turn is active so the caller falls back to taskQueue.Submit.
func (a *Agent) SteerInbound(msg bus.InboundMessage, text string) bool {
	sess := a.sessions.Get(sessionTriple(msg, msg.ProjectID))
	return sess.PushSteerIfActive(provider.Message{
		Role:      "user",
		Content:   text,
		Metadata:  senderMetadata(msg),
		Timestamp: time.Now().UnixMilli(),
	})
}

// recoverWebTriple maps a URL `?session=` token (which can be a
// session_key for any channel, OR a legacy web chat_id) to the full
// (channel, accountID, chatID, projectID) tuple downstream callers
// need.
//
// Without recovering accountID too, an inbound web write to a
// telegram/wechat session would query Manager.Get(channel, "", chatID),
// miss the existing row (which has account_id=<bot_id>), and mint a
// brand-new session under the wrong triple — the user sees the reply
// briefly, but a refresh loads the original session's history and the
// just-written exchange vanishes.
//
// projectID is "" for loose chats and forwarded onto the inbound
// message so bindSession routes the sandbox + workspace.Store to the
// project folder.
//
// Two-step recovery:
//  1. If the token matches a session_key → look up the full triple +
//     project.
//  2. Otherwise treat it as a web chat_id (preserves the brand-new
//     "+New chat" path where the row doesn't exist yet).
func (a *Agent) recoverWebTriple(sessionId string) (channel, accountID, chatID, projectID string) {
	channel, accountID, chatID = "web", "", sessionId
	if !a.sessions.SessionExists(sessionId) {
		return
	}
	if c, acc, ci, err := a.sessions.LookupSessionTriple(sessionId); err == nil && (c != "" || ci != "") {
		channel = c
		if channel == "" {
			channel = "web"
		}
		if ci != "" {
			chatID = ci
		}
		accountID = acc
	}
	projectID = a.sessions.LookupSessionProject(sessionId)
	return
}

// home returns the agent's home (metadata) directory path.
func (a *Agent) home() string {
	return a.homePath
}

// SetGroupContext configures group chat awareness for this agent's system prompt.
func (a *Agent) SetGroupContext(gc *GroupContext) {
	a.ctxBuilder.SetGroupContext(gc)
}

// InjectGroupMessage appends a message from another bot into the session history
// without triggering an LLM call. This gives the agent awareness of what other
// bots said in the group chat.
//
// The `\[name\]:` prefix escapes the brackets so the web UI's CommonMark
// renderer doesn't read short single-token messages (e.g. `[idoubi]: hello`)
// as a link reference definition and silently swallow them. The LLM still
// reads it as a bracketed sender label — the backslash escapes are well-
// understood markdown source.
func (a *Agent) InjectGroupMessage(ctx context.Context, msg bus.InboundMessage) {
	sess := a.sessions.Get(sessionTriple(msg, msg.ProjectID))
	label := msg.SenderName
	if label == "" {
		label = "Bot"
	}
	content := fmt.Sprintf("\\[%s\\]: %s", label, msg.Text)
	sess.Append(provider.Message{
		Role:     "user",
		Content:  content,
		Metadata: senderMetadata(msg),
	})
}

// SetSubAgentSpawner sets the sub-agent spawner for the spawn_subagent tool.
func (a *Agent) SetSubAgentSpawner(spawner tools.SubAgentSpawner) {
	a.subAgentSpawner = spawner
	tools.RegisterSubAgent(a.registry, spawner, a.name)
}

// ToolRegistry returns the agent's tool registry for external registration.
func (a *Agent) ToolRegistry() *tools.Registry {
	return a.registry
}

// SetOwnerUserID tags this agent with the owning user ID. The value is
// propagated into every HookContext so plugins like mem0 can namespace
// data per user.
func (a *Agent) SetOwnerUserID(uid string) {
	a.ownerUserID = uid
}

// OwnerUserID returns the agent's owning user ID — the user that
// created / owns this agent. Exposed so callers that mint records
// on the user's behalf (e.g. /goal slash) can stamp ownership
// without reaching into agent internals.
func (a *Agent) OwnerUserID() string { return a.ownerUserID }

// SetMeter wires the admin token meter onto this agent. Called by the
// gateway at boot / hot-reload so every Chat call lands a RecordTokens
// invocation. Nil is fine — meterTokens() is a no-op when unset.
func (a *Agent) SetMeter(m usage.Meter) { a.meter = m }

// SetQuotaStore wires the billing quota store. Called by the gateway at
// boot / hot-reload alongside SetMeter. Nil disables quota enforcement.
func (a *Agent) SetQuotaStore(qs usage.QuotaStore) { a.quotaStore = qs }

// checkQuota returns a non-empty rejection message when the agent's
// owner has exceeded their billing quota. Returns "" when the request
// should proceed (no quota, unlimited, or still under limit).
func (a *Agent) checkQuota(ctx context.Context) string {
	if a.quotaStore == nil || a.meter == nil {
		return ""
	}
	status, err := usage.CheckQuota(ctx, a.quotaStore, a.meter, a.ownerUserID)
	if err != nil || status.Allowed {
		return ""
	}
	return fmt.Sprintf("Sorry, your usage quota has been exceeded (used %d/%d tokens, %d/%d requests). Your quota resets on %s. Please contact your service provider to upgrade your plan.",
		status.TokensUsed, status.MonthlyTokenLimit,
		status.RequestsUsed, status.MonthlyRequestLimit,
		status.ResetsAt)
}

// meterTokens records one Chat call's token counts. Safe to call with
// zero usage (still bumps request_count). Errors are logged but never
// propagated — metering must not break the chat path. The agent's
// configured model string carries the provider prefix when a per-agent
// override is set; we split it so the meter stores provider and model
// in their own columns rather than mashing them together.
// durationMs is the wall-clock time of the LLM call; pass 0 when not
// measured (the daily bucket doesn't use it, only the log table).
func (a *Agent) meterTokens(ctx context.Context, sessionKey string, u provider.Usage, durationMs int64) {
	if a.meter == nil {
		return
	}
	prov, mdl := provider.SplitProviderModel(a.model)
	t := usage.Tokens{
		Input:         u.InputTokens,
		Output:        u.OutputTokens,
		CacheRead:     u.CacheReadTokens,
		CacheCreation: u.CacheCreationTokens,
	}
	if err := a.meter.RecordTokens(ctx, a.ownerUserID, a.agentID, sessionKey, prov, mdl, t); err != nil {
		slog.Warn("meter record failed", "agent", a.name, "error", err)
	}
	if err := a.meter.RecordTokenLog(ctx, a.ownerUserID, a.agentID, sessionKey, prov, mdl, t, durationMs); err != nil {
		slog.Warn("meter log failed", "agent", a.name, "error", err)
	}
}

// streamChatToResponse is a drop-in replacement for provider.Chat that
// pipes text chunks to the chat-event channel in real time via
// content_delta events. The web UI subscriber appends each delta to
// the in-flight assistant bubble so users see the answer materialize
// token-by-token instead of waiting for the whole ReAct loop to
// finish.
//
// Tool-calls / thinking / RawAssistant / Usage are extracted from the
// final (Done=true) chunk so the returned *provider.Response matches
// what provider.Chat would have produced — the caller's downstream
// logic (HasToolCalls check, session.Append with thinking, meterTokens)
// doesn't have to change.
//
// Use this at every site that previously called provider.Chat in the
// HandleMessage path. Providers that don't actually stream still work
// — they just deliver one big chunk on Done.
func (a *Agent) streamChatToResponse(ctx context.Context, messages []provider.Message, tools []provider.Tool) (*provider.Response, error) {
	return a.streamChatToResponseWithOptions(ctx, messages, tools, true)
}

func (a *Agent) streamChatToResponseQuiet(ctx context.Context, messages []provider.Message, tools []provider.Tool) (*provider.Response, error) {
	return a.streamChatToResponseWithOptions(ctx, messages, tools, false)
}

// Complete is a one-shot background LLM call for system-level text generation
// (conversation summaries, diagnostic reports): no tools, no streaming chat
// events, thinking disabled so reasoning tokens don't eat the output budget.
// It goes straight to provider.Chat (not streamChatToResponse) precisely so
// it does NOT record an llm_call_diag row — a report-generation call must not
// pollute the failure trail it's reporting on.
func (a *Agent) Complete(ctx context.Context, messages []provider.Message, maxTokens int) (string, error) {
	ctx = provider.WithNoThinking(ctx)
	if maxTokens <= 0 {
		maxTokens = a.maxTokens
	}
	resp, err := a.provider.Chat(ctx, messages, nil, a.model, maxTokens, a.temperature)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// firstTokenStallError marks a stream that produced zero chunks within the
// first-token deadline — the request WAS accepted (headers 200, SSE open)
// but the provider never started generating. Distinct from
// context.Canceled on purpose: a user Stop must end the turn silently,
// while a stall means the caller is still waiting and llmRetry should
// back off and re-send (nothing was delivered, so a retry is safe).
// sand (Cursor Grok Bot) carries the same shape as FirstTokenStallError.
type firstTokenStallError struct{ waited time.Duration }

func (e *firstTokenStallError) Error() string {
	return fmt.Sprintf("provider stream produced no first token within %s (first-token stall watchdog)", e.waited)
}

// firstTokenStallDeadline bounds how long a streaming LLM call may sit with
// an open stream and zero chunks. ResponseHeaderTimeout (120s, provider.go)
// already bounds "connected but no headers"; this bounds the next wedge —
// headers fine, SSE open, then silence, which previously blocked Next()
// forever and serialized the whole per-chat queue behind the dead request.
// Overridable via FLUCTIO_FIRST_TOKEN_STALL_MS (min 1000ms; sand uses the
// same 150s default via SAND_FIRST_TOKEN_STALL_DEADLINE_MS). A var so
// tests can shrink it.
var firstTokenStallDeadline = resolveFirstTokenStallDeadline()

func resolveFirstTokenStallDeadline() time.Duration {
	if v := strings.TrimSpace(os.Getenv("FLUCTIO_FIRST_TOKEN_STALL_MS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1000 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 150 * time.Second
}

// firstTokenWatchdog arms the stall timer around one streaming call. mark()
// is called on the first received chunk (timer stops, watchdog disarmed);
// if the deadline passes with no chunk, the callback cancels the
// stream-scoped ctx — never the turn ctx — so the producer goroutine's
// body read errors out, the chunk channel closes, and the consumer's
// stallFired check surfaces firstTokenStallError instead of the
// context.Canceled the cancel would otherwise masquerade as.
type firstTokenWatchdog struct {
	got   atomic.Bool
	fired atomic.Bool
	timer *time.Timer
}

func newFirstTokenWatchdog(deadline time.Duration, cancel context.CancelFunc) *firstTokenWatchdog {
	w := &firstTokenWatchdog{}
	w.timer = time.AfterFunc(deadline, func() {
		if !w.got.Load() {
			w.fired.Store(true)
			cancel()
		}
	})
	return w
}

// mark records the first chunk and disarms the timer. Safe to call on
// every chunk (only the first transition matters).
func (w *firstTokenWatchdog) mark() {
	w.got.Store(true)
	w.timer.Stop()
}

func (w *firstTokenWatchdog) stop() { w.timer.Stop() }

func (a *Agent) streamChatToResponseWithOptions(ctx context.Context, messages []provider.Message, tools []provider.Tool, emitDeltas bool) (*provider.Response, error) {
	start := time.Now()
	msgCount := len(messages)
	hasImg := requestHasImage(messages)
	// Stream-scoped cancel so the stall watchdog (and only it) can abort
	// the dead stream without touching the caller's turn ctx.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	sr, err := a.provider.ChatStream(streamCtx, messages, tools, a.model, a.maxTokens, a.temperature)
	if err != nil {
		a.recordLLMCallDiag(ctx, time.Since(start), llmDiagInfo{
			status: classifyCallError(err), httpStatus: extractHTTPStatus(err),
			errMsg: err.Error(), msgCount: msgCount, hasImage: hasImg,
		})
		return nil, err
	}
	watchdog := newFirstTokenWatchdog(firstTokenStallDeadline, cancelStream)
	defer watchdog.stop()
	var (
		contentBuilder strings.Builder
		toolCalls      []provider.ToolCall
		thinking       string
		thinkingSig    string
		rawAssistant   json.RawMessage
		streamUsage    provider.Usage
	)
	for {
		chunk, ok := sr.Next()
		if !ok {
			break
		}
		watchdog.mark()
		if chunk.Content != "" {
			contentBuilder.WriteString(chunk.Content)
			if emitDeltas {
				// Push the incremental delta. The web chat panel
				// appends it to the bubble in progress; consumers
				// that only know about the legacy `content` event
				// ignore unknown types and rely on the final
				// emit (caller's responsibility) instead.
				emitEvent(ctx, ChatEvent{
					Type: "content_delta",
					Data: map[string]any{"delta": chunk.Content},
				})
			}
		}
		if chunk.Done {
			toolCalls = chunk.ToolCalls
			if chunk.Thinking != "" {
				thinking = chunk.Thinking
			}
			if chunk.ThinkingSignature != "" {
				thinkingSig = chunk.ThinkingSignature
			}
			if len(chunk.RawAssistant) > 0 {
				rawAssistant = chunk.RawAssistant
			}
			if chunk.Usage.InputTokens > 0 || chunk.Usage.OutputTokens > 0 ||
				chunk.Usage.CacheReadTokens > 0 || chunk.Usage.CacheCreationTokens > 0 {
				streamUsage = chunk.Usage
			}
		}
	}
	if stallFired := watchdog.fired.Load(); stallFired {
		// Override BEFORE the sr.Err() branch below: the watchdog's cancel
		// makes the stream error read as context.Canceled, which the loop's
		// error path would silently swallow as a user Stop. A stall is not
		// a Stop — surface the distinct retryable error so llmRetry re-sends.
		a.recordLLMCallDiag(ctx, time.Since(start), llmDiagInfo{
			status: "first_token_stall", errMsg: "no first chunk within " + firstTokenStallDeadline.String(),
			msgCount: msgCount, hasImage: hasImg,
		})
		return nil, &firstTokenStallError{waited: firstTokenStallDeadline}
	}
	if err := sr.Err(); err != nil {
		a.recordLLMCallDiag(ctx, time.Since(start), llmDiagInfo{
			status: classifyCallError(err), errMsg: err.Error(),
			msgCount: msgCount, hasImage: hasImg,
		})
		return nil, err
	}
	// Mirror what AnthropicProvider.parseSSE does when no
	// RawAssistant was emitted but we still captured thinking text:
	// pack {thinking, signature} as a thinking content-block so the
	// next turn replays it correctly to extended-thinking models.
	if len(rawAssistant) == 0 && thinking != "" {
		if raw, err := json.Marshal(map[string]string{
			"type":      "thinking",
			"thinking":  thinking,
			"signature": thinkingSig,
		}); err == nil {
			rawAssistant = raw
		}
	}
	resp := &provider.Response{
		Content:      contentBuilder.String(),
		ToolCalls:    toolCalls,
		Thinking:     thinking,
		Usage:        streamUsage,
		RawAssistant: rawAssistant,
	}
	a.recordLLMCallDiag(ctx, time.Since(start), llmDiagInfo{
		status: "ok", resp: resp, msgCount: msgCount, hasImage: hasImg,
	})
	return resp, nil
}

// llmDiagInfo carries the outcome-specific fields the diagnostic recorder
// needs; common fields (agent/session/provider/model/duration) it derives
// itself from the agent + ctx.
type llmDiagInfo struct {
	status     string
	httpStatus int
	errMsg     string
	resp       *provider.Response
	msgCount   int
	hasImage   bool
}

// recordLLMCallDiag writes one llm_call_diag row, best-effort. Best-effort
// because diagnostics must never break the agent turn — a DB hiccup or a
// non-DBStore store is swallowed and the turn proceeds. sessionKey is pulled
// from the stream on ctx (the call sites don't receive it directly).
func (a *Agent) recordLLMCallDiag(ctx context.Context, dur time.Duration, info llmDiagInfo) {
	db, ok := a.dataStore.(*store.DBStore)
	if !ok || db == nil {
		return
	}
	prov, mdl := provider.SplitProviderModel(a.model)
	sessionKey := ""
	if s := streamFromContext(ctx); s != nil {
		sessionKey = s.sessionKey
	}
	rec := store.LLMCallDiag{
		AgentID:         a.agentID,
		SessionKey:      sessionKey,
		Provider:        prov,
		Model:           mdl,
		Status:          info.status,
		HTTPStatus:      info.httpStatus,
		ErrorMsg:        truncateRunes(info.errMsg, 500),
		DurationMs:      dur.Milliseconds(),
		RequestMsgCount: info.msgCount,
		HasImage:        info.hasImage,
	}
	if info.resp != nil {
		rec.ToolCallCount = len(info.resp.ToolCalls)
		rec.ResponseChars = len(info.resp.Content)
		rec.InputTokens = info.resp.Usage.InputTokens
		rec.OutputTokens = info.resp.Usage.OutputTokens
	}
	if err := db.RecordLLMCallDiag(ctx, rec); err != nil {
		slog.Warn("llm_call_diag record failed", "agent", a.name, "error", err)
	}
}

// classifyCallError maps a provider error to a coarse status for the
// diagnostic trail. context.Canceled (user stopped) and DeadlineExceeded
// (timeout) get their own buckets; everything else is a generic "error".
func classifyCallError(err error) string {
	var stall *firstTokenStallError
	if errors.As(err, &stall) {
		return "first_token_stall"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "error"
}

// extractHTTPStatus pulls the status code out of a provider.HTTPError if the
// error is one; 0 when it's a network-layer failure with no HTTP response.
func extractHTTPStatus(err error) int {
	var he *provider.HTTPError
	if errors.As(err, &he) {
		return he.StatusCode
	}
	return 0
}

// requestHasImage reports whether any message carries an image_url content
// part (multimodal input) — a fingerprint for attributing vision-path
// failures without storing the image itself.
func requestHasImage(messages []provider.Message) bool {
	for _, m := range messages {
		for _, p := range m.ContentParts {
			if p.Type == "image_url" {
				return true
			}
		}
	}
	return false
}

// truncateRunes caps a string to n runes so error_msg can't blow up the row.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// HookRegistry returns the agent's hook registry for external hook registration.
func (a *Agent) HookRegistry() *HookRegistry {
	return a.hooks
}

// Registry returns the agent's tool registry. Used by plugin wiring to
// register plugin-provided tools (RegisterPluginTools) onto the agent —
// the same role HookRegistry plays for hook plugins.
func (a *Agent) Registry() *tools.Registry {
	return a.registry
}

// WireGoals turns the /goal feature on for this Agent. Side effects:
//
//   - Stash the store on the agent.
//   - Register the AfterModelCall token-accounting hook (folds
//     Response.Usage into the active goal, flips budget_limited on
//     exhaust).
//   - Register the model-callable update_goal tool.
//   - Register a PostTurn hook that, when allowed, fires the next
//     continuation synchronously.
//
// Must be called after SetOwnerUserID so the registered tool and
// hook carry the right owner. Called by manager.buildAgent when a
// data store is available; nil store turns the feature off cleanly.
func (a *Agent) WireGoals(st goal.Store) {
	if st == nil {
		return
	}
	a.goalStore = st

	if hook := NewTokenAccountingHook(st, a.messageBus, a.name); hook != nil {
		a.hooks.Register(AfterModelCall, hook)
	}
	tools.RegisterGoalTools(a.registry, st, a.name)

	// Trigger continuation only at turn boundaries (PostTurn), not
	// mid-turn from AfterToolCall. AfterToolCall publishing
	// optimistically while a turn is still running opens a window
	// where the next continuation lands in bus.Inbound before a
	// concurrent /goal pause can; PostTurn closes that window.
	//
	// PostTurn fires for every source — we accept user (a real reply
	// or a /goal resume) and goal_context (chain the loop). Other
	// sources (cron, heartbeat, sub-agent) must NOT auto-continue or
	// we'd loop. The budget_limit wrap-up arrives as goal_context too,
	// but TryFireContinuation re-reads the goal status and bails on
	// non-Active goals, so a wrap-up turn doesn't cause a chain.
	a.hooks.Register(PostTurn, a.goalTriggerHook(allowedContinuationSources))
}

// allowedContinuationSources is the whitelist of bus sources that
// may auto-fire the next continuation from a PostTurn hook. User
// turns start / resume the loop; goal_context turns chain it.
var allowedContinuationSources = map[string]bool{
	bus.SourceUser:        true,
	bus.SourceGoalContext: true,
}

// goalTriggerHook builds a HookFunc that fires the next continuation
// for the in-flight session, when all gates pass.
func (a *Agent) goalTriggerHook(allowed map[string]bool) HookFunc {
	return func(ctx context.Context, hc *HookContext) {
		if !allowed[hc.Source] {
			return
		}
		if hc.IsPlanMode {
			return
		}
		if hc.GoalSessionKey == "" {
			return
		}
		if a.goalStore == nil {
			return
		}
		goal.TryFireContinuation(ctx, a.goalStore, a.messageBus, a.name, hc.GoalSessionKey)
	}
}

// sessionHasActiveGoal reports whether the session this inbound is
// for has a goal in Active state. Used as a hard precedence rule
// over auto-plan-mode: an active goal is an autonomous loop; plan-mode
// is a "wait for human approval" gate. The two cannot coexist on the
// same turn without breaking the goal's autonomy guarantee.
//
// Best-effort: a store error or missing session returns false. One
// indexed read per inbound turn — cheap enough to skip caching.
func (a *Agent) sessionHasActiveGoal(ctx context.Context, msg bus.InboundMessage) bool {
	if a.goalStore == nil || a.sessions == nil {
		return false
	}
	sess := a.sessions.Get(sessionTriple(msg, msg.ProjectID))
	if sess == nil {
		return false
	}
	g, err := a.goalStore.GetGoalBySession(ctx, a.name, sess.SessionKey())
	if err != nil || g == nil {
		return false
	}
	return g.Status == goal.StatusActive
}

// buildUserMessage flattens an inbound message into the user-role
// provider.Message that lands in session history. Tags Origin so
// goal-context continuations get recognized by the compaction /
// WebChatHistory / FTS filters (which check Origin != OriginUser),
// and merges PhotoURL (legacy IM single) + PhotoURLs (web multi)
// into one ContentParts slice. Image-only sends skip a leading
// empty text part — some upstreams reject content-less wire messages.
// cronTriggerGuidance is injected as a system message when a scheduled
// task fires, so the model treats the inbound directive as a task to
// execute and deliver — not a fresh user request to acknowledge. Pairs
// with the OriginCron tag on the user message for traceability.
const cronTriggerGuidance = "Scheduled-task trigger: the user message below is a task directive you previously set for yourself via create_cron_job. Execute it now and deliver the corresponding notification or result to the user directly. Do not acknowledge, confirm, or echo the directive, and do not treat it as a newly-requested task."

// sessionStartTime returns the timestamp of the session's first persisted
// message — used as the stable time anchor for BuildSystemPromptAs so the
// system prompt renders byte-identically across turns (prefix cache). Falls
// back to time.Now() before the first message is stored.
func sessionStartTime(sess *session.Session) time.Time {
	if hist := sess.GetMessages(); len(hist) > 0 && hist[0].Timestamp != 0 {
		return time.UnixMilli(hist[0].Timestamp)
	}
	return time.Now()
}

func buildUserMessage(msg bus.InboundMessage, modelID string) provider.Message {
	origin := provider.OriginUser
	switch msg.Source {
	case bus.SourceGoalContext:
		origin = provider.OriginGoalContext
	case bus.SourceCron:
		origin = provider.OriginCron
	}
	// IM DMs are not prefixed with `[SenderName]:` — there's only one
	// chatter per DM, the sender is already surfaced as a per-turn
	// system block when needed (see renderSender for the group case),
	// and putting an English-name bracket in front of every line biases
	// the model away from the language preferences set in SOUL.md
	// ("默认中文" loses to N copies of "[idoubicc]:" surrounding it).
	// Web has always been bare; this brings IM DMs in line.
	// Group fan-out still needs in-content tags so the model can tell
	// speakers apart across turns — routing.go pre-prefixes group
	// messages before queueing, so msg.Text already carries `[A]: …`
	// when PeerKind=="group". We pass it through unchanged.
	userText := msg.Text
	userMsg := provider.Message{
		Role:      "user",
		Content:   userText,
		Origin:    origin,
		Metadata:  senderMetadata(msg),
		Timestamp: time.Now().UnixMilli(),
	}
	// Prefer materialized local paths (no base64 truncation when the LLM
	// later routes a path through the vision tool); fall back to raw URLs
	// if persistence was skipped (older path / cloud store not wired).
	imageRefs := msg.ImagePaths
	if len(imageRefs) == 0 {
		imageRefs = msg.PhotoURLs
		if msg.PhotoURL != "" {
			imageRefs = append([]string{msg.PhotoURL}, imageRefs...)
		}
	}
	if len(imageRefs) == 0 {
		return userMsg
	}
	if meta, ok := config.LookupModelMeta(modelID); ok && meta.SupportsVision() {
		// Multimodal primary model: inline image_url parts, built from the
		// local files by us (complete bytes — the LLM never copies base64,
		// so no truncation). Raw http(s)/data refs pass through verbatim.
		var parts []provider.ContentPart
		if userText != "" {
			parts = append(parts, provider.ContentPart{Type: "text", Text: userText})
		}
		for _, ref := range imageRefs {
			url := ref
			if !looksLikeURL(ref) {
				du, err := tools.ReadImageAsDataURL(ref)
				if err != nil {
					slog.Warn("buildUserMessage: skip unreadable image", "path", ref, "error", err)
					continue
				}
				url = du
			}
			parts = append(parts, provider.ContentPart{
				Type: "image_url", ImageURL: &provider.ImageURL{URL: url, Detail: "auto"},
			})
		}
		if len(parts) == 0 {
			return userMsg
		}
		// The model sees the image bytes inline but not where they live on
		// disk; acting on the file later in the turn (vision fallback, note
		// attachments, file tools) needs the path — surface local paths the
		// same way the text-only branch does, minus the vision-lecture.
		if locals := localImagePaths(imageRefs); len(locals) > 0 {
			hint := buildImagePathHint(locals)
			if parts[0].Type == "text" && parts[0].Text != "" {
				parts[0].Text += "\n\n" + hint
			} else {
				parts = append([]provider.ContentPart{{Type: "text", Text: hint}}, parts...)
			}
		}
		userMsg.Content = ""
		userMsg.ContentParts = parts
	} else {
		// Text-only primary model: embed image refs as text so the agent
		// can pass them to the `vision` tool. No image_url block → the
		// model's endpoint won't 400 on an unsupported content type, and
		// the agent isn't tempted to truncate base64 into a tool arg.
		hint := buildImageRefHint(imageRefs)
		if userText == "" {
			userMsg.Content = hint
		} else {
			userMsg.Content = userText + "\n\n" + hint
		}
	}
	return userMsg
}

// localImagePaths filters refs down to materialized local files — URL/data
// refs have no on-disk path to surface.
func localImagePaths(refs []string) []string {
	var out []string
	for _, r := range refs {
		if !looksLikeURL(r) {
			out = append(out, r)
		}
	}
	return out
}

// buildImagePathHint is the multimodal counterpart of buildImageRefHint: the
// model already sees the image inline, but acting on the file later in the
// turn (vision fallback, note attachments, file tools) needs its path.
func buildImagePathHint(refs []string) string {
	if len(refs) == 1 {
		return fmt.Sprintf("（图片已保存到本地：%s。需要对该文件操作（如存入笔记附件）时使用此路径。）", refs[0])
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "（图片已保存到本地，共 %d 张；需要文件操作（如存入笔记附件）时使用以下路径：\n", len(refs))
	for _, r := range refs {
		fmt.Fprintf(&sb, "- %s\n", r)
	}
	sb.WriteString("）")
	return sb.String()
}

// buildImageRefHint renders the text-only-model fallback notice that lists
// materialized image paths for the agent to feed into the vision tool.
func buildImageRefHint(refs []string) string {
	var sb strings.Builder
	if len(refs) == 1 {
		sb.WriteString("用户上传了一张图片，但你（当前主模型）无法直接查看图片。如需识别图片内容，请调用 vision 工具，image 参数使用下面的路径：\n")
	} else {
		fmt.Fprintf(&sb, "用户上传了 %d 张图片，但你（当前主模型）无法直接查看图片。如需识别图片内容，请调用 vision 工具，image 参数依次使用下面的路径：\n", len(refs))
	}
	for _, r := range refs {
		fmt.Fprintf(&sb, "- %s\n", r)
	}
	return sb.String()
}

func looksLikeURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "data:")
}

// persistInboundImages downloads each inbound image (data: URL decoded,
// http(s) downloaded) into the session's uploads/ dir and records the local
// paths on msg.ImagePaths. Doing this at ingest — not in buildUserMessage —
// means the vision tool and the non-multimodal text-routing both get real
// file paths the LLM can pass without truncating base64, and multimodal
// models get image_url blocks built by us from complete bytes. Local-FS
// only for now (cloud workspaceStore wiring is a follow-up); on failure it
// silently leaves ImagePaths empty so buildUserMessage falls back to URLs.
func (a *Agent) persistInboundImages(msg *bus.InboundMessage) {
	urls := msg.PhotoURLs
	if msg.PhotoURL != "" {
		urls = append([]string{msg.PhotoURL}, urls...)
	}
	if len(urls) == 0 {
		return
	}
	home, err := config.HomeDir()
	if err != nil || home == "" {
		return
	}
	// Resolve the session_key (s-...) so uploads land in sessions/<sessionKey>/,
	// matching the workspace scope bindSession set — a /new doesn't carry
	// the previous session's uploads. sessions.Get is idempotent on the
	// triple, so the later Get in HandleMessage reuses this same session.
	sess := a.sessions.Get(sessionTriple(*msg, msg.ProjectID))
	dir := filepath.Join(home, "workspaces", a.name, inboundScopeDir(msg.ProjectID, sess.SessionKey()), "uploads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("persistInboundImages: mkdir", "dir", dir, "error", err)
		return
	}
	var paths []string
	for i, u := range urls {
		data, ext, err := fetchImageBytes(u)
		if err != nil {
			slog.Warn("persistInboundImages: fetch", "url", u, "error", err)
			continue
		}
		full := filepath.Join(dir, fmt.Sprintf("%d_%d%s", time.Now().UnixMilli(), i, ext))
		if err := os.WriteFile(full, data, 0o644); err != nil {
			slog.Warn("persistInboundImages: write", "path", full, "error", err)
			continue
		}
		paths = append(paths, full)
	}
	if len(paths) > 0 {
		msg.ImagePaths = paths
	}
}

// inboundScopeDir is the workspace sub-directory matching the turn's scope:
// projects/<pid> when project-scoped, else sessions/<sessionKey>. The
// session branch uses the session_key (s-...), NOT the channel chat_id,
// so a /new (fresh session_key) starts an empty uploads set too.
func inboundScopeDir(projectID, sessionKey string) string {
	if projectID != "" {
		return filepath.Join("projects", projectID)
	}
	return filepath.Join("sessions", sessionKey)
}

// fetchImageBytes resolves an image reference to bytes + extension. Accepts
// data: URLs (base64-decoded) and http(s) URLs (downloaded via the shared
// downloadMediaURL helper). ext includes the leading dot.
func fetchImageBytes(ref string) ([]byte, string, error) {
	if strings.HasPrefix(ref, "data:") {
		return decodeDataURL(ref)
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		b, err := downloadMediaURL(ref)
		if err != nil {
			return nil, "", err
		}
		return b, extFromMIME(http.DetectContentType(b)), nil
	}
	return nil, "", fmt.Errorf("unsupported image ref: %s", ref)
}

// decodeDataURL and extFromMIME are shared with the inbound-attachment
// path — see attachments.go. fetchImageBytes reuses them.

// RegisterWebSearchChain exposes the web_search tool to this agent using a
// provider chain (primary + fallbacks). Pass nil to skip — the tool won't
// appear in the agent's tool list, so the model can't try to call it.
func (a *Agent) RegisterWebSearchChain(chain *toolproviders.Chain) {
	tools.RegisterWebSearchChain(a.registry, chain)
}

// RegisterImageGenChain exposes the image_gen tool to this agent.
func (a *Agent) RegisterImageGenChain(chain *toolproviders.Chain) {
	tools.RegisterImageGenChain(a.registry, chain)
}

// RegisterWebFetchChain swaps the agent's web_fetch backend for a
// provider chain (e.g. direct → jina → firecrawl). Pass nil to keep the
// legacy direct-only fetcher already wired during agent construction.
func (a *Agent) RegisterWebFetchChain(chain *toolproviders.Chain) {
	tools.RegisterWebFetchChain(a.registry, chain)
}

// RegisterTTSChain exposes the tts tool to this agent.
func (a *Agent) RegisterTTSChain(chain *toolproviders.Chain) {
	tools.RegisterTTSChain(a.registry, chain)
}

// RegisterVisionChain exposes the vision (image understanding) tool to this
// agent, as a multimodal fallback for primary models that can't see images.
func (a *Agent) RegisterVisionChain(chain *toolproviders.Chain) {
	tools.RegisterVisionChain(a.registry, chain)
}

// Sessions returns the session manager for this agent.
func (a *Agent) Sessions() *session.Manager {
	return a.sessions
}

// WebChatHistory returns chat history for a specific session — the
// name is historical; it now serves any channel because the dashboard
// surfaces all-channel chats in the sidebar.
//
// Reads from the append-only session_messages archive (via
// Session.ArchivedMessages) instead of the in-memory working set, so
// post-compaction sessions show the original conversation rather than a
// summary + last 20 turns. Falls back to the working set when no
// archive is available (file-backed mode or pre-archive sessions).
//
// sessionId may be either a canonical session_key (what
// ListWebSessions returns) or a legacy web chat_id from older URLs;
// ResolveSessionKey untangles them.
// WebChatHistory returns one paginated slice of the session's archived
// messages for the web chat panel. beforeSeq > 0 returns messages with
// seq strictly less than it (older); beforeSeq <= 0 returns the most
// recent page. The page is sliced from the raw archived messages BEFORE
// the role/origin filter runs, so the seq cursor stays continuous even
// though filtered-out rows (goal_context, empty assistant) don't appear
// in the returned history. earliestSeq is the oldest seq in this page
// (0 if empty) — the client feeds it back as the next `before`. hasMore
// is true when older messages remain beyond this page.
func (a *Agent) WebChatHistory(sessionId string, beforeSeq, limit int) ([]map[string]any, int, bool) {
	if sessionId == "" {
		sessionId = "web-ui"
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	resolved := a.sessions.ResolveSessionKey(sessionId)
	sess := a.sessions.GetByKey(resolved)
	all := sess.ArchivedMessages() // ascending by seq
	// Window to messages strictly older than beforeSeq (most-recent page
	// when beforeSeq <= 0).
	end := len(all)
	if beforeSeq > 0 {
		for i, m := range all {
			if m.Seq >= beforeSeq {
				end = i
				break
			}
		}
	}
	window := all[:end]
	hasMore := false
	start := 0
	if len(window) > limit {
		start = len(window) - limit
		hasMore = true
	}
	page := window[start:]
	earliestSeq := 0
	if len(page) > 0 {
		earliestSeq = page[0].Seq
	}
	// A forked session prefixes its parent's archived [0..forkSeq] as a
	// read-only snapshot. The prefix lives outside B's own seq namespace,
	// so shift it to negative seqs (-(forkSeq+1)..-1) — keeps the merged
	// list totally ordered and lets the UI tell inherited turns (seq<0,
	// no fork affordance) from B's own (seq>=0). The prefix is never
	// paginated: it's a fixed snapshot shown in full, while B's own
	// archive pages normally.
	renderedPage := page
	if prefix := sess.ParentPrefixMessages(); len(prefix) > 0 {
		offset := -(sess.ParentForkSeq() + 1)
		shifted := make([]provider.Message, len(prefix))
		for i, m := range prefix {
			m.Seq = offset + i
			shifted[i] = m
		}
		renderedPage = append(shifted, page...)
	}
	var history []map[string]any
	for _, m := range renderedPage {
		// turn_aborted 边界标记（崩溃自愈 / 停止标记）：runtime 注入但
		// 用户可见 —— 在 Origin 过滤之前拦截，渲染成 notice entry（前端
		// 识别 kind="turn_aborted_notice"），不作为用户气泡。
		if m.Origin == provider.OriginTurnAbort {
			history = append(history, map[string]any{
				"role":      "user",
				"kind":      "turn_aborted_notice",
				"content":   m.TextContent(),
				"timestamp": m.Timestamp,
			})
			continue
		}
		// Hide runtime-injected messages (currently only goal_context
		// continuations). They live in the session for the LLM's
		// benefit; surfacing them to the user would expose audit
		// scaffolding the user never typed. Matches Codex's slash-only
		// /goal UX — the audit prompt is internal-only.
		if m.Origin != provider.OriginUser {
			continue
		}
		switch m.Role {
		case "user":
			// Multimodal user turns store text inside ContentParts and
			// leave Content empty (see HandleMessageStream's image
			// attachment path). Surface both shapes here:
			//   - text (Content fallback to joined text parts)
			//   - imageUrls (image_url parts) so the chat UI can render
			//     image thumbnails on bubbles loaded from history, not
			//     just on the live in-flight bubble.
			text := m.TextContent()
			var imageURLs []string
			for _, p := range m.ContentParts {
				if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL != "" {
					imageURLs = append(imageURLs, p.ImageURL.URL)
				}
			}
			// IM-routed turns store an "\[idoubi\]: hello" prefix on
			// Content so the LLM can attribute the line in group chats
			// when the system prompt rolls off. The web panel renders
			// the nickname separately from `senderName` metadata, so
			// strip the prefix from `text` here to keep the bubble body
			// clean. Cover both the escaped (post-fix) and unescaped
			// (legacy session rows) shapes.
			senderName, _ := m.Metadata["senderName"].(string)
			if senderName != "" {
				text = stripSenderPrefix(text, senderName)
			}
			if text == "" && len(imageURLs) == 0 {
				continue
			}
			entry := map[string]any{"role": "user", "content": text, "timestamp": m.Timestamp, "seq": m.Seq}
			if len(imageURLs) > 0 {
				entry["imageUrls"] = imageURLs
			}
			if senderName != "" {
				entry["senderName"] = senderName
				if v, ok := m.Metadata["senderAvatarUrl"].(string); ok && v != "" {
					entry["senderAvatarUrl"] = v
				}
				if v, ok := m.Metadata["senderId"].(string); ok && v != "" {
					entry["senderId"] = v
				}
				if v, ok := m.Metadata["senderChannel"].(string); ok && v != "" {
					entry["senderChannel"] = v
				}
			}
			history = append(history, entry)
		case "assistant":
			// 压缩提示单独渲染成 notice entry（前端识别
			// entry.kind="compaction_notice"）。必须放在常规 assistant
			// 处理之前，避免 notice 文案被当成正常回复。
			if _, ok := m.Metadata["compactionNotice"]; ok {
				meta, _ := m.Metadata["compactionNotice"].(map[string]any)
				entry := map[string]any{
					"role":      "assistant",
					"kind":      "compaction_notice",
					"content":   m.Content,
					"timestamp": m.Timestamp,
				}
				for k, v := range meta {
					entry[k] = v // before / after / retained_turns
				}
				history = append(history, entry)
				continue
			}
			entry := map[string]any{"role": "assistant", "timestamp": m.Timestamp, "seq": m.Seq}
			if m.Content != "" {
				entry["content"] = m.Content
			}
			if len(m.ToolCalls) > 0 {
				var calls []map[string]string
				for _, tc := range m.ToolCalls {
					calls = append(calls, map[string]string{
						"id":        tc.ID,
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					})
				}
				entry["toolCalls"] = calls
			}
			// Surface persisted assistant-side metadata so the UI can
			// re-render iteration-cap badges, etc. on history reload —
			// without this, the badge only ever showed on the live turn.
			if len(m.Metadata) > 0 {
				entry["metadata"] = m.Metadata
			}
			// Skip empty assistant messages (no content, no tool calls)
			if m.Content == "" && len(m.ToolCalls) == 0 {
				continue
			}
			history = append(history, entry)
		case "tool":
			entry := map[string]any{
				"role":       "tool",
				"content":    m.Content,
				"name":       m.Name,
				"toolCallId": m.ToolCallID,
				"timestamp":  m.Timestamp,
				"seq":        m.Seq,
			}
			if len(m.Metadata) > 0 {
				entry["metadata"] = m.Metadata
			}
			history = append(history, entry)
		}
	}
	return history, earliestSeq, hasMore
}

// WebChatSessions returns a list of web chat sessions with metadata.
func (a *Agent) WebChatSessions() []session.WebSession {
	return a.sessions.ListWebSessions()
}

// DeleteWebChatSession removes a chat session (any channel) by the URL
// token — accepts either session_key or legacy web chat_id.
func (a *Agent) DeleteWebChatSession(sessionId string) error {
	return a.sessions.DeleteSessionByID(sessionId)
}

// RenameWebChatSession sets a custom title for a chat session (any
// channel) by the URL token.
func (a *Agent) RenameWebChatSession(sessionId, title string) error {
	return a.sessions.RenameSessionByID(sessionId, title)
}

// MoveWebChatSession reassigns a chat to a different project (or
// detaches it when projectID is "") and migrates its workspace files
// from the old scope to the new one. Drives the sidebar drag-and-drop
// affordance.
//
// Order matters:
//  1. Resolve the URL token to the canonical session_key.
//  2. Read the current project_id so we know the source workspace
//     scope (loose chat = sessions/<sid>/, project chat =
//     projects/<oldPid>/<sid>/).
//  3. Release any live sandbox bound to this chat — leaving it up
//     would keep the old bind-mount referenced and the new mount
//     wouldn't take effect until eviction. Released proactively so
//     the next turn cold-starts at the new path.
//  4. Move workspace files (no-op when the source dir is empty).
//  5. Flip sessions.project_id in the store and drop the in-memory
//     Session cache so the next Get re-reads the row.
//
// Steps 4 and 5 are not atomic: a crash between them leaves the row
// pointing at the new project but files at the old path (or vice
// versa). The pending follow-up move is idempotent — re-running this
// method finishes the migration cleanly.
func (a *Agent) MoveWebChatSession(ctx context.Context, sessionId, projectID string) error {
	key := a.sessions.ResolveSessionKey(sessionId)
	if key == "" {
		return fmt.Errorf("session not found: %s", sessionId)
	}
	oldProject := a.sessions.LookupSessionProject(key)
	if oldProject == projectID {
		return nil
	}
	if a.sandboxPool != nil {
		if err := a.sandboxPool.Release(a.name, oldProject, key); err != nil {
			slog.Warn("MoveWebChatSession: sandbox release failed",
				"agent", a.name, "session", key, "error", err)
		}
	}
	if a.workspaceStore != nil {
		if err := a.workspaceStore.Move(ctx, a.name, oldProject, key, projectID, key); err != nil {
			return fmt.Errorf("workspace move: %w", err)
		}
	}
	return a.sessions.MoveSessionByID(sessionId, projectID)
}

// ForkSession creates a new chat session B forked from sourceSessionKey
// at forkSeq — B inherits the parent's session_messages archive
// [0..forkSeq] as a read-only prefix (merged into LLM context + history
// at read time, never copied) and copies the parent's project_id so the
// fork clusters beside its parent in the sidebar. B is a fresh web chat
// (chatID == its own session_key) so it resolves independently of A.
//
// Returns B's session_key; the frontend switches to it.
func (a *Agent) ForkSession(sourceSessionKey string, forkSeq int) (string, error) {
	key := a.sessions.ResolveSessionKey(sourceSessionKey)
	if key == "" {
		return "", fmt.Errorf("session not found: %s", sourceSessionKey)
	}
	projectID := a.sessions.LookupSessionProject(key)
	newKey := a.sessions.OpenForkSession("web", "", "", projectID, key, forkSeq)
	return newKey, nil
}

// Model returns the agent's model name.
func (a *Agent) Model() string {
	return a.model
}

// CostTracker returns the agent's cost tracker for usage/billing queries.
func (a *Agent) CostTracker() *costtracker.Tracker {
	return a.costTracker
}

// dumpLLMRequest appends the full LLM-bound payload to a dedicated file
// when FLUCTIO_DUMP_LLM is set. Default path is ~/.fluctio/logs/llm-dump.log
// (overridable via FLUCTIO_DUMP_LLM_FILE) — separate from gateway.log so
// the multi-thousand-line system prompt doesn't drown structured slog
// entries, and tail-able regardless of whether the gateway runs under air,
// daemon, or as a foreground process.
//
// Multi-line content is written as one block per turn (not per-line slog
// calls) so timestamps don't shred the system prompt.
func dumpLLMRequest(agentName, model string, messages []provider.Message, tools []provider.Tool) {
	if os.Getenv("FLUCTIO_DUMP_LLM") == "" {
		return
	}
	path := os.Getenv("FLUCTIO_DUMP_LLM_FILE")
	if path == "" {
		home := os.Getenv("FLUCTIO_HOME")
		if home == "" {
			if h, err := os.UserHomeDir(); err == nil {
				home = h + "/.fluctio"
			}
		}
		if home == "" {
			return
		}
		path = home + "/logs/llm-dump.log"
	}
	_ = os.MkdirAll(filepathDir(path), 0o755)

	var b strings.Builder
	fmt.Fprintf(&b, "\n=== LLM REQUEST  ts=%s  agent=%s  model=%s  messages=%d  tools=%d ===\n",
		time.Now().Format(time.RFC3339Nano), agentName, model, len(messages), len(tools))
	for i, m := range messages {
		fmt.Fprintf(&b, "--- msg[%d] role=%s ---\n", i, m.Role)
		// Prefer Content; fall back to ContentParts for multimodal turns
		// (image_url stubs keep logs readable instead of dumping data URLs).
		content := m.Content
		if content == "" && len(m.ContentParts) > 0 {
			var pb strings.Builder
			for _, p := range m.ContentParts {
				switch p.Type {
				case "text":
					pb.WriteString(p.Text)
				case "image_url":
					pb.WriteString("[image_url]")
				default:
					fmt.Fprintf(&pb, "[%s]", p.Type)
				}
				pb.WriteString("\n")
			}
			content = pb.String()
		}
		if content != "" {
			b.WriteString(content)
			if !strings.HasSuffix(content, "\n") {
				b.WriteString("\n")
			}
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "[tool_call name=%s args=%s]\n", tc.Function.Name, tc.Function.Arguments)
		}
	}
	if len(tools) > 0 {
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.Function.Name)
		}
		fmt.Fprintf(&b, "--- tools (%d) ---\n%s\n", len(tools), strings.Join(names, ", "))
	}
	b.WriteString("=== END LLM REQUEST ===\n")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		// Fall back to stderr so the dump isn't silently lost.
		fmt.Fprint(os.Stderr, b.String())
		return
	}
	defer f.Close()
	_, _ = f.WriteString(b.String())
}

// filepathDir is a tiny inline helper to dodge importing path/filepath
// just for one Dir() call in this single function.
func filepathDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

// renderClientParams turns the per-request `params` blob the API
// caller submitted into a system message that nudges the LLM to
// honor those values when calling tools. Returns "" when params is
// empty so we don't add a noise message every turn.
//
// Why a system message and not a binding into tool args:
//
//	v1 trades determinism for simplicity. Apps don't know which
//	tools the agent has — they just send a flat key/value blob, and
//	the agent owner's system prompt tells the LLM what to do with
//	each known key. LLMs are reliable at copying JSON-shaped values
//	verbatim into tool calls (the failure mode is "ignored", not
//	"corrupted"); a stronger forcing layer is a v2 problem.
//
// Output shape: a `## Client Parameters` section with the JSON
// pretty-printed in a fenced block, plus a one-liner reminding the
// model these are constraints. The header + fence are deliberate —
// LLMs honor structured params framed as a separate document
// section much more reliably than as inline prose.
func renderClientParams(params map[string]any) string {
	if len(params) == 0 {
		return ""
	}
	blob, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		return ""
	}
	// Minimal by design — one fact, no behavioural prose. Earlier
	// versions tried to nudge the model with "treat as constraints" /
	// "don't shell out" / "look at the skills section" and each one
	// opened a new literal-misread surface (the model treated `model`
	// as a directive to call that API, refused outright "no skill
	// matches", or did `ls Skills/` looking for a directory). How to
	// pick a tool / skill is the agent's regular job, fully covered
	// by the system prompt's skills section and any per-agent SOUL.md.
	// The only thing the system has to say here is "here is the data
	// the client sent" — anything more is noise.
	return "## Client Parameters\n\n" +
		"The user's client app submitted these parameters alongside " +
		"the message. Forward them to whichever tool / skill you call.\n\n" +
		"```json\n" + string(blob) + "\n```"
}

// stripSenderPrefix removes the leading "\[name\]: " (or unescaped
// "[name]: ") attribution wrapper that the agent loop injects on
// IM-routed user turns. Used by the web history rendering so the
// nickname can be surfaced via dedicated metadata and the bubble body
// no longer double-shows "[idoubi]: hello" alongside an avatar header.
// Returns the original string when no prefix matches.
func stripSenderPrefix(text, senderName string) string {
	if senderName == "" {
		return text
	}
	for _, p := range []string{
		"\\[" + senderName + "\\]: ",
		"[" + senderName + "]: ",
	} {
		if strings.HasPrefix(text, p) {
			return text[len(p):]
		}
	}
	return text
}

// senderMetadata extracts UI-only sender identity off an inbound IM
// message (Discord/Telegram/Slack/...) and returns a metadata map ready
// to attach to the persisted user-role Message. The web chat panel
// reads these fields back via WebChatHistory to render an avatar +
// nickname header on each bubble. Returns nil for web chats and any
// other caller that doesn't populate SenderName so we don't bloat
// session_messages rows with empty maps.
//
// The map is deliberately not Marshal()-strict — provider serializers
// ignore Message.Metadata, so anything we put here stays out of the
// LLM payload. The nickname is still funneled to the LLM via the
// `\[nickname\]: ` prefix on Message.Content (set by callers).
func senderMetadata(msg bus.InboundMessage) map[string]any {
	if msg.SenderName == "" {
		return nil
	}
	md := map[string]any{
		"senderName":    msg.SenderName,
		"senderChannel": msg.Channel,
	}
	if msg.UserID != "" {
		md["senderId"] = msg.UserID
	}
	if msg.SenderAvatarURL != "" {
		md["senderAvatarUrl"] = msg.SenderAvatarURL
	}
	return md
}

// logSystemPromptFingerprint emits one structured line per turn that
// proves what the LLM was *actually* told about skills. The refresh
// log up the call stack only proves the loader produced N skills; this
// confirms they survived the BuildSystemPromptAs assembly into the
// system message we're about to ship. Used to chase the "group chat
// doesn't see skills" report — diff this line between a DM turn and a
// group turn for the same agent and the divergence point becomes
// obvious.
func (a *Agent) logSystemPromptFingerprint(channel, chatID, userID, prompt string) {
	skillCount := strings.Count(prompt, "<skill name=")
	hasFeishu := strings.Contains(prompt, "feedback-to-feishu")
	// Per-chatter file presence — sized so we can tell at a glance
	// whether the chatter's USER.md / MEMORY.md actually reached the
	// model this turn. Zero on either means the section was omitted
	// (no row, empty content, or chatterUID didn't resolve). Match
	// against the canonical section header text used in context.go;
	// keep this in sync with that file or the diagnostic goes dark.
	hasUserMD := strings.Contains(prompt, "<current_chatter_profile")
	hasMemorySection := strings.Contains(prompt, "<chatter_long_term_memory")
	hasSoul := strings.Contains(prompt, "# SOUL.md")
	hasIdentity := strings.Contains(prompt, "# IDENTITY.md")
	// "Remembering things across conversations" is the chatbot-mode
	// instruction block telling the LLM it CAN persist via write_file.
	// If chatbot mode is misconfigured / not applied, this string
	// won't be in the prompt and the model defaults to "I have no
	// memory" reflexive replies.
	hasPersistenceInstr := strings.Contains(prompt, "Remembering things across conversations")
	mode := a.promptMode
	if mode == "" {
		mode = config.PromptModeAgent
	}
	slog.Info("system prompt assembled",
		"agent", a.name, "channel", channel, "chat_id", chatID, "user", userID,
		"mode", mode,
		"bytes", len(prompt),
		"skill_blocks", skillCount,
		"has_user_md", hasUserMD,
		"has_memory", hasMemorySection,
		"has_soul", hasSoul,
		"has_identity", hasIdentity,
		"has_persistence_instr", hasPersistenceInstr,
		"has_feedback_to_feishu", hasFeishu)
}

// renderChatbotPersistenceReminder returns a terse imperative system
// message reminding the LLM that in chatbot mode it has write_file /
// edit_file available and MUST use them to persist chatter info.
//
// Why a per-turn reminder rather than relying on the big "Remembering
// things across conversations" block in the chatbotInfo prompt:
// Sonnet 4.x in chatbot mode (no other tools, simple persona) reverts
// to a strong training prior of "I am an AI without persistent
// memory" — observed lying to chatters with phrasings like
// "跨对话我没有记忆" even when the system prompt explicitly grants
// the capability. Putting a short, imperative reminder right before
// the user turn buys recency weight that outranks the training
// prior in practice.
//
// userMD / memoryMD are the CURRENT contents of those files (the same
// data that's already in the system prompt under <current_chatter_profile>
// / <chatter_long_term_memory>). We inline them again here for two
// reasons: 1) recency weight — they're now the very last system message
// before the user turn, so the model can't miss them; 2) Sonnet was
// observed reading the earlier system-prompt copies as schema docs
// rather than chatter facts and denying knowledge in fresh sessions
// even with "Name: 狗子" present. Double-rendering is wasted tokens
// but observably wins.
//
// Empty for non-chatbot modes (no tool surface mismatch there).
func renderChatbotPersistenceReminder(mode, displayName, userMD, memoryMD string) string {
	if mode != config.PromptModeChatbot {
		return ""
	}
	var sb strings.Builder

	// Identity reinforcement. Sonnet 4.x ignores "Your name is X" lines
	// in the middle of the system prompt and reverts to "I am Claude"
	// for "你是谁" / "who are you" questions. Putting the identity in
	// the per-turn reminder (last system message before the user turn)
	// + framing it as a violation-warning seems to be what's required
	// to override the training prior in practice.
	if displayName != "" {
		sb.WriteString("## Your identity (per-turn anchor)\n\n")
		sb.WriteString(fmt.Sprintf("In this runtime you ARE **%s**. When a chatter asks \"你是谁\" / \"who are you\", introduce yourself as **%s** — never \"Claude\" or \"AI 助手\" / \"AI assistant\". Saying \"我是 Claude\" / \"I am Claude\" is a role violation; do not do it. IDENTITY.md / SOUL.md below may add personality / role detail on top of this name, but the name itself is %s.\n\n", displayName, displayName, displayName))
	}

	sb.WriteString("## Chatter context (load-bearing — re-read every turn)\n\n")

	if strings.TrimSpace(userMD) != "" {
		sb.WriteString("These are facts about the person you're talking to RIGHT NOW (from USER.md you've persisted). Quote them verbatim when asked \"我是谁\" / \"你记得我吗\":\n\n")
		sb.WriteString("```\n")
		sb.WriteString(strings.TrimSpace(userMD))
		sb.WriteString("\n```\n\n")
	} else {
		sb.WriteString("USER.md is empty — you do not yet know who this chatter is. When they share their name / role / preferences, you MUST call write_file('USER.md', ...) in the SAME turn so the next conversation has them.\n\n")
	}

	if strings.TrimSpace(memoryMD) != "" {
		sb.WriteString("Long-term facts you've recorded about this chatter (from MEMORY.md):\n\n")
		sb.WriteString("```\n")
		sb.WriteString(strings.TrimSpace(memoryMD))
		sb.WriteString("\n```\n\n")
	}

	sb.WriteString("## Persistence rules\n\n")
	sb.WriteString("- You have `write_file` and `edit_file` in your tools — USE them whenever you learn something worth remembering.\n")
	sb.WriteString("- Identity (name, role, preferences, location, what to call them) → `write_file('USER.md', ...)` or `edit_file('USER.md', ...)`. ALWAYS USER.md. Never MEMORY.md for these.\n")
	sb.WriteString("- Recurring topics / decisions / project facts to hold across sessions → `MEMORY.md`.\n")
	sb.WriteString("- **If MEMORY.md already contains identity-shaped content** (e.g. \"关于<name>\" with name / role / preferences mixed in), that's a prior mistake — when the chatter shares an identity update, MIGRATE the identity bits out of MEMORY.md into USER.md (write USER.md with the consolidated profile, then edit_file MEMORY.md to remove the identity bullets that just moved). Don't perpetuate the wrong structure by tacking on more identity in MEMORY.md.\n")
	sb.WriteString("- NEVER say \"我记住了\" / \"I'll remember\" without actually calling the tool. The text is a lie; the tool call is the truth.\n")
	sb.WriteString("- NEVER say \"我没有跨对话记忆\" / \"I have no cross-session memory\" — that is FALSE; USER.md and MEMORY.md persist forever once you write them.\n")
	sb.WriteString("- When asked \"你记住我了吗\" / \"我是谁\", READ the USER.md block above this message. If it has content, the answer is yes — quote the name. If it's empty, the answer is \"not yet — tell me\" and then write whatever they say.")

	return sb.String()
}

// renderChannelHints emits per-turn protocol notes that the LLM can
// only honor if it knows about them. Today there's exactly one: IM
// channels with a single-message-per-bubble UI accept the
// channels.SplitMessageMarker token as "split this reply into multiple
// bubbles." The marker constant is colocated with the splitter in
// internal/channels/base.go so changing the wire token only touches
// one place; the actual split happens in the channels manager's
// dispatcher, uniformly across all IM adapters.
//
// `splitEnabled` is the per-agent toggle. When false (the default) we
// skip the hint so the LLM never learns the marker — and the dispatcher
// collapses any stray marker back to a newline. The two branches must
// stay in lockstep.
//
// Returns "" for non-IM channels (web, api) so they don't waste tokens
// on a hint the chatter wouldn't perceive — web renders one bubble per
// chat-message anyway.
func renderChannelHints(msg bus.InboundMessage, splitEnabled bool) string {
	if !splitEnabled || !isIMChannel(msg.Channel) {
		return ""
	}
	// Sample alone is enough — the LLM picks up the protocol from one
	// well-formed example without us listing every rule.
	return "## Reply Format\n\n" +
		"This channel renders one chat bubble per message. To split your " +
		"reply into separate bubbles, write `" + channels.SplitMessageMarker +
		"` on its own line between the parts. Each part is sent as a " +
		"distinct message in order.\n\n" +
		"Use this when a short, conversational, multi-beat reply reads more " +
		"naturally than one long block (e.g. \"好。\\n" + channels.SplitMessageMarker +
		"\\n第一条先到了。\\n" + channels.SplitMessageMarker + "\\n第二条在这。\"). " +
		"For a single coherent answer, just reply normally — no marker needed."
}

// isIMChannel returns true for channels with single-message-per-bubble
// UX where splitting one logical reply into multiple sequential
// messages reads naturally. Web/API channels render long replies in
// place — splitting there adds nothing.
func isIMChannel(channel string) bool {
	switch channel {
	case "wechat", "qq", "telegram", "discord", "slack", "line", "feishu":
		return true
	}
	return false
}

// renderSender emits a per-turn system block naming who the message
// came from on the originating IM channel. Used for GROUP messages so
// the LLM can attribute each turn to the right speaker.
//
// Skipped for DMs: there's only one chatter per DM, their identity is
// stable across the session and already captured in USER.md /
// per-chatter MEMORY. Repeating it as a per-turn English system block
// just adds language bias (SOUL.md's "默认中文" loses to N copies of
// "The latest user turn was sent by:…" surrounding it) without telling
// the LLM anything new. Web chats also don't get this block, so DM
// behavior now matches web.
//
// Returns "" for web chats and any other caller that doesn't populate
// SenderName, so we don't waste tokens.
func renderSender(msg bus.InboundMessage) string {
	if msg.SenderName == "" {
		return ""
	}
	if msg.PeerKind != "group" {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Current Sender\n\nThe latest user turn was sent by:\n")
	fmt.Fprintf(&b, "- channel: %s\n", msg.Channel)
	fmt.Fprintf(&b, "- username: %s\n", msg.SenderName)
	if msg.UserID != "" {
		fmt.Fprintf(&b, "- user_id: %s\n", msg.UserID)
	}
	if msg.PeerKind != "" {
		fmt.Fprintf(&b, "- peer_kind: %s\n", msg.PeerKind)
	}
	return b.String()
}

// isPlanMode reports whether the inbound message asked for plan-only
// output (no tool calls, just a numbered plan the user reviews before
// authorizing real work). Truthy values: bool true, string "true"/"1",
// any non-zero number. The frontend posts `params: {planMode: true}`.
func isPlanMode(params map[string]any) bool {
	v, ok := params["planMode"]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	case float64:
		return t != 0
	case int:
		return t != 0
	}
	return false
}

// planModeNudge is the system message we prepend on plan-mode turns.
// Spells out the contract: tools are server-side disabled THIS turn so
// don't attempt them; they WILL be available on the next turn when the
// user says "go" — so reference tool names by name in the plan when a
// step needs one. Earlier drafts only said "tools are disabled" without
// the "but they exist for execution" half, and the model dutifully
// wrote plans that didn't reference any tools (including delegate_task,
// which is exactly the tool we wrote to make these plans work). The
// model also gets a tool catalog injected as a separate system message
// so it has the full surface to reference, not just whatever it
// remembers from the global system prompt.
func planModeNudge() string {
	return "# PLAN MODE — output a plan only\n\n" +
		"The user has switched on plan mode for this message. They want " +
		"to see what you intend to do BEFORE any real work happens.\n\n" +
		"Tools are DISABLED for this response only — do not attempt to call " +
		"any tool, it will fail. They WILL be available on the next turn " +
		"when the user replies (the available set is listed in the tool " +
		"catalog system message). Reference tool names by name in the " +
		"plan so the execution turn knows what you intend to invoke at " +
		"each step.\n\n" +
		"For multi-chunk fan-out work (find N leads in K categories, " +
		"summarize each of M docs, draft P emails, etc.) explicitly plan " +
		"to use `delegate_task` and write out the per-call task scope. " +
		"That's the only way the execution turn stays inside its " +
		"iteration budget; trying to do all of it directly will burn the " +
		"cap on exploration and never reach synthesis.\n\n" +
		"Your VERY FIRST execution action (next turn) should be " +
		"`write_file('todo.md', <plan as - [ ] items>)` so the user sees " +
		"a live progress panel as you work. Mention this in the plan as " +
		"an explicit Step 0 (or fold it into Step 1) — the UI requires " +
		"the file to render anything.\n\n" +
		"Output a numbered plan with 3-7 steps. Each step is one or two " +
		"sentences describing the action plus the tool you'll use, e.g. " +
		"\"Step 3: Use `delegate_task` to find 10 solo insurance agents in " +
		"the US Sun Belt — owner-operated, mobile-phone preferred. " +
		"Expected output: a markdown table.\". Group related micro-" +
		"actions into a single step — a plan is a roadmap, not a " +
		"transcript.\n\n" +
		"End with exactly one line: \"Reply with 'go' to execute, or " +
		"tell me what to change.\"\n\n" +
		"Do not start the work. Do not apologize for needing a plan. " +
		"Just the plan."
}

// buildToolCatalogForPlan builds a compact "what tools are available
// for the execution turn" reference, injected as its own system message
// during plan mode. We pass tools=nil to the LLM in plan mode so the
// model can't accidentally call any — but that also means the model
// can't *see* the tool registry at all, which empirically caused it to
// write plans that omitted delegate_task entirely (it didn't know the
// tool existed). The catalog brings that knowledge back as plain text
// without surfacing a callable schema.
//
// Format: name + first-sentence summary, one per line. Truncate long
// descriptions hard — the model only needs enough to decide whether
// the tool fits a plan step, not enough to construct the call.
func buildToolCatalogForPlan(toolDefs []provider.Tool) string {
	if len(toolDefs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Tool catalog (reference only — tools are disabled THIS turn, available next turn)\n\n")
	b.WriteString("When your plan needs one of these, name it explicitly in the relevant step.\n\n")
	for _, t := range toolDefs {
		name := t.Function.Name
		desc := strings.TrimSpace(t.Function.Description)
		// First sentence only — keep the catalog scannable. Fall back to
		// the first 160 chars if no period is found (some tool descs are
		// run-on paragraphs).
		if idx := strings.IndexAny(desc, ".\n"); idx > 0 && idx < 200 {
			desc = strings.TrimSpace(desc[:idx])
		} else if len(desc) > 200 {
			desc = strings.TrimSpace(desc[:200]) + "…"
		}
		fmt.Fprintf(&b, "- `%s` — %s\n", name, desc)
	}
	return b.String()
}

// handlePlanMode is the single-shot plan-only path: store the user
// message, ask the model for a plan with tools disabled, persist + emit
// the response with planMode metadata so the UI can badge the bubble.
// No iteration loop, no cap, no tool execution. On the next turn (sent
// without the planMode flag) the regular HandleMessage path executes
// against the full session including this plan.
func (a *Agent) handlePlanMode(ctx context.Context, msg bus.InboundMessage) string {
	chatterUID := a.chatterUserID(msg)
	ctx = store.WithChannel(ctx, msg.Channel)
	sess := a.sessions.Get(sessionTriple(msg, msg.ProjectID))
	// Session.ctx() builds its OWN context from session-held fields
	// rather than inheriting the caller's ctx — without binding the
	// chatter onto sess itself, the WithChatterUserID we just stamped
	// above never reaches AppendSessionMessage / SaveSession and the
	// chatter_user_id column stays empty.
	{
		prov, mdl := provider.SplitProviderModel(a.model)
		sess.SetProviderModel(prov, mdl)
	}
	// Steering during plan drafting: plan mode has no ReAct loop to drain
	// into, so a mid-draft steer is parked in history and answered on
	// the user's next turn — which matches the plan-mode contract
	// (review the plan, then reply to execute).
	sess.BeginTurn()
	defer a.flushLeftoverSteer(sess)
	defer sess.PadOrphanToolResultsAndMarkAborted(session.ToolResultStoppedNote)

	// Mirror the regular path's user-message construction so multimodal
	// + IM-bridge payloads (PhotoURL / PhotoURLs) land in session
	// history the same way they would on a non-plan turn.
	userMsg := buildUserMessage(msg, a.model)
	sess.Append(userMsg)

	if a.provider == nil {
		noProviderMsg := "Agent is not configured with a usable LLM provider. Check that cfg.Providers contains the prefix referenced by model `" + a.model + "`."
		emitEvent(ctx, ChatEvent{Type: "error", Data: map[string]any{"message": noProviderMsg}})
		emitEvent(ctx, ChatEvent{Type: "done"})
		return noProviderMsg
	}

	systemPrompt := a.ctxBuilder.BuildSystemPromptAs(chatterUID, a.memory, sessionStartTime(sess))
	a.logSystemPromptFingerprint(msg.Channel, msg.ChatID, chatterUID, systemPrompt)
	// Tool catalog injection: plan mode passes tools=nil to the LLM so
	// it can't accidentally call anything, but that also hides the
	// registry from the planning model. Without this, plans were written
	// as if delegate_task / web_search / camoufox-cli didn't exist —
	// which defeated the whole point of having Plan mode set up fan-out
	// work for the execution turn.
	toolDefs := a.registry.DefinitionsForMode(builtinAllowForMode(a.promptMode))
	catalog := buildToolCatalogForPlan(toolDefs)
	messages := []provider.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "system", Content: planModeNudge()},
	}
	if catalog != "" {
		messages = append(messages, provider.Message{Role: "system", Content: catalog})
	}
	sessionMsgs := sess.GetMessages()
	if prefix := sess.ParentPrefixMessages(); len(prefix) > 0 {
		sessionMsgs = append(prefix, sessionMsgs...)
	}
	if prefix := sess.ParentPrefixMessages(); len(prefix) > 0 {
		sessionMsgs = append(prefix, sessionMsgs...)
	}
	messages = append(messages, withConversationGapContext(sessionMsgs)...)
	if a.piiScrubEnabled {
		messages = privacy.ScrubMessages(messages, privacy.Options{Entropy: a.piiEntropyEnabled})
	}

	resp, err := a.streamChatToResponse(ctx, messages, nil)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Info("plan-mode chat canceled", "agent", a.name)
			emitEvent(ctx, ChatEvent{Type: "done"})
			return ""
		}
		slog.Error("plan-mode chat failed", "agent", a.name, "error", err)
		emitEvent(ctx, ChatEvent{Type: "error", Data: map[string]any{"message": err.Error()}})
		emitEvent(ctx, ChatEvent{Type: "done"})
		return slashT(msg.Lang, "error.plan_failed")
	}
	a.meterTokens(ctx, sess.Key(), resp.Usage, 0)

	planMeta := map[string]any{"planMode": true}
	sess.Append(provider.Message{
		Role:         "assistant",
		Content:      resp.Content,
		Thinking:     resp.Thinking,
		Metadata:     planMeta,
		Timestamp:    time.Now().UnixMilli(),
		RawAssistant: resp.RawAssistant,
	})
	emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{
		"content":  resp.Content,
		"metadata": planMeta,
	}})
	emitEvent(ctx, ChatEvent{Type: "done"})
	return resp.Content
}

// appendSteer folds drained steer messages into the running turn: each
// is persisted to the session, added to the live LLM message slice, and
// echoed as a "steer" event so the web UI renders it as a user bubble
// (persisted → late-join backfill + seq-dedup work for free).
func (a *Agent) appendSteer(ctx context.Context, sess *session.Session, messages []provider.Message, steer []provider.Message) []provider.Message {
	for _, sm := range steer {
		// Approval replies (/yes or /no) tapped mid-stream: drain the
		// now-authorized (or cleared) pending calls immediately so the
		// user doesn't wait for the turn to end. Without this the steer
		// would just fold "/yes" as a user message and the parked exec
		// would never run this turn (flushLeftoverSteer only parks).
		if c := strings.TrimSpace(sm.Content); c == "/yes" || c == "/no" {
			if n := a.applyAuthReplySteer(ctx, sess, &messages, c == "/yes"); n > 0 {
				slog.Info("approval steer drained pending mid-turn",
					"agent", a.name, "approved", c == "/yes", "count", n)
			}
		}
		sess.Append(sm)
		messages = append(messages, sm)
		emitEvent(ctx, ChatEvent{Type: "steer", Data: map[string]any{"content": sm.Content}})
		slog.Info("steer message folded into running turn", "agent", a.name)
	}
	return messages
}

// flushLeftoverSteer handles the end-of-turn race: a steer accepted by
// PushSteerIfActive after the loop's last drain but before the turn was
// declared done (realistically only the max-iteration synthesis call,
// an errored turn, or a sub-millisecond window — the between-rounds and
// pre-done drains cover every normal path). It's persisted to history
// so it isn't lost and rides the next turn's context; we deliberately
// do NOT re-run a hidden turn for it (kept simple + avoids the
// IM-has-no-reply asymmetry of a recursive redispatch).
func (a *Agent) flushLeftoverSteer(sess *session.Session) {
	leftover := sess.EndTurn()
	for _, m := range leftover {
		sess.Append(m)
	}
	if len(leftover) > 0 {
		slog.Warn("steer arrived at end of turn; parked in history for the next turn",
			"agent", a.name, "count", len(leftover))
	}
}

// ErrContextTooLong is a sentinel returned by llmRetry when the provider
// rejected the request because the prompt exceeded the model's context
// window (HTTP 400 with a "too long" / "context length" body, or 413).
// It is NOT a transient failure — retrying the same payload is pointless,
// so llmRetry returns it immediately instead of burning retry attempts.
// The caller (HandleMessage / HandleMessageStream) recognises it and
// triggers an in-place compaction + resend, mirroring Hermes' restart_
// with_compressed_messages recovery path. Borrowed from the Hermes agent
// error-classifier taxonomy, which routes context_length errors to
// "compress" rather than "retry".
var ErrContextTooLong = errors.New("context length exceeded")

// classifyLLMError maps a provider error to a coarse recovery category,
// the same idea as Hermes' classify_api_error. The returned category drives
// llmRetry's decision: "terminal" (don't retry — billing/auth dead-ends,
// client cancellation), "context_length" (don't retry the same payload —
// the caller must compress first), or "retryable" (transient — backoff and
// retry). Keeping this in the agent layer (not the provider layer) matches
// the existing classifyCallError / classifyToolError helpers and avoids any
// change to provider.HTTPError's shape.
//
// Status-code mapping follows the Hermes FailoverReason taxonomy:
//   - 401/403 (non-transient) → terminal     (auth dead-end; refresh is a provider concern)
//   - 402                     → terminal     (billing exhausted; retrying wastes a call)
//   - 400 w/ "too long" body  → context_length
//   - 413                     → context_length
//   - 408/409/425/429         → retryable
//   - 5xx                     → retryable
//   - other 4xx (non-PTL)     → retryable    (conservative; param-fallback already handled in provider)
func classifyLLMError(err error) string {
	if err == nil {
		return ""
	}
	// Stall check BEFORE the context checks: the watchdog's cancel makes
	// the surfaced error chain contain context.Canceled, but a stall is a
	// provider failure the caller is still waiting on — retryable, not a
	// terminal user Stop.
	var stall *firstTokenStallError
	if errors.As(err, &stall) {
		return "retryable"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "terminal"
	}
	var he *provider.HTTPError
	if !errors.As(err, &he) {
		// Connection-layer failure (dial refused/reset, read EOF, stream
		// cut): the providers wrap these as "send request: %w" /
		// "read stream: %w" around *url.Error / net errors, which
		// implement net.Error. Tier them apart from bounded "retryable":
		// a network blip never reached the provider and amplifies no
		// server load, so llmRetry retries them without a cap. Errors
		// WITHOUT a typed network cause (plain strings, response parse
		// failures) deliberately stay bounded — an unbounded loop on a
		// deterministic error would hang the turn forever.
		var ne net.Error
		if errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return "connection"
		}
		// Network-layer failure (send/read/EOF) — transient, retry.
		return "retryable"
	}
	code := he.StatusCode
	switch {
	case code == http.StatusPaymentRequired: // 402
		return "terminal"
	case code == http.StatusUnauthorized || code == http.StatusForbidden: // 401/403
		return "terminal"
	case code == http.StatusRequestEntityTooLarge: // 413
		return "context_length"
	case code == http.StatusBadRequest: // 400 — could be PTL or a param error
		body := strings.ToLower(he.Body)
		if strings.Contains(body, "context length") ||
			strings.Contains(body, "too long") ||
			strings.Contains(body, "too many tokens") ||
			strings.Contains(body, "maximum context") ||
			strings.Contains(body, "reduce the length") {
			return "context_length"
		}
		return "retryable"
	case code == http.StatusTooManyRequests: // 429
		return "retryable"
	case code >= 500: // 5xx — overloaded / server error / gateway
		return "retryable"
	default:
		return "retryable"
	}
}

// llmRetry wraps an LLM call with retry logic, in two tiers:
//
//   - bounded (5xx/429, untyped network strings): exponential backoff
//     (1s, 4s, 9s) across up to llmRetryAttempts calls. The server may
//     genuinely be overloaded — hammering it forever is abuse.
//   - unbounded (typed connection failures — dial/read/EOF before or
//     mid-stream): the request never reached the provider, so retries
//     load nobody but ourselves. Codex-style connection retry: 5s
//     doubling capped at 60s, no attempt limit, so a long task survives
//     a 30-minute network outage instead of dying after 14 seconds.
//     ctx cancellation (Stop, shutdown) always breaks the loop.
//
// Context cancellation / deadline exceeded and non-transient HTTP
// failures (billing/auth) are treated as terminal — there's no point
// retrying when the caller has gone away or the failure is permanent.
// Context-length errors are surfaced immediately as ErrContextTooLong so
// the caller can compress and resend rather than retrying an oversized
// payload.
//
// The label argument is used for structured logging (typically a.name).
const llmRetryAttempts = 3

// connRetryBackoff computes the unbounded tier's delay: 5s doubling,
// capped at 60s. A var so tests can shrink the sleeps.
var connRetryBackoff = func(attempt int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= time.Minute {
			return time.Minute
		}
	}
	return d
}

// retryAfterJitter returns the fractional jitter layered on top of a
// server-provided Retry-After delay (result ∈ [0, 0.5) → up to +50%,
// matching sand's computeServerPacedDelayMs). A var so tests can pin it.
var retryAfterJitter = rand.Float64

// retryAfterDelay converts a Retry-After header on a retryable HTTPError
// (429/5xx) into the backoff the server asked for, jittered by up to +50%
// and clamped to 30s so a hostile or broken value can't stall the turn.
// ok=false means no usable directive — the caller falls back to its
// normal backoff ladder.
func retryAfterDelay(err error) (time.Duration, bool) {
	var he *provider.HTTPError
	if !errors.As(err, &he) {
		return 0, false
	}
	secs, ok := he.RetryAfterSeconds()
	if !ok || secs <= 0 {
		return 0, false
	}
	d := time.Duration(secs) * time.Second
	d += time.Duration(float64(d) * 0.5 * retryAfterJitter())
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d, true
}

func llmRetry(ctx context.Context, label string, fn func(context.Context) (*provider.Response, error)) (*provider.Response, error) {
	for attempt := 1; ; attempt++ {
		resp, err := fn(ctx)
		if err == nil {
			if attempt > 1 {
				slog.Info("LLM call succeeded after retries",
					"agent", label, "attempts", attempt)
			}
			return resp, nil
		}

		// Classify the error to decide whether retrying helps at all.
		// This is the Hermes-style recovery router: terminal errors
		// (cancellation, billing, auth) and context-length errors are
		// not worth retrying — return immediately so the caller can take
		// the right recovery action (compress-and-resend for PTL, give
		// up for billing) instead of burning two more attempts.
		category := classifyLLMError(err)
		if category == "terminal" {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			slog.Warn("LLM call failed with terminal error, not retrying",
				"agent", label, "error", err)
			return nil, err
		}
		if category == "context_length" {
			slog.Warn("LLM call rejected for context length, surfacing for compaction",
				"agent", label, "error", err)
			return nil, fmt.Errorf("%w: %s", ErrContextTooLong, err.Error())
		}

		connection := category == "connection"
		if !connection && attempt >= llmRetryAttempts {
			slog.Error("LLM call failed after all retries",
				"agent", label, "attempts", llmRetryAttempts, "error", err)
			return nil, err
		}

		var backoff time.Duration
		serverPaced := false
		if paced, ok := retryAfterDelay(err); ok {
			// The server explicitly told us when to come back (429/5xx
			// with Retry-After). Honor it instead of the fixed ladder —
			// hammering an overloaded provider on our own schedule
			// amplifies the 429s. Checked before the connection tier
			// because a Retry-After can only ride on an HTTP response,
			// which by definition means the request DID reach the
			// provider (connection-tier errors carry no HTTPError).
			backoff, serverPaced = paced, true
		} else if connection {
			backoff = connRetryBackoff(attempt)
			if attempt >= 2 {
				// Surface from the 2nd retry on (the 1st is usually a
				// transient blip that resolves silently): the user sees
				// "reconnecting" instead of a seemingly frozen turn.
				// No-op when ctx carries no event consumers (background
				// Complete calls).
				emitEvent(ctx, ChatEvent{Type: "reconnecting", Data: map[string]any{
					"attempt":   attempt,
					"backoffMs": backoff.Milliseconds(),
				}})
			}
		} else {
			backoff = time.Duration(attempt*attempt) * time.Second // 1s, 4s, 9s
		}
		slog.Warn("LLM call failed, retrying",
			"agent", label, "attempt", attempt,
			"category", category, "backoff", backoff, "server_paced", serverPaced, "error", err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, errors.Join(err, ctx.Err())
		}
	}
}

// callLLMWithPTLRecovery wraps one ReAct-round LLM call with context-length
// (PTL) recovery: if the provider rejects the request because the prompt is
// too long, it compresses the conversation-history portion of messages
// in-place and retries the call once. This is FastClaw's equivalent of
// Hermes' restart_with_compressed_messages recovery path — the alternative
// (reporting an error and aborting the turn) is exactly the "long task
// dies mid-way" failure mode the loop-level fault-tolerance upgrade targets.
//
// messages is the full LLM-bound slice (system msgs + history). The first
// element is always the system prompt; compaction must never touch it, so
// the recovery splits messages into [system-prefix] + [history], compresses
// only the history, and reassembles. On a successful compaction the returned
// messages slice reflects the shrinkage so the caller can persist it onto
// the session (otherwise the next round would resend the same oversized
// payload and PTL again).
//
// callLLM performs the actual (retryable) provider call via llmRetry. PTL
// recovery fires at most once — a second PTL on the compressed payload means
// even the summary won't fit, and there's nothing more useful to do than
// surface the error.
func (a *Agent) callLLMWithPTLRecovery(ctx context.Context, messages []provider.Message, tools []provider.Tool, callLLM func(context.Context, []provider.Message, []provider.Tool) (*provider.Response, error)) (*provider.Response, []provider.Message, error) {
	// llmRetry keeps the transient-error backoff (network/5xx/429) and
	// routes terminal + context-length errors immediately, so the PTL
	// branch below fires on the first oversized response rather than
	// after two wasted retries.
	resp, err := llmRetry(ctx, a.name, func(ctx context.Context) (*provider.Response, error) {
		return callLLM(ctx, messages, tools)
	})
	if err == nil {
		return resp, messages, nil
	}
	if !errors.Is(err, ErrContextTooLong) {
		return nil, messages, err
	}

	// PTL: compress the history portion and retry once. Find the split
	// point — the system messages live at the front (role=="system");
	// everything after the last system message is conversation history
	// safe to compact. Guard against an all-system edge case (no history
	// to compress → nothing we can do).
	split := len(messages)
	for split > 0 && messages[split-1].Role == "system" {
		split--
	}
	if split == 0 {
		// No history to compact — the system prompt alone overflows.
		slog.Error("PTL recovery: system prompt alone exceeds context window",
			"agent", a.name, "msg_count", len(messages))
		return nil, messages, err
	}
	sysPrefix := messages[:split]
	history := messages[split:]
	// Force an aggressive threshold (half the normal compaction trigger)
	// so compression actually removes enough tokens to clear the PTL.
	threshold := a.compactionThresholdNow("")
	forceThreshold := max(threshold/2, 1000)
	compactResult, cerr := CompactMessages(history, a.homePath, a.bgProvider(), a.model, forceThreshold)
	if cerr != nil || compactResult == nil || !compactResult.Pruned {
		slog.Warn("PTL recovery: compaction did not reduce history",
			"agent", a.name, "error", cerr)
		return nil, messages, err
	}
	slog.Info("PTL recovery: compacted history, retrying LLM call",
		"agent", a.name,
		"before_msgs", len(history), "after_msgs", len(compactResult.Messages))
	messages = append(append([]provider.Message{}, sysPrefix...), compactResult.Messages...)

	resp, err = llmRetry(ctx, a.name, func(ctx context.Context) (*provider.Response, error) {
		return callLLM(ctx, messages, tools)
	})
	return resp, messages, err
}

// HandleMessage processes an inbound message through the ReAct loop.
// popLang extracts and removes the "lang" string param from params,
// returning it. Returns "" when params is nil, has no "lang" key, or the
// value isn't a string. Removing the key keeps the locale out of the
// LLM-facing client-params blob (see renderClientParams) — only the
// dedicated bus.InboundMessage.Lang field carries it onward.
func popLang(params map[string]any) string {
	if params == nil {
		return ""
	}
	v, ok := params["lang"]
	if !ok {
		return ""
	}
	delete(params, "lang")
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func (a *Agent) HandleMessage(ctx context.Context, msg bus.InboundMessage) string {
	a.persistInboundImages(&msg)
	// Lift the chatter's UI locale out of Params (where the web client
	// forwards its i18n setting) into the Lang field, so slash replies can
	// localize and the locale doesn't leak into the LLM-facing "Client
	// Parameters" system message rendered from Params. No-op for IM/cron
	// sources that never set params["lang"].
	msg.Lang = popLang(msg.Params)
	// IM channels, cron, and legacy callers never set params["lang"], so
	// fall back to the agent's configured default language (agent.json
	// "language" field). Empty stays empty → slashT then applies its own
	// default (Chinese).
	if msg.Lang == "" {
		msg.Lang = a.language
	}
	// Regex hooks: intercept messages matching a pattern and execute CLI
	// instead of the LLM. Evaluated before slash commands so fixed-format
	// messages (e.g. "翻译 xxx") bypass the agent loop entirely.
	if reply, hookName, matched, feedToLLM := a.matchRegexHooks(ctx, msg.Text); matched {
		sess := a.sessions.Get(sessionTriple(msg, msg.ProjectID))
		userMsg := buildUserMessage(msg, a.model)
		toolCallMsg := provider.Message{Role: "assistant", Content: "", ToolCalls: []provider.ToolCall{{ID: "regex-hook-0", Type: "function", Function: provider.FunctionCall{Name: "regex_hook: " + hookName, Arguments: regexHookArgs(msg.Text)}}}, Timestamp: time.Now().UnixMilli()}
		toolResMsg := provider.Message{Role: "tool", ToolCallID: "regex-hook-0", Content: "matched"}
		replyMsg := provider.Message{Role: "assistant", Content: reply, Timestamp: time.Now().UnixMilli()}
		if feedToLLM {
			sess.BeginTurn()
			sess.Append(userMsg)
			sess.Append(toolCallMsg)
			sess.Append(toolResMsg)
			sess.Append(replyMsg)
			sess.EndTurn()
		} else {
			// FeedToLLM=false: archive the exchange hidden (llm_visible=0)
			// so it shows in web history but stays out of the LLM working
			// set / summary / recall. emitEvent below still surfaces it live.
			sess.AppendArchivedHidden([]provider.Message{userMsg, toolCallMsg, toolResMsg, replyMsg})
		}
		emitEvent(ctx, ChatEvent{Type: "tool_call", Data: map[string]any{"id": "regex-hook-0", "name": "regex_hook: " + hookName, "arguments": msg.Text}})
		emitEvent(ctx, ChatEvent{Type: "tool_result", Data: map[string]any{"id": "regex-hook-0", "name": "regex_hook: " + hookName, "result": "matched"}})
		emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": reply}})
		emitEvent(ctx, ChatEvent{Type: "done"})
		return reply
	}
	// Check for slash commands first. Empty reply means "handled but
	// intentionally silent" — /goal foo and /goal resume both fall
	// through to a streaming continuation that IS the response, so
	// emitting a separate content event would just clutter the chat
	// with a redundant confirmation bubble.
	//
	// Slashes that queued a continuation emit `turn_pending` instead
	// of `done`; the POST SSE handler treats that as "stay open, the
	// real reply is coming on the next bus-fired turn." Without it,
	// the stream closes immediately and the typing indicator vanishes
	// while the model is still warming up.
	if result := a.handleSlashCommand(msg); result.handled {
		if result.reply != "" {
			emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": result.reply}})
		}
		if result.continueToLoop {
			// /yes (or /yolo with approved pending): fall through to the
			// ReAct loop so drainApprovedPending executes the authorized
			// calls and the LLM continues. Don't emit done — the loop
			// owns the turn end.
		} else if result.continuationQueued {
			emitEvent(ctx, ChatEvent{Type: "turn_pending"})
		} else {
			emitEvent(ctx, ChatEvent{Type: "done"})
		}
		if !result.continueToLoop {
			return result.reply
		}
	}

	// Quota gate: reject the turn early when the agent owner has
	// exceeded their billing ceiling. Checked before plan-mode and
	// the main ReAct loop so no LLM tokens are burned.
	if rejection := a.checkQuota(ctx); rejection != "" {
		emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": rejection}})
		emitEvent(ctx, ChatEvent{Type: "done"})
		return rejection
	}

	// Plan mode short-circuits the ReAct loop: tools off, the model
	// emits a numbered plan, the user reviews it and replies normally
	// (no planMode flag) on the next turn to execute. Lets users catch
	// the agent before it burns the iteration budget exploring the
	// wrong direction — the failure mode we saw on long research
	// prompts where deepseek-flash spent 95 messages exploring and
	// never produced a deliverable.
	// Plan-mode is silently dropped when this session has an active
	// goal. Goal is supposed to be autonomous — pausing for human
	// approval mid-loop contradicts the contract. Strip the flag so
	// downstream hooks see this turn as a normal one (IsPlanMode=false
	// → goalTriggerHook re-fires PostTurn → continuation chain stays
	// alive instead of waiting on the 30 s probe). To regain plan-mode
	// behaviour during goal-driven work, /goal pause first.
	if isPlanMode(msg.Params) {
		if a.sessionHasActiveGoal(ctx, msg) {
			slog.Info("ignoring plan-mode flag — session has an active goal",
				"agent", a.name, "chat_id", msg.ChatID)
			delete(msg.Params, "planMode")
		} else {
			return a.handlePlanMode(ctx, msg)
		}
	}

	chatterUID := a.chatterUserID(msg)
	// Tag ctx so the sandbox layer can bind-mount this chatter's
	// per-user skills dir into the container at /root/.agents/skills
	// (where `npx skills add -g -y` writes). Tagging happens before
	// any sandbox.Get call below so attachments + exec inherit it.
	// Tag ctx with the chatter so DBStore session writes stamp the
	// chatter_user_id column (sessions / session_messages /
	// session_events). user_id stays = UserSpace owner so admin views
	// continue to list "all sessions on my bots"; chatter_user_id
	// records the actual participant for per-chatter queries.
	ctx = store.WithChannel(ctx, msg.Channel)
	// Per-turn channel context for the skill-refresh diagnostic. Lets
	// us correlate the "skills summary refreshed" log emitted inside
	// refreshSkillsFromStore with the channel the request arrived on,
	// to chase the "IM doesn't see agent skills" report.
	slog.Info("turn: refreshing skills",
		"agent", a.name, "channel", msg.Channel, "chat_id", msg.ChatID, "user", chatterUID)
	a.refreshSkillsFromStore(chatterUID)
	sess := a.sessions.Get(sessionTriple(msg, msg.ProjectID))
	// Bind chatter onto sess. Session.ctx() builds its own
	// context.Background-rooted ctx for store calls, so the
	// WithChatterUserID we stamped onto the caller ctx above does NOT
	// reach AppendSessionMessage / SaveSession on its own — sess has to
	// carry the chatter itself.
	{
		prov, mdl := provider.SplitProviderModel(a.model)
		sess.SetProviderModel(prov, mdl)
	}
	// Bind the registry to this chat's session so workspace.Store reads
	// + writes get session-scoped paths and (when a sandbox pool is
	// wired) the executor used by exec/read_file/list_dir is tied to a
	// session-private container.
	a.bindSession(ctx, msg.Channel, msg.AccountID, msg.ChatID, sess.SessionKey(), msg.ProjectID)
	// Flag whether this turn's chatter is the agent owner / channel
	// admin. File tools use this to refuse identity-file reads from
	// regular chatters (SOUL/IDENTITY/BOOTSTRAP/... leak as verbatim
	// chat replies otherwise).
	a.registry.SetCallerIsAdmin(a.singleUser || a.isAdminChatter(msg))
	// Host-FS trust is stricter than identity-file trust: no singleUser
	// short-circuit, or an anonymous IM chatter on a single-user install
	// could read the operator's ~/.ssh via ~/Documents-style paths.
	a.registry.SetCallerHostTrusted(a.isAdminChatter(msg))
	slog.Info("diag: identity gate (msg)", "agent", a.agentID, "singleUser", a.singleUser, "isAdmin", a.isAdminChatter(msg), "channel", msg.Channel, "msgUserID", msg.UserID, "ownerUserID", a.ownerUserID)
	// Plumb the persistent session_key for goal-scoped tools.
	// SetSessionID above uses msg.ChatID (the channel-level chat
	// identifier); goal tools need the durable session.Session.SessionKey
	// to address rows in agent_goals.
	a.registry.SetGoalSessionKey(sess.SessionKey())
	// Per-user file writes (USER.md / MEMORY.md) need to land in the
	// per-turn chatter's row, not the UserSpace owner — see
	// Registry.systemFileUserID for the routing rule.

	// Steering: mark a turn in-flight so messages arriving mid-run are
	// buffered onto the session (drained between tool iterations below)
	// instead of starting a separate turn. flushLeftoverSteer parks any
	// steer that lost the end-of-turn race into history. Registered
	// before PadOrphanToolResults so it runs LAST (defers are LIFO) —
	// orphan padding settles history first.
	sess.BeginTurn()
	defer a.flushLeftoverSteer(sess)

	// Safety net for client-aborted turns: if the loop exits with a
	// tool_use that never got its matching tool_result appended (the
	// user clicked Stop while a long-running exec was in flight, the
	// SDK returned no response for it, etc.), pad the orphan so the
	// session history stays well-formed. Without this, the tool keeps
	// rendering as a forever-spinning "running" entry on history
	// rebuild and the next turn's API call gets a 400 from Anthropic
	// for orphaned tool_use ids.
	defer sess.PadOrphanToolResultsAndMarkAborted(session.ToolResultStoppedNote)

	// Reset per-turn tool failure tracking. The web_fetch (and any
	// future tool that opts in) consults the registry's
	// PriorFailure to refuse a guaranteed-fail retry within the
	// same turn — without StartTurn here, failures from a previous
	// turn would poison legit retries the user explicitly asked for.
	a.registry.StartTurn()

	// Hook: BeforeSystemPrompt
	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: BeforeSystemPrompt, UserID: a.ownerUserID})

	chatterMem := a.memory
	systemPrompt := a.ctxBuilder.BuildSystemPromptAs(chatterUID, chatterMem, sessionStartTime(sess))
	a.logSystemPromptFingerprint(msg.Channel, msg.ChatID, chatterUID, systemPrompt)

	// Hook: AfterSystemPrompt
	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: AfterSystemPrompt, UserID: a.ownerUserID})

	// Store the raw user message. Images may arrive via the legacy
	// PhotoURL (single, used by IM bridges) or PhotoURLs (multi, used by
	// the web chat upload path); flatten both into one content-parts
	// slice so the provider sees `[text, image, image, …]`.
	// buildUserMessage handles multi-image flatten + senderMetadata.
	// `[SenderName]:` content-prefix policy lives there (group-only;
	// DMs stay bare to avoid SOUL.md language-bias regressions).
	userMsg := buildUserMessage(msg, a.model)
	sess.Append(userMsg)

	// Context compaction: check if session messages are too large
	sessionMsgs := sess.GetMessages()
	threshold := a.compactionThresholdNow(systemPrompt)
	// A forked session's LLM context is parent prefix + working set, so
	// the prefix eats part of the window — lower the trigger threshold
	// by the prefix's estimated tokens so B compacts its own working set
	// earlier instead of letting prefix+workset blow past the window.
	// The prefix itself is a read-only snapshot and is never pruned.
	if prefix := sess.ParentPrefixMessages(); len(prefix) > 0 {
		if pt := EstimateTokens(prefix); pt > 0 && threshold > pt {
			threshold -= pt
		}
	}
	compactResult, err := CompactMessages(sessionMsgs, a.homePath, a.bgProvider(), a.model, threshold)
	if err != nil {
		slog.Warn("compaction error", "agent", a.name, "error", err)
	}
	if compactResult != nil && compactResult.Pruned {
		// Replace session messages with compacted version
		sess.ReplaceMessages(compactResult.Messages)
		sessionMsgs = compactResult.Messages
		slog.Info("context compacted", "agent", a.name, "log_file", compactResult.LogFile)
		// Persist a topic summary so cross-session recall captures long
		// conversations the user never explicitly /compact'd or /new'd.
		// Mirrors the /compact slash path (trigger "compaction").
		a.maybeExtractSummary(sess, "auto-compaction")
		// Persist compaction notice (方案 B: role=assistant + metadata.compactionNotice)
		// and emit SSE event so web/IM can show a bubble (Phase 3 Task 1).
		if compactResult.TokensBefore > 0 {
			text, meta := buildCompactionNotice(compactResult)
			sess.Append(provider.Message{
				Role:      "assistant",
				Content:   text,
				Metadata:  map[string]any{"compactionNotice": meta},
				Timestamp: time.Now().UnixMilli(),
			})
			emitEvent(ctx, ChatEvent{Type: "compaction_notice", Data: map[string]any{
				"content":        text,
				"before":         meta["before"],
				"after":          meta["after"],
				"retained_turns": meta["retained_turns"],
			}})
			// Broadcast notice text to IM channels (web session receives it
			// via the SSE event above; IM has no SSE so we push via the
			// outbound bus). Sent before the ReAct loop starts so the user
			// sees the notice before the agent's reply. Non-blocking send:
			// a full outbound queue drops the notice rather than stalling
			// the agent loop — matches sendMediaFiles pattern.
			if msg.Channel != "web" && isIMChannel(msg.Channel) && a.messageBus != nil {
				select {
				case a.messageBus.Outbound <- bus.OutboundMessage{
					Channel:   msg.Channel,
					AccountID: msg.AccountID,
					ChatID:    msg.ChatID,
					Text:      text,
				}:
				default:
					slog.Warn("outbound channel full, dropping compaction notice", "agent", a.name)
				}
			}
		}
	}

	messages := make([]provider.Message, 0, len(sessionMsgs)+4)
	messages = append(messages, provider.Message{Role: "system", Content: systemPrompt})
	if hints := renderChannelHints(msg, a.splitReplies); hints != "" {
		messages = append(messages, provider.Message{Role: "system", Content: hints})
	}
	if senderMsg := renderSender(msg); senderMsg != "" {
		messages = append(messages, provider.Message{Role: "system", Content: senderMsg})
	}
	if paramsMsg := renderClientParams(msg.Params); paramsMsg != "" {
		messages = append(messages, provider.Message{Role: "system", Content: paramsMsg})
	}
	// Persistence reminder — chatbot-only, positioned just before the
	// session history so recency weight outranks the model's training
	// prior of "I have no cross-session memory". See
	// renderChatbotPersistenceReminder for why this isn't enough to put
	// in the main system prompt alone.
	if reminder := renderChatbotPersistenceReminder(a.promptMode, a.displayName, chatterMem.LoadUserFile(), chatterMem.LoadMemory()); reminder != "" {
		messages = append(messages, provider.Message{Role: "system", Content: reminder})
	}
	if msg.Source == bus.SourceCron {
		messages = append(messages, provider.Message{Role: "system", Content: cronTriggerGuidance})
	}
	if prefix := sess.ParentPrefixMessages(); len(prefix) > 0 {
		sessionMsgs = append(prefix, sessionMsgs...)
	}
	messages = append(messages, withConversationGapContext(sessionMsgs)...)

	toolDefs := a.registry.DefinitionsForMode(builtinAllowForMode(a.promptMode))

	// Loop detection: track consecutive identical tool calls
	type toolCallSig struct {
		name string
		hash [32]byte
	}
	var lastSig toolCallSig
	consecutiveCount := 0
	totalToolCalls := 0
	// allFailedRounds is the count of CONSECUTIVE rounds where every
	// tool result came back as a 4xx/5xx HTTP error or an executor
	// error. This catches the "model rotates through five guessed
	// URLs that all 404" pattern that loop detection (which keys on
	// identical args) misses. After three such rounds we drop tools
	// from the next LLM call so the model is forced to produce text
	// directly instead of burning more rounds chasing dead URLs.
	allFailedRounds := 0
	const failedRoundsLimit = 3
	// toolFailStreak is a per-tool-name count of CONSECUTIVE failures
	// (different args allowed — same-args is already caught by
	// consecutiveCount above). Borrowed from Hermes' same-tool failure
	// streak guardrail: when a single tool keeps failing across rounds
	// even though other tools in those rounds succeeded (so
	// allFailedRounds resets), this trips and temporarily hides that one
	// tool from the next LLM call so the model is forced to change tack
	// instead of grinding the same broken tool. Hermes uses 3 warn / 8
	// hard-stop; we pick a single mid-value since FastClaw has no
	// warn-only tier yet.
	toolFailStreak := make(map[string]int)
	const toolFailStreakLimit = 5
	var trippedTools []string // tools that hit the limit this turn (for the hide-nudge)
	// truncationRetried caps the max-output continuation at one retry per
	// turn so a model that keeps hitting the cap can't loop forever asking
	// itself to "continue". Phase 3 (see looksTruncated + the nudge below).
	truncationRetried := false
	// emptyRetried caps the empty-response nudge at one retry per turn —
	// same shape as truncationRetried. Without it, a model that keeps
	// returning empty responses would burn every remaining iteration on
	// nudges and the user would still get the error string at the end.
	emptyRetried := false

	// replyParts accumulates every non-empty assistant text segment
	// emitted across iterations (preamble lines before tool calls + the
	// final answer). IM channels deliver a single OutboundMessage per
	// turn, so without accumulation only the last segment reaches WeChat
	// while the chat panel shows all of them. Joined with
	// channels.SplitMessageMarker at return time; manager.dispatchOutbound
	// splits on it (AllowSplit=true) or collapses to newlines otherwise.
	var replyParts []string
	var kbSources []kb.KnowledgeSource                                                         // cached [K#] citation sources from this turn's KB retrieval
	ctx = kb.WithSourcesAccumulator(ctx, &kbSources)                                           // KB tool calls append their citation sources here
	ctx = kb.WithSourceOrigin(ctx, kb.SourceOrigin{SessionID: sess.Key(), Seq: len(messages)}) // L1 dedup: same session+seq = rewrite of captured content
	citedMemos := make(map[int64]bool)
	ctx = tools.WithCitedSummaries(ctx, &citedMemos) // memory_search dedups against this across calls
	consumedRecalls := make([]string, 0, 2)
	ctx = tools.WithConsumedRecallIDs(ctx, &consumedRecalls) // recall.consumed: fetch_messages marks this turn's recall ids used

	// Drain user-authorized pending calls (/yes, /yolo) BEFORE the loop
	// so their results are in `messages` when the LLM picks up the turn.
	totalToolCalls += a.drainApprovedPending(ctx, sess, &messages)

	// ReAct loop
	for i := 0; i < a.maxToolIterations; i++ {
		slog.Info("agent loop iteration",
			"agent", a.name,
			"iteration", i+1,
			"channel", msg.Channel,
			"chat_id", msg.ChatID,
		)

		// Hook: BeforeModelCall
		hcBefore := &HookContext{AgentName: a.name, Point: BeforeModelCall, Messages: messages, Source: msg.Source, Channel: msg.Channel, AccountID: msg.AccountID, ChatID: msg.ChatID, UserID: a.ownerUserID}
		a.hooks.Run(ctx, hcBefore)

		// KB auto-query hook outputs: collect the indicator line once,
		// record synthetic knowledgebase_search tool_call/result pairs
		// into the transcript, honor strict-mode SkipLLM (emit the
		// prebuilt answer and end the turn), and adopt the hook's
		// rewritten messages (augment mode injects a [KB] context block).
		if len(kbSources) == 0 && len(hcBefore.KnowledgeSources) > 0 {
			kbSources = hcBefore.KnowledgeSources
		}
		for _, stc := range hcBefore.SyntheticToolCalls {
			tcID := "synth-" + stc.Name
			emitEvent(ctx, ChatEvent{Type: "tool_call", Data: map[string]any{"id": tcID, "name": stc.Name, "arguments": stc.Args}})
			emitEvent(ctx, ChatEvent{Type: "tool_result", Data: map[string]any{"id": tcID, "name": stc.Name, "result": stc.Result}})
			asstMsg := provider.Message{Role: "assistant", Content: "", ToolCalls: []provider.ToolCall{{ID: tcID, Type: "function", Function: provider.FunctionCall{Name: stc.Name, Arguments: stc.Args}}}, Timestamp: time.Now().UnixMilli()}
			sess.Append(asstMsg)
			toolMsg := provider.Message{Role: "tool", ToolCallID: tcID, Content: stc.Result}
			sess.Append(toolMsg)
		}
		if hcBefore.SkipLLM {
			content := hcBefore.PrebuiltContent
			emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": content}})
			emitEvent(ctx, ChatEvent{Type: "done"})
			return content
		}
		messages = hcBefore.Messages

		if a.provider == nil {
			slog.Error("agent has no provider configured", "agent", a.name, "model", a.model)
			noProviderMsg := "Agent is not configured with a usable LLM provider. Check that cfg.Providers contains the prefix referenced by model `" + a.model + "`."
			emitEvent(ctx, ChatEvent{Type: "error", Data: map[string]any{"message": noProviderMsg}})
			emitEvent(ctx, ChatEvent{Type: "done"})
			return noProviderMsg
		}
		// After enough consecutive rounds where every tool came back
		// as 4xx/5xx, drop tools from the next call so the model is
		// forced to produce a text answer with what it has. The
		// system message above the request makes the constraint
		// explicit so the model doesn't apologetically dangle.
		callTools := toolDefs
		if allFailedRounds >= failedRoundsLimit {
			slog.Warn("disabling tools after consecutive failed rounds",
				"agent", a.name, "failed_rounds", allFailedRounds)
			callTools = nil
			messages = append(messages, provider.Message{
				Role: "system",
				Content: fmt.Sprintf(
					"The last %d rounds of tool calls all failed (HTTP errors or empty results). Stop calling tools and answer the user directly with what you know — explain that authoritative sources weren't reachable and provide your best-effort response based on training knowledge, clearly marked as unverified.",
					allFailedRounds,
				),
			})
		}
		// Per-tool streak circuit breaker: hide any single tool that has
		// failed toolFailStreakLimit times in a row (different args each
		// time) so the model can't keep grinding it. Finer-grained than
		// the allFailedRounds kill-switch — keeps the working tools live
		// while sidelining just the broken one. Borrowed from Hermes'
		// ToolCallGuardrailConfig same-tool failure streak.
		if len(callTools) > 0 {
			callTools, trippedTools = hideTrippedTools(callTools, toolFailStreak, toolFailStreakLimit, trippedTools)
			if len(trippedTools) > 0 {
				slog.Warn("hiding tools after per-tool failure streak",
					"agent", a.name, "tools", trippedTools)
				messages = append(messages, provider.Message{
					Role: "system",
					Content: fmt.Sprintf(
						"These tools have failed repeatedly and are temporarily unavailable this turn: %s. Do not attempt them; use a different tool or answer directly with what you already know.",
						strings.Join(trippedTools, ", "),
					),
				})
			}
		}
		// PII scrub snapshot for the dump only — the actual call's scrub
		// happens inside the callLLMWithPTLRecovery callback so a PTL
		// compaction (which shrinks the raw `messages`) stays consistent.
		dumpMessages := messages
		if a.piiScrubEnabled {
			dumpMessages = privacy.ScrubMessages(messages, privacy.Options{Entropy: a.piiEntropyEnabled})
		}
		dumpLLMRequest(a.name, a.model, dumpMessages, callTools)
		// callLLMWithPTLRecovery adds context-length (PTL) recovery on top
		// of llmRetry: if the provider rejects an oversized prompt, it
		// compacts the history portion of messages once and retries, so a
		// long-running task survives a context-window overflow mid-turn
		// instead of dying with "processing_failed".
		var resp *provider.Response
		resp, messages, err = a.callLLMWithPTLRecovery(ctx, messages, callTools, func(ctx context.Context, msgs []provider.Message, tools []provider.Tool) (*provider.Response, error) {
			llm := msgs
			if a.piiScrubEnabled {
				llm = privacy.ScrubMessages(msgs, privacy.Options{Entropy: a.piiEntropyEnabled})
			}
			return a.streamChatToResponse(ctx, llm, tools)
		})

		// Hook: AfterModelCall
		hcAfter := &HookContext{AgentName: a.name, Point: AfterModelCall, Messages: messages, Response: resp, Error: err, StartTime: hcBefore.StartTime, Channel: msg.Channel, AccountID: msg.AccountID, ChatID: msg.ChatID, UserID: a.ownerUserID, GoalSessionKey: a.registry.GoalSessionKey()}
		a.hooks.Run(ctx, hcAfter)

		if err != nil {
			// Cancellation is a control-flow outcome (Stop, shutdown, or a
			// disconnected caller), not a provider failure. Publishing it as
			// an error leaves a persisted "context canceled" bubble that can
			// arrive after the UI has already rendered "(Stopped)".
			if errors.Is(err, context.Canceled) {
				slog.Info("LLM chat canceled", "agent", a.name)
				emitEvent(ctx, ChatEvent{Type: "done"})
				return ""
			}
			slog.Error("LLM chat failed after retries", "agent", a.name, "error", err)
			emitEvent(ctx, ChatEvent{Type: "error", Data: map[string]any{"message": err.Error()}})
			emitEvent(ctx, ChatEvent{Type: "done"})
			return slashT(msg.Lang, "error.processing_failed")
		}
		a.meterTokens(ctx, sess.Key(), resp.Usage, 0)
		a.maybeRecoverToolCalls(resp)

		if !resp.HasToolCalls() {
			if strings.TrimSpace(resp.Content) == "" {
				// Empty-response nudge (borrowed from Cursor sand's reply
				// nudge): the model produced nothing — no text, no tool
				// calls. Rather than deliver the raw error string to the
				// IM user ("model returned an empty response"), give the
				// model one hidden chance to answer with what it already
				// has, tools off. The nudge is transient (messages only,
				// never appended to the session) and capped at one retry
				// by emptyRetried; a second empty response falls through
				// to the error delivery below.
				if !emptyRetried {
					emptyRetried = true
					slog.Warn("model returned an empty response, nudging once for a real answer",
						"agent", a.name, "iteration", i+1)
					messages = append(messages, emptyResponseNudge())
					continue
				}
				// Auto-title only needs the opening turns — fire even
				// when the turn itself failed (e.g. context too long →
				// empty response), or long sessions that pre-date
				// auto-title never get named because every turn bails
				// here before runPostTurn.
				a.tryAutoTitle(ctx, messages)
				emptyMsg := "model returned an empty response"
				emitEvent(ctx, ChatEvent{Type: "error", Data: map[string]any{"message": emptyMsg}})
				emitEvent(ctx, ChatEvent{Type: "done"})
				return emptyMsg
			}
			// Max-output truncation recovery (Phase 3): when the response
			// used ~all of the output budget AND has no tool calls, it was
			// almost certainly cut off mid-answer. Rather than deliver a
			// half-finished reply, retain what we have, nudge the model to
			// continue, and loop once. Borrowed from OpenSpace's
			// FORCE_TOOL_ON_MAX_OUTPUT_RECOVERY — same idea (the response
			// hit the cap, so make the model finish) without provider-side
			// finish_reason (FastClaw's Response has none, so we infer via
			// token usage). Capped at one retry via truncationRetried so a
			// model that keeps maxing out can't spin.
			if !truncationRetried && looksTruncated(resp, a.maxTokens) {
				truncationRetried = true
				slog.Warn("response likely truncated at max_tokens, requesting continuation",
					"agent", a.name, "output_tokens", resp.Usage.OutputTokens, "max_tokens", a.maxTokens)
				asst := provider.Message{Role: "assistant", Content: resp.Content, Thinking: resp.Thinking, Metadata: kbSourcesMetadata(kbSources), Timestamp: time.Now().UnixMilli(), RawAssistant: resp.RawAssistant}
				sess.Append(asst)
				emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": resp.Content}})
				replyParts = append(replyParts, resp.Content)
				messages = append(messages, asst)
				messages = append(messages, continuationNudge())
				continue
			}
			kbMeta := kbSourcesMetadata(kbSources)
			asst := provider.Message{Role: "assistant", Content: resp.Content, Thinking: resp.Thinking, Metadata: kbMeta, Timestamp: time.Now().UnixMilli(), RawAssistant: resp.RawAssistant}
			sess.Append(asst)
			emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": resp.Content, "metadata": kbMeta}})
			if resp.Content != "" {
				replyParts = append(replyParts, resp.Content)
			}
			// End-of-turn steer race: a message buffered after the last
			// between-rounds drain but before we declare the turn done.
			// Fold it in and keep going instead of returning, so the
			// user's mid-flight instruction isn't deferred to a new turn.
			if steer := sess.DrainSteer(); len(steer) > 0 {
				// Carry the just-produced answer into the next LLM call
				// only when it has text. A no-text, no-tool-call
				// assistant message is an invalid turn for Anthropic
				// (an assistant turn needs a non-empty content block),
				// and this is the only path that would re-send one.
				if resp.Content != "" {
					messages = append(messages, asst)
				}
				messages = a.appendSteer(ctx, sess, messages, steer)
				continue
			}
			emitEvent(ctx, ChatEvent{Type: "done"})
			a.runPostTurn(ctx, msg, messages, totalToolCalls, chatterMem)
			return joinReplyParts(replyParts)
		}

		// Emit assistant content before tool calls if present
		if resp.Content != "" {
			emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": resp.Content}})
			replyParts = append(replyParts, resp.Content)
		}

		// Emit tool_call events
		for _, tc := range resp.ToolCalls {
			emitEvent(ctx, ChatEvent{Type: "tool_call", Data: map[string]any{
				"id":        tc.ID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			}})
		}

		assistantMsg := provider.Message{
			Role:         "assistant",
			Content:      resp.Content,
			ToolCalls:    resp.ToolCalls,
			Thinking:     resp.Thinking,
			Metadata:     kbSourcesMetadata(kbSources),
			Timestamp:    time.Now().UnixMilli(),
			RawAssistant: resp.RawAssistant,
		}
		sess.Append(assistantMsg)
		messages = append(messages, assistantMsg)

		// Loop detection: check before executing
		loopDetected := false
		for _, tc := range resp.ToolCalls {
			sig := toolCallSig{
				name: tc.Function.Name,
				hash: sha256.Sum256([]byte(tc.Function.Arguments)),
			}
			if sig.name == lastSig.name && sig.hash == lastSig.hash {
				consecutiveCount++
			} else {
				consecutiveCount = 1
				lastSig = sig
			}
			if consecutiveCount >= 3 {
				slog.Warn("tool loop detected", "agent", a.name, "tool", tc.Function.Name)
				warnMsg := provider.Message{
					Role:    "system",
					Content: "Loop detected: you called the same tool with the same arguments 3 times. Please try a different approach.",
				}
				sess.Append(warnMsg)
				messages = append(messages, warnMsg)
				loopDetected = true
				break
			}
		}
		if loopDetected {
			break
		}

		// Fire BeforeToolCall hooks
		for _, tc := range resp.ToolCalls {
			a.hooks.Run(ctx, &HookContext{
				AgentName: a.name,
				Point:     BeforeToolCall,
				ToolName:  tc.Function.Name,
				ToolArgs:  tc.Function.Arguments,
				Channel:   msg.Channel,
				AccountID: msg.AccountID,
				ChatID:    msg.ChatID,
				UserID:    a.ownerUserID,
			})
		}

		// Apply per-round parallel cap. The LLM decides how many
		// tool calls to emit; we cap how many run concurrently this
		// round. Overflow gets a synthetic "deferred" tool_result so
		// the model sees them as resolved (no orphan tool_use ids
		// that would poison the next API request) but without
		// content — naturally re-issuing them next round when it can
		// react to the executed batch's results. Effective default
		// is 0 = unlimited; users hit specific rate-limited APIs
		// (Brave free tier 1RPS, etc.) set it to 1 / 2 to force
		// strict serial / lightly-parallel execution.
		executeCalls := resp.ToolCalls
		// Auth gate (stage 3): split executeCalls into allowed vs.
		// blocked/prompted before running anything. Blocked calls get a
		// synthetic tool_result so every tool_use id stays paired;
		// prompted calls are parked on the session for /yes to drain.
		var authBlocked []toolCallResult
		authPrompted := false
		if a.authGate != nil {
			var blockedMap map[string]toolCallResult
			var promptDesc string
			executeCalls, blockedMap, promptDesc = a.filterAuthorizedCalls(sess, executeCalls)
			if promptDesc != "" {
				a.emitAuthPrompt(ctx, promptDesc, msg)
				authPrompted = true
			}
			for _, br := range blockedMap {
				authBlocked = append(authBlocked, br)
			}
		}
		// Apply the per-round parallel cap on the post-auth set so the cap
		// can't resurrect calls the gate just refused (the previous code
		// re-sliced resp.ToolCalls and overrode the gate's filtering).
		var deferredCalls []provider.ToolCall
		if a.maxParallelToolCalls > 0 && len(executeCalls) > a.maxParallelToolCalls {
			deferredCalls = executeCalls[a.maxParallelToolCalls:]
			executeCalls = executeCalls[:a.maxParallelToolCalls]
			slog.Info("deferring tool calls beyond parallel cap",
				"agent", a.name,
				"cap", a.maxParallelToolCalls,
				"deferred", len(deferredCalls),
			)
		}

		// Execute tools concurrently via SDK engine
		slog.Info("executing tools concurrently",
			"agent", a.name,
			"count", len(executeCalls),
		)
		results := a.engine.executeToolsConcurrently(ctx, a.registry, executeCalls, a.workspacePath)
		// Merge auth-denied calls (each already paired to its tool_use id)
		// into the result set so the next API request is well-formed.
		results = append(results, authBlocked...)
		// Append synthetic deferred results so every original tool_use
		// id has a paired tool_result. The deferred message tells the
		// model exactly why it didn't run — it can re-issue next
		// round once it has the executed batch's results.
		for _, tc := range deferredCalls {
			results = append(results, toolCallResult{
				toolCallID: tc.ID,
				toolName:   tc.Function.Name,
				result: fmt.Sprintf(
					"Deferred — this turn's parallel-tool cap is %d, and you emitted %d. Re-issue this exact call next round if you still need it; you'll have the other tools' results to inform the decision then.",
					a.maxParallelToolCalls, len(resp.ToolCalls),
				),
			})
		}

		// Defensive backstop: if the SDK returned fewer results than tool
		// calls (and the bridge somehow didn't already pad — belt and
		// suspenders since orphan tool_use ids poison the next API request
		// with HTTP 400), synthesize a failure result so every tool_use
		// gets a paired tool_result in the conversation history.
		if len(results) < len(resp.ToolCalls) {
			padded := make([]toolCallResult, len(resp.ToolCalls))
			gotByID := make(map[string]toolCallResult, len(results))
			for _, r := range results {
				gotByID[r.toolCallID] = r
			}
			for i, tc := range resp.ToolCalls {
				if r, ok := gotByID[tc.ID]; ok {
					padded[i] = r
					continue
				}
				padded[i] = toolCallResult{
					toolCallID: tc.ID,
					toolName:   tc.Function.Name,
					result:     "tool execution did not return a result",
					err:        fmt.Errorf("missing executor response for %s", tc.ID),
				}
			}
			results = padded
		}

		// Round-level failure detection: did EVERY result come back
		// as a 4xx/5xx HTTP error or executor error? Tracked here so
		// the next iteration can decide whether to drop tools.
		roundAllFailed := len(results) > 0
		// Process results
		for idx, r := range results {
			totalToolCalls++
			tc := resp.ToolCalls[idx]
			resultContent, meta := extractToolMeta(r.result)
			resultContent = annotateReachability(r.toolName, resultContent, a.registry)

			// Hook: AfterToolCall
			a.hooks.Run(ctx, &HookContext{
				AgentName:      a.name,
				Point:          AfterToolCall,
				ToolName:       r.toolName,
				ToolResult:     resultContent,
				Error:          r.err,
				Channel:        msg.Channel,
				AccountID:      msg.AccountID,
				ChatID:         msg.ChatID,
				UserID:         a.ownerUserID,
				GoalSessionKey: a.registry.GoalSessionKey(),
				IsPlanMode:     isPlanMode(msg.Params),
				Source:         msg.Source,
			})

			if r.err != nil {
				slog.Warn("tool execution error",
					"agent", a.name,
					"name", r.toolName,
					"error", r.err,
				)
			}

			// Classify the result: did this single call fail? Records
			// it in the registry's per-turn failure map so a later
			// retry of the same args can be short-circuited (see
			// Registry.PriorFailure / web_fetch).
			thisFailed := isFailedToolResult(r.err, resultContent)
			if thisFailed {
				summary := r.err.Error()
				if summary == "" || summary == "<nil>" {
					summary = firstNonEmptyLine(resultContent)
				}
				a.registry.RecordToolFailure(r.toolName, tc.Function.Arguments, summary)
				if cat, hint := classifyToolError(resultContent); cat != "" {
					resultContent = resultContent + "\n[失败类别: " + cat + "] [可恢复: " + hint + "]"
				}
				// Per-tool consecutive-failure streak. Unlike
				// allFailedRounds (which resets on ANY round success),
				// this counts one tool's failures across rounds even
				// when sibling tools in the same round succeed — the
				// "web_search keeps 502ing while read_file works" shape
				// that allFailedRounds can't see.
				toolFailStreak[r.toolName]++
			} else {
				// One call in this round produced a real result —
				// the round as a whole isn't "all failed".
				roundAllFailed = false
				toolFailStreak[r.toolName] = 0
			}

			// Index in FTS if available
			if a.ftsStore != nil {
				_ = a.ftsStore.Index(a.name, msg.ChatID, "tool:"+r.toolName, resultContent, time.Now())
			}
			// Persist image_gen output to /workspace and rewrite the URLs so
			// generated images flow through the normal workspace-deliverable
			// path (gateway ships real bytes to IM; turn-end fallback covers a
			// reference the model forgot). Other tools' images are left alone.
			if r.toolName == "image_gen" {
				resultContent = a.persistImageGenOutput(ctx, msg.ChatID, msg.ProjectID, resultContent)
			}

			toolMsg := provider.Message{
				Role:       "tool",
				Content:    resultContent,
				ToolCallID: tc.ID,
				Name:       r.toolName,
				Metadata:   meta,
			}
			sess.Append(toolMsg)
			messages = append(messages, toolMsg)

			evt := map[string]any{
				"id":     tc.ID,
				"name":   r.toolName,
				"result": resultContent,
			}
			if meta != nil {
				evt["metadata"] = meta
			}
			emitEvent(ctx, ChatEvent{Type: "tool_result", Data: evt})
		}
		// Update consecutive-failed-rounds tally now that the whole
		// round's results have been processed. A single non-failure
		// resets it — the model just got useful info, give it room
		// to use it.
		if roundAllFailed {
			allFailedRounds++
		} else {
			allFailedRounds = 0
		}

		// Auth-prompted round: end the turn here. The auth_prompt event
		// already rendered tappable /yes /no /auto /yolo buttons in the
		// UI; calling the LLM again would just relay the holding
		// tool_result as "请回复 /yes" text — burying the buttons under
		// a redundant agent bubble. /yes (or /no / /auto / /yolo) on
		// the next turn drains the parked calls via drainApprovedPending
		// and the LLM picks up with all results (allowed + authorized)
		// in context.
		if authPrompted {
			emitEvent(ctx, ChatEvent{Type: "done"})
			a.runPostTurn(ctx, msg, messages, totalToolCalls, chatterMem)
			return joinReplyParts(replyParts)
		}

		// Steering: messages that arrived while this tool round ran are
		// folded in here, between rounds, so the next LLM call sees them
		// and can change course.
		if steer := sess.DrainSteer(); len(steer) > 0 {
			messages = a.appendSteer(ctx, sess, messages, steer)
		}
	}

	slog.Warn("max tool iterations reached — forcing final delivery", "agent", a.name, "max", a.maxToolIterations)
	// Forced final delivery: one more LLM call with tools disabled and a
	// nudge that tells the model to synthesize what it has. Replaces the
	// old behavior of just returning a canned warning, which left users
	// with zero deliverable after a full iteration budget got burned.
	finalMessages := append(messages, capReachedNudge(a.maxToolIterations))
	if a.piiScrubEnabled {
		finalMessages = privacy.ScrubMessages(finalMessages, privacy.Options{Entropy: a.piiEntropyEnabled})
	}
	finalContent := ""
	finalResp, finalErr := a.streamChatToResponseQuiet(ctx, finalMessages, nil)
	if finalErr == nil {
		finalContent = scrubLeakedToolCallContent(finalResp.Content)
		a.meterTokens(ctx, sess.Key(), finalResp.Usage, 0)
	}
	if finalContent == "" {
		// Synthesis call itself failed or returned empty — fall back to
		// the canned line so the user still gets *something* with the
		// badge attached.
		finalContent = fmt.Sprintf("I've reached the maximum number of tool iterations (%d) and couldn't synthesize a final response. The work above represents what I gathered before hitting the limit.", a.maxToolIterations)
	}
	capMeta := iterationCapMetadata(a.maxToolIterations)
	sess.Append(provider.Message{
		Role:      "assistant",
		Content:   finalContent,
		Metadata:  capMeta,
		Timestamp: time.Now().UnixMilli(),
	})
	emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{
		"content":  finalContent,
		"metadata": capMeta,
	}})
	if finalContent != "" {
		replyParts = append(replyParts, finalContent)
	}
	emitEvent(ctx, ChatEvent{Type: "done"})
	a.runPostTurn(ctx, msg, messages, totalToolCalls, chatterMem)
	return joinReplyParts(replyParts)
}

// joinReplyParts joins accumulated assistant text segments with
// channels.SplitMessageMarker so manager.dispatchOutbound can deliver
// them as separate IM bubbles when AllowSplit is true. Channels
// without AllowSplit collapse the marker to a newline at dispatch
// time, so users still see every segment in one message instead of
// dropping all but the last.
func joinReplyParts(parts []string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return ""
	}
	if len(out) == 1 {
		return out[0]
	}
	return strings.Join(out, channels.SplitMessageMarker)
}

// isFailedToolResult is the agent loop's heuristic for "this tool
// returned nothing useful". Used both to populate the per-turn failure
// map (so a later identical call can be refused up front) and to drive
// the consecutive-failed-rounds short-circuit. We deliberately stay
// conservative — empty exec output is legit for many shell commands —
// and only flag the high-signal patterns: tool error, HTTP 4xx/5xx,
// or the `[Analyze the error above…]` envelope our wrapper appends to
// upstream failures.
func isFailedToolResult(err error, content string) bool {
	if err != nil {
		return true
	}
	c := strings.TrimSpace(content)
	if strings.HasPrefix(c, "HTTP 4") || strings.HasPrefix(c, "HTTP 5") {
		return true
	}
	if strings.Contains(c, "[Analyze the error above and try a different approach.]") {
		return true
	}
	return false
}

// firstNonEmptyLine returns the first non-empty line of s, trimmed
// and capped at 120 chars. Used to make a stash-friendly summary of a
// tool result when err.Error() is empty. (Named distinctly from
// skills.firstLine to avoid the duplicate declaration.)
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 120 {
			return line[:120] + "…"
		}
		return line
	}
	return ""
}

// hideTrippedTools removes any tool whose consecutive-failure streak has hit
// the limit from the LLM-bound tool list for this round, and records newly-
// tripped tool names so the caller can nudge the model about them. already
// carries tools that tripped on a prior round of the SAME turn so we don't
// re-nudge (and so a tool stays hidden once tripped). Returns the filtered
// tool slice and the accumulated tripped list.
//
// This is the per-tool counterpart to the allFailedRounds kill-switch:
// instead of dropping ALL tools when every round fails, it surgically hides
// just the one repeat-offender while leaving the rest available — the
// "web_search 502s five times but read_file works fine" shape that a
// whole-round counter can't isolate.
func hideTrippedTools(toolDefs []provider.Tool, streak map[string]int, limit int, already []string) ([]provider.Tool, []string) {
	if len(streak) == 0 {
		return toolDefs, already
	}
	trippedSet := make(map[string]bool, len(already))
	for _, n := range already {
		trippedSet[n] = true
	}
	newlyTripped := []string{}
	for name, count := range streak {
		if count >= limit && !trippedSet[name] {
			newlyTripped = append(newlyTripped, name)
			trippedSet[name] = true
		}
	}
	if len(newlyTripped) == 0 {
		// Still filter by the full trippedSet so tools that tripped in an
		// earlier round stay hidden this round.
		if len(already) == 0 {
			return toolDefs, already
		}
	}
	filtered := make([]provider.Tool, 0, len(toolDefs))
	for _, t := range toolDefs {
		if trippedSet[t.Function.Name] {
			continue
		}
		filtered = append(filtered, t)
	}
	return filtered, append(already, newlyTripped...)
}

// padOrphanToolResults lives on Session now (Session.PadOrphanToolResults
// in internal/session) — the turn-exit defers call it directly, and the
// crash-restart load path reuses the same walk to heal sessions whose
// last turn died with the daemon (see Session.healInterruptedTurn).

// msg is the InboundMessage that drove this turn — its (channel, account,
// chat, project) plus Source ride along on the HookContext so PostTurn
// hooks can route to session-scoped state and tell user-driven turns
// apart from runtime-originated ones (cron, heartbeat, sub-agent, goal
// continuation).
//
// chatterMem is the chatter-scoped Memory built at the top of the turn —
// auto-persist writes the extracted facts back through it so a visitor
// on a public agent accrues their *own* MEMORY.md / USER.md, not the
// owner's. nil falls back to the agent-scoped Memory (legacy behavior).
//
// Streaming (HandleMessageStream) and non-streaming (HandleMessage) both
// fire this. The streaming path calls it from inside the background
// goroutine that drains the SSE stream, after the final assistant
// message has been appended to the session — i.e. after the user's
// reply is fully on-record.
func (a *Agent) runPostTurn(ctx context.Context, msg bus.InboundMessage, messages []provider.Message, toolCallCount int, chatterMem *Memory) {
	if chatterMem == nil {
		chatterMem = a.memory
	}
	a.turnCount.Add(1)

	// Index user/assistant messages in FTS. Skip runtime-injected
	// messages (e.g. goal_context continuations) — they're synthetic
	// audit prompts, not searchable conversation content.
	if a.ftsStore != nil {
		for _, m := range messages {
			if m.Origin != provider.OriginUser {
				continue
			}
			if m.Role == "user" || m.Role == "assistant" {
				_ = a.ftsStore.Index(a.name, "", m.Role, m.Content, time.Now())
			}
		}
	}

	// Fire PostTurn hooks
	a.hooks.Run(ctx, &HookContext{
		AgentName:      a.name,
		Point:          PostTurn,
		Messages:       messages,
		TurnCount:      int(a.turnCount.Load()),
		ToolCallCount:  toolCallCount,
		Workspace:      a.homePath,
		UserID:         a.ownerUserID,
		Channel:        msg.Channel,
		AccountID:      msg.AccountID,
		ChatID:         msg.ChatID,
		Source:         msg.Source,
		GoalSessionKey: a.registry.GoalSessionKey(),
		IsPlanMode:     isPlanMode(msg.Params),
	})

	// Pending-skill notice: if skill_manage / skills_learner staged
	// anything during this turn (or earlier turns still awaiting
	// approval), surface the count + names to the chatter so they know
	// to run `fluctio skill approve`. Gated on user-driven sources so
	// cron / heartbeat turns don't spam the chat — mirrors
	// goalTriggerHook's allowedContinuationSources gate.
	if allowedContinuationSources[msg.Source] {
		if names, err := pendingSkillNames(a.homePath); err == nil && len(names) > 0 {
			emitEvent(ctx, ChatEvent{
				Type: "skill_pending",
				Data: map[string]any{
					"count": len(names),
					"names": names,
				},
			})
		} else if err != nil {
			slog.Warn("skill_pending notice: list failed", "agent", a.name, "error", err)
		}
	}

	// Auto-persist memory every N user turns. Single-user flatten dropped
	// the per-chatter durable DB counter (CountChatterUserMessages); the
	// cadence now keys on a.turnCount, the in-memory turn counter that
	// resets on daemon restart / agent eviction. With a single user that
	// only delays a persist by at most EveryNTurns — acceptable vs.
	// threading a session_key into runPostTurn just to re-count user
	// messages from the DB.
	willFire := false
	turns := int(a.turnCount.Load())
	if a.memoryCfg.AutoPersist.Enabled && a.memoryCfg.AutoPersist.EveryNTurns > 0 {
		willFire = turns > 0 && turns%a.memoryCfg.AutoPersist.EveryNTurns == 0
	}
	slog.Info("auto-persist gate",
		"agent", a.name,
		"enabled", a.memoryCfg.AutoPersist.Enabled,
		"turns", turns,
		"every_n_turns", a.memoryCfg.AutoPersist.EveryNTurns,
		"will_fire", willFire)
	if willFire {
		model := a.memoryCfg.AutoPersist.Model
		if model == "" {
			model = a.model
		}
		slog.Info("auto-persist firing", "agent", a.name, "model", model, "turns", turns, "messages", len(messages))
		go AutoPersistMemory(ctx, chatterMem, a.bgProvider(), model, messages)
	}

	// Auto-title: within the AfterRounds..AfterRounds+MaxTries window,
	// ask the LLM for a one-line summary and write it to sessions.title.
	// maybeAutoTitle bails when the title is already non-empty (user
	// renamed OR a previous run landed), so retries are cheap — one DB
	// lookup, no LLM. sessionUserTurns counts THIS session's user
	// messages from the in-memory slice runPostTurn already holds.
	// Auto-title: fire if the session has enough user turns. tryAutoTitle
	// is shared with the empty-response bail so a failed turn still names
	// the session — the title only needs the opening turns.
	a.tryAutoTitle(ctx, messages)

	// Skills learner
	if a.skillsLearner != nil {
		go func() {
			if err := a.skillsLearner.MaybeExtract(ctx, messages, toolCallCount); err != nil {
				slog.Debug("skills learner error", "error", err)
			}
		}()
	}
}

// HandleMessageStream processes a message through the ReAct loop and returns
// a StreamReader for the final response. Tool call iterations use non-streaming Chat;
// the final text response uses ChatStream for true SSE streaming.
func (a *Agent) HandleMessageStream(ctx context.Context, msg bus.InboundMessage) *provider.StreamReader {
	a.persistInboundImages(&msg)
	// Regex hooks: intercept messages matching a pattern and execute CLI
	// instead of the LLM.
	if reply, hookName, matched, feedToLLM := a.matchRegexHooks(ctx, msg.Text); matched {
		sess := a.sessions.Get(sessionTriple(msg, msg.ProjectID))
		userMsg := buildUserMessage(msg, a.model)
		toolCallMsg := provider.Message{Role: "assistant", Content: "", ToolCalls: []provider.ToolCall{{ID: "regex-hook-0", Type: "function", Function: provider.FunctionCall{Name: "regex_hook: " + hookName, Arguments: regexHookArgs(msg.Text)}}}, Timestamp: time.Now().UnixMilli()}
		toolResMsg := provider.Message{Role: "tool", ToolCallID: "regex-hook-0", Content: "matched"}
		replyMsg := provider.Message{Role: "assistant", Content: reply, Timestamp: time.Now().UnixMilli()}
		if feedToLLM {
			sess.BeginTurn()
			sess.Append(userMsg)
			sess.Append(toolCallMsg)
			sess.Append(toolResMsg)
			sess.Append(replyMsg)
			sess.EndTurn()
		} else {
			// FeedToLLM=false: archive the exchange hidden (llm_visible=0)
			// so it shows in web history but stays out of the LLM working
			// set / summary / recall. emitEvent below still surfaces it live.
			sess.AppendArchivedHidden([]provider.Message{userMsg, toolCallMsg, toolResMsg, replyMsg})
		}
		emitEvent(ctx, ChatEvent{Type: "tool_call", Data: map[string]any{"id": "regex-hook-0", "name": "regex_hook: " + hookName, "arguments": msg.Text}})
		emitEvent(ctx, ChatEvent{Type: "tool_result", Data: map[string]any{"id": "regex-hook-0", "name": "regex_hook: " + hookName, "result": "matched"}})
		emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": reply}})
		emitEvent(ctx, ChatEvent{Type: "done"})
		ch := make(chan provider.StreamChunk, 2)
		go func() {
			ch <- provider.StreamChunk{Content: reply, Done: true}
			close(ch)
		}()
		return provider.NewStreamReader(ch)
	}
	// Reuse setup logic from HandleMessage. Empty reply is "handled
	// but silent" — see the HandleMessage twin. Still emit a Done
	// chunk so callers waiting on the stream don't hang.
	if result := a.handleSlashCommand(msg); result.handled {
		if result.continueToLoop {
			// /yes (or /yolo with approved pending): emit the reply via
			// SSE so the user sees the "✅ authorized" ack, then fall
			// through to the streaming ReAct loop so drainApprovedPending
			// runs and the LLM continues with the authorized results.
			if result.reply != "" {
				emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": result.reply}})
			}
		} else {
			ch := make(chan provider.StreamChunk, 2)
			go func() {
				ch <- provider.StreamChunk{Content: result.reply, Done: true}
				close(ch)
			}()
			return provider.NewStreamReader(ch)
		}
	}

	// Quota gate — mirrors the check in HandleMessage.
	if rejection := a.checkQuota(ctx); rejection != "" {
		return a.stringStream(rejection)
	}

	chatterUID := a.chatterUserID(msg)
	// Tag ctx so DBStore session writes stamp chatter_user_id — see
	// the HandleMessage path for the rationale.
	ctx = store.WithChannel(ctx, msg.Channel)
	slog.Info("turn: refreshing skills",
		"agent", a.name, "channel", msg.Channel, "chat_id", msg.ChatID, "user", chatterUID)
	a.refreshSkillsFromStore(chatterUID)
	sess := a.sessions.Get(sessionTriple(msg, msg.ProjectID))
	// Bind chatter onto sess so its ctx() embeds WithChatterUserID
	// for DBStore session writes — Session.ctx() rebuilds ctx from its
	// own fields, so the chatter has to live on sess itself.
	{
		prov, mdl := provider.SplitProviderModel(a.model)
		sess.SetProviderModel(prov, mdl)
	}
	a.bindSession(ctx, msg.Channel, msg.AccountID, msg.ChatID, sess.SessionKey(), msg.ProjectID)
	a.registry.SetCallerIsAdmin(a.singleUser || a.isAdminChatter(msg))
	a.registry.SetCallerHostTrusted(a.isAdminChatter(msg))
	slog.Info("diag: identity gate (stream)", "agent", a.agentID, "singleUser", a.singleUser, "isAdmin", a.isAdminChatter(msg), "channel", msg.Channel, "msgUserID", msg.UserID, "ownerUserID", a.ownerUserID)
	a.registry.SetGoalSessionKey(sess.SessionKey())
	// Per-user file writes (USER.md / MEMORY.md) need to land in the
	// per-turn chatter's row, not the UserSpace owner — see
	// Registry.systemFileUserID for the routing rule.

	// Same orphan-tool_use safety net as HandleMessage. The streaming path
	// previously lacked this, so loop detection (which appends an assistant
	// tool_use + a system warn and breaks without ever running tools) and
	// any other premature exit between sess.Append(assistantMsg) and tool
	// result append left orphaned tool_use ids in the session. The next
	// turn's API request — especially against Anthropic-compat endpoints
	// like DeepSeek's /anthropic — then 400s with "tool_use ids were found
	// without tool_result blocks immediately after".
	defer sess.PadOrphanToolResultsAndMarkAborted(session.ToolResultStoppedNote)

	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: BeforeSystemPrompt, UserID: a.ownerUserID})
	chatterMem := a.memory
	systemPrompt := a.ctxBuilder.BuildSystemPromptAs(chatterUID, chatterMem, sessionStartTime(sess))
	a.logSystemPromptFingerprint(msg.Channel, msg.ChatID, chatterUID, systemPrompt)
	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: AfterSystemPrompt, UserID: a.ownerUserID})

	// Store raw user message — buildUserMessage handles multi-image
	// flatten + senderMetadata. Group msgs keep their `[SenderName]:`
	// prefix (applied in buildUserMessage); DMs stay bare.
	userMsg := buildUserMessage(msg, a.model)
	sess.Append(userMsg)

	sessionMsgs := sess.GetMessages()
	threshold := a.compactionThresholdNow(systemPrompt)
	// A forked session's LLM context is parent prefix + working set, so
	// the prefix eats part of the window — lower the trigger threshold
	// by the prefix's estimated tokens so B compacts its own working set
	// earlier instead of letting prefix+workset blow past the window.
	// The prefix itself is a read-only snapshot and is never pruned.
	if prefix := sess.ParentPrefixMessages(); len(prefix) > 0 {
		if pt := EstimateTokens(prefix); pt > 0 && threshold > pt {
			threshold -= pt
		}
	}
	compactResult, err := CompactMessages(sessionMsgs, a.homePath, a.bgProvider(), a.model, threshold)
	if err != nil {
		slog.Warn("compaction error", "agent", a.name, "error", err)
	}
	if compactResult != nil && compactResult.Pruned {
		sess.ReplaceMessages(compactResult.Messages)
		sessionMsgs = compactResult.Messages
		// Persist a topic summary so cross-session recall captures long
		// conversations the user never explicitly /compact'd or /new'd.
		a.maybeExtractSummary(sess, "auto-compaction")
		// Persist compaction notice (方案 B) + emit SSE event — Phase 3 Task 1.
		if compactResult.TokensBefore > 0 {
			text, meta := buildCompactionNotice(compactResult)
			sess.Append(provider.Message{
				Role:      "assistant",
				Content:   text,
				Metadata:  map[string]any{"compactionNotice": meta},
				Timestamp: time.Now().UnixMilli(),
			})
			emitEvent(ctx, ChatEvent{Type: "compaction_notice", Data: map[string]any{
				"content":        text,
				"before":         meta["before"],
				"after":          meta["after"],
				"retained_turns": meta["retained_turns"],
			}})
			// Broadcast notice text to IM channels (web session receives it
			// via the SSE event above; IM has no SSE so we push via the
			// outbound bus). Sent before the ReAct loop starts so the user
			// sees the notice before the agent's reply. Non-blocking send:
			// a full outbound queue drops the notice rather than stalling
			// the agent loop — matches sendMediaFiles pattern.
			if msg.Channel != "web" && isIMChannel(msg.Channel) && a.messageBus != nil {
				select {
				case a.messageBus.Outbound <- bus.OutboundMessage{
					Channel:   msg.Channel,
					AccountID: msg.AccountID,
					ChatID:    msg.ChatID,
					Text:      text,
				}:
				default:
					slog.Warn("outbound channel full, dropping compaction notice", "agent", a.name)
				}
			}
		}
	}

	messages := make([]provider.Message, 0, len(sessionMsgs)+4)
	messages = append(messages, provider.Message{Role: "system", Content: systemPrompt})
	if hints := renderChannelHints(msg, a.splitReplies); hints != "" {
		messages = append(messages, provider.Message{Role: "system", Content: hints})
	}
	if senderMsg := renderSender(msg); senderMsg != "" {
		messages = append(messages, provider.Message{Role: "system", Content: senderMsg})
	}
	if paramsMsg := renderClientParams(msg.Params); paramsMsg != "" {
		messages = append(messages, provider.Message{Role: "system", Content: paramsMsg})
	}
	if reminder := renderChatbotPersistenceReminder(a.promptMode, a.displayName, chatterMem.LoadUserFile(), chatterMem.LoadMemory()); reminder != "" {
		messages = append(messages, provider.Message{Role: "system", Content: reminder})
	}
	if msg.Source == bus.SourceCron {
		messages = append(messages, provider.Message{Role: "system", Content: cronTriggerGuidance})
	}
	if prefix := sess.ParentPrefixMessages(); len(prefix) > 0 {
		sessionMsgs = append(prefix, sessionMsgs...)
	}
	messages = append(messages, withConversationGapContext(sessionMsgs)...)

	toolDefs := a.registry.DefinitionsForMode(builtinAllowForMode(a.promptMode))

	type toolCallSig struct {
		name string
		hash [32]byte
	}
	var lastSig toolCallSig
	consecutiveCount := 0
	totalToolCalls := 0
	var kbSources []kb.KnowledgeSource // cached [K#] citation sources from this turn's KB retrieval
	ctx = kb.WithSourcesAccumulator(ctx, &kbSources)
	ctx = kb.WithSourceOrigin(ctx, kb.SourceOrigin{SessionID: sess.Key(), Seq: len(messages)}) // L1 dedup: same session+seq = rewrite of captured content
	citedMemos := make(map[int64]bool)
	ctx = tools.WithCitedSummaries(ctx, &citedMemos)

	// Drain user-authorized pending calls (/yes, /yolo) BEFORE the loop.
	totalToolCalls += a.drainApprovedPending(ctx, sess, &messages)

	// ReAct loop - use Chat for tool iterations
	for i := 0; i < a.maxToolIterations; i++ {
		hcBefore := &HookContext{AgentName: a.name, Point: BeforeModelCall, Messages: messages, Source: msg.Source, Channel: msg.Channel, AccountID: msg.AccountID, ChatID: msg.ChatID, UserID: a.ownerUserID}
		a.hooks.Run(ctx, hcBefore)
		if len(kbSources) == 0 && len(hcBefore.KnowledgeSources) > 0 {
			kbSources = hcBefore.KnowledgeSources
		}

		// KB auto-query hook: strict-mode SkipLLM short-circuit (stream
		// the prebuilt answer) + adopt rewritten messages (augment mode).
		if hcBefore.SkipLLM {
			ch := make(chan provider.StreamChunk, 2)
			ch <- provider.StreamChunk{Content: hcBefore.PrebuiltContent}
			ch <- provider.StreamChunk{Done: true}
			close(ch)
			return provider.NewStreamReader(ch)
		}
		messages = hcBefore.Messages

		dumpLLMRequest(a.name, a.model, messages, toolDefs)
		// Same PTL recovery as the non-streaming path: compact + retry on
		// context-length overflow so a long task survives instead of dying.
		var resp *provider.Response
		resp, messages, err = a.callLLMWithPTLRecovery(ctx, messages, toolDefs, func(ctx context.Context, msgs []provider.Message, tools []provider.Tool) (*provider.Response, error) {
			return a.provider.Chat(ctx, msgs, tools, a.model, a.maxTokens, a.temperature)
		})

		hcAfter := &HookContext{AgentName: a.name, Point: AfterModelCall, Messages: messages, Response: resp, Error: err, StartTime: hcBefore.StartTime, Channel: msg.Channel, AccountID: msg.AccountID, ChatID: msg.ChatID, UserID: a.ownerUserID, GoalSessionKey: a.registry.GoalSessionKey()}
		a.hooks.Run(ctx, hcAfter)

		if err != nil {
			slog.Error("LLM chat failed after retries", "agent", a.name, "error", err)
			return a.stringStream(slashT(msg.Lang, "error.processing_failed"))
		}
		a.meterTokens(ctx, sess.Key(), resp.Usage, 0)
		a.maybeRecoverToolCalls(resp)

		if !resp.HasToolCalls() {
			// Final response - use streaming
			sr, err := a.provider.ChatStream(ctx, messages, toolDefs, a.model, a.maxTokens, a.temperature)
			if err != nil {
				slog.Error("LLM stream failed, falling back", "agent", a.name, "error", err)
				sess.Append(provider.Message{Role: "assistant", Content: resp.Content})
				a.runPostTurn(ctx, msg, append(messages, provider.Message{Role: "assistant", Content: resp.Content}), totalToolCalls, chatterMem)
				return a.stringStream(resp.Content)
			}

			// Collect content in background for session storage.
			// Capture inbound msg + per-turn state out here — the goroutine
			// below shadows `msg` with the local assistant Message, and
			// runPostTurn needs the inbound (channel / chat_id / source).
			inboundMsg := msg
			messagesAtTurnStart := messages
			capturedToolCalls := totalToolCalls
			capturedChatterMem := chatterMem
			outCh := make(chan provider.StreamChunk, 64)
			outReader := provider.NewStreamReader(outCh)
			go func() {
				defer close(outCh)
				var full strings.Builder
				var thinking, thinkingSig string
				var rawAssistant json.RawMessage
				var streamUsage provider.Usage
				for {
					chunk, ok := sr.Next()
					if !ok {
						break
					}
					if chunk.Content != "" {
						full.WriteString(chunk.Content)
					}
					if chunk.Thinking != "" {
						thinking = chunk.Thinking
					}
					if chunk.ThinkingSignature != "" {
						thinkingSig = chunk.ThinkingSignature
					}
					if len(chunk.RawAssistant) > 0 {
						rawAssistant = chunk.RawAssistant
					}
					if chunk.Usage.InputTokens > 0 || chunk.Usage.OutputTokens > 0 ||
						chunk.Usage.CacheReadTokens > 0 || chunk.Usage.CacheCreationTokens > 0 {
						streamUsage = chunk.Usage
					}
					select {
					case outCh <- chunk:
					case <-ctx.Done():
						return
					}
				}
				a.meterTokens(ctx, sess.Key(), streamUsage, 0)
				msg := provider.Message{Role: "assistant", Content: full.String(), Thinking: thinking, Metadata: kbSourcesMetadata(kbSources)}
				switch {
				case len(rawAssistant) > 0:
					// Provider already serialized the assistant message
					// in its wire format (e.g. OpenAI/DeepSeek with
					// reasoning_content). Persist verbatim so the next
					// turn replays it byte-identically — required for
					// DeepSeek thinking mode.
					msg.RawAssistant = rawAssistant
				case thinking != "":
					// Anthropic extended thinking: pack {thinking, signature}
					// as a content-block so the next turn can echo it back.
					if raw, err := json.Marshal(map[string]string{
						"type":      "thinking",
						"thinking":  thinking,
						"signature": thinkingSig,
					}); err == nil {
						msg.RawAssistant = raw
					}
				}
				sess.Append(msg)
				// Fire PostTurn now that the assistant message is
				// persisted. Auto-persist (memory.go) lives behind
				// runPostTurn; without this call the streaming path
				// silently skipped it — see the FIXME at runPostTurn.
				a.runPostTurn(ctx, inboundMsg, append(messagesAtTurnStart, msg), capturedToolCalls, capturedChatterMem)
			}()
			return outReader
		}

		// Tool calls - process concurrently via SDK engine
		assistantMsg := provider.Message{
			Role:         "assistant",
			Content:      resp.Content,
			ToolCalls:    resp.ToolCalls,
			Thinking:     resp.Thinking,
			Metadata:     kbSourcesMetadata(kbSources),
			Timestamp:    time.Now().UnixMilli(),
			RawAssistant: resp.RawAssistant,
		}
		sess.Append(assistantMsg)
		messages = append(messages, assistantMsg)

		// Loop detection
		loopDetected := false
		for _, tc := range resp.ToolCalls {
			sig := toolCallSig{
				name: tc.Function.Name,
				hash: sha256.Sum256([]byte(tc.Function.Arguments)),
			}
			if sig.name == lastSig.name && sig.hash == lastSig.hash {
				consecutiveCount++
			} else {
				consecutiveCount = 1
				lastSig = sig
			}
			if consecutiveCount >= 3 {
				slog.Warn("tool loop detected", "agent", a.name, "tool", tc.Function.Name)
				warnMsg := provider.Message{
					Role:    "system",
					Content: "Loop detected: you called the same tool with the same arguments 3 times. Please try a different approach.",
				}
				sess.Append(warnMsg)
				messages = append(messages, warnMsg)
				loopDetected = true
				break
			}
		}
		if loopDetected {
			break
		}

		// Fire BeforeToolCall hooks
		for _, tc := range resp.ToolCalls {
			a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: BeforeToolCall, ToolName: tc.Function.Name, ToolArgs: tc.Function.Arguments, Channel: msg.Channel, AccountID: msg.AccountID, ChatID: msg.ChatID, UserID: a.ownerUserID})
		}

		// Auth gate: split into allowed vs. blocked/prompted. Blocked
		// calls get a synthetic tool_result; prompted calls park on the
		// session for /yes to drain next turn.
		streamCalls := resp.ToolCalls
		var authBlocked []toolCallResult
		authPrompted := false
		if a.authGate != nil {
			var blockedMap map[string]toolCallResult
			var promptDesc string
			streamCalls, blockedMap, promptDesc = a.filterAuthorizedCalls(sess, streamCalls)
			if promptDesc != "" {
				a.emitAuthPrompt(ctx, promptDesc, msg)
				authPrompted = true
			}
			for _, br := range blockedMap {
				authBlocked = append(authBlocked, br)
			}
		}
		// Execute tools concurrently via SDK engine
		results := a.engine.executeToolsConcurrently(ctx, a.registry, streamCalls, a.workspacePath)
		results = append(results, authBlocked...)
		totalToolCalls += len(results)

		for idx, r := range results {
			tc := resp.ToolCalls[idx]
			resultContent, meta := extractToolMeta(r.result)
			resultContent = annotateReachability(r.toolName, resultContent, a.registry)
			// Gate the structured-failure tag behind isFailedToolResult so that
			// successful results containing broad classifier substrings
			// ("port 5030", "timeout 5s config", "http 4xx docs") are NOT
			// mis-tagged. Mirrors the main (non-streaming) path's gate at loop.go:2737.
			if isFailedToolResult(r.err, resultContent) {
				if cat, hint := classifyToolError(resultContent); cat != "" {
					resultContent = resultContent + "\n[失败类别: " + cat + "] [可恢复: " + hint + "]"
				}
			}
			a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: AfterToolCall, ToolName: r.toolName, ToolResult: resultContent, Error: r.err, Channel: msg.Channel, AccountID: msg.AccountID, ChatID: msg.ChatID, UserID: a.ownerUserID, GoalSessionKey: a.registry.GoalSessionKey(), IsPlanMode: isPlanMode(msg.Params), Source: msg.Source})

			if r.err != nil {
				slog.Warn("tool execution error", "agent", a.name, "name", r.toolName, "error", r.err)
			}
			if r.toolName == "image_gen" {
				resultContent = a.persistImageGenOutput(ctx, msg.ChatID, msg.ProjectID, resultContent)
			}

			toolMsg := provider.Message{Role: "tool", Content: resultContent, ToolCallID: tc.ID, Name: r.toolName, Metadata: meta}
			sess.Append(toolMsg)
			messages = append(messages, toolMsg)
		}

		// Auth-prompted round: end the turn here. The auth_prompt event
		// already rendered tappable /yes /no /auto /yolo buttons in the
		// UI; calling the LLM again would just relay the holding
		// tool_result as "请回复 /yes" text — burying the buttons under
		// a redundant agent bubble. Mirrors the HandleMessage fix.
		if authPrompted {
			emitEvent(ctx, ChatEvent{Type: "done"})
			a.runPostTurn(ctx, msg, messages, totalToolCalls, chatterMem)
			return a.stringStream("")
		}
	}

	slog.Warn("max tool iterations reached — streaming forced final delivery", "agent", a.name, "max", a.maxToolIterations)
	return a.streamFinalDeliveryAfterCap(ctx, msg, messages, sess, totalToolCalls, chatterMem)
}

// streamFinalDeliveryAfterCap runs one extra ChatStream with tools
// disabled and a synthesis nudge, then persists the assistant message
// with iteration-cap metadata so the chat UI can badge the bubble.
// Returned StreamReader matches the contract of the normal "final
// response" branch above so callers don't need a special case.
func (a *Agent) streamFinalDeliveryAfterCap(ctx context.Context, inboundMsg bus.InboundMessage, messages []provider.Message, sess *session.Session, toolCallCount int, chatterMem *Memory) *provider.StreamReader {
	capMeta := iterationCapMetadata(a.maxToolIterations)
	finalMessages := append(messages, capReachedNudge(a.maxToolIterations))
	sr, err := a.provider.ChatStream(ctx, finalMessages, nil, a.model, a.maxTokens, a.temperature)
	if err != nil {
		// Streaming endpoint failed — persist+emit a fallback line
		// with the badge so the user still gets the signal.
		fallback := fmt.Sprintf("I've reached the maximum number of tool iterations (%d) and couldn't synthesize a final response. The work above represents what I gathered before hitting the limit.", a.maxToolIterations)
		fallbackMsg := provider.Message{Role: "assistant", Content: fallback, Metadata: capMeta, Timestamp: time.Now().UnixMilli()}
		sess.Append(fallbackMsg)
		emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": fallback, "metadata": capMeta}})
		a.runPostTurn(ctx, inboundMsg, append(messages, fallbackMsg), toolCallCount, chatterMem)
		return a.stringStream(fallback)
	}

	outCh := make(chan provider.StreamChunk, 64)
	outReader := provider.NewStreamReader(outCh)
	go func() {
		defer close(outCh)
		var full strings.Builder
		var thinking, thinkingSig string
		var rawAssistant json.RawMessage
		var streamUsage provider.Usage
		for {
			chunk, ok := sr.Next()
			if !ok {
				break
			}
			if chunk.Content != "" {
				full.WriteString(chunk.Content)
			}
			if chunk.Thinking != "" {
				thinking = chunk.Thinking
			}
			if chunk.ThinkingSignature != "" {
				thinkingSig = chunk.ThinkingSignature
			}
			if len(chunk.RawAssistant) > 0 {
				rawAssistant = chunk.RawAssistant
			}
			if chunk.Usage.InputTokens > 0 || chunk.Usage.OutputTokens > 0 ||
				chunk.Usage.CacheReadTokens > 0 || chunk.Usage.CacheCreationTokens > 0 {
				streamUsage = chunk.Usage
			}
			select {
			case outCh <- chunk:
			case <-ctx.Done():
				return
			}
		}
		a.meterTokens(ctx, sess.Key(), streamUsage, 0)
		content := full.String()
		if content == "" {
			content = fmt.Sprintf("I've reached the maximum number of tool iterations (%d) and couldn't synthesize a final response. The work above represents what I gathered before hitting the limit.", a.maxToolIterations)
		}
		finalMsg := provider.Message{
			Role:      "assistant",
			Content:   content,
			Thinking:  thinking,
			Metadata:  capMeta,
			Timestamp: time.Now().UnixMilli(),
		}
		switch {
		case len(rawAssistant) > 0:
			finalMsg.RawAssistant = rawAssistant
		case thinking != "":
			if raw, err := json.Marshal(map[string]string{
				"type":      "thinking",
				"thinking":  thinking,
				"signature": thinkingSig,
			}); err == nil {
				finalMsg.RawAssistant = raw
			}
		}
		sess.Append(finalMsg)
		// Out-of-band content event so SSE subscribers + chat_events
		// archive carry the cap-reached flag — chunks themselves don't
		// have a metadata field, so we publish it once here.
		emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{
			"content":  "",
			"metadata": capMeta,
		}})
		// Fire PostTurn so AutoPersist (and any future PostTurn hook)
		// runs on the streaming path too — see the no-tool-calls
		// branch in HandleMessageStream for the rationale.
		a.runPostTurn(ctx, inboundMsg, append(messages, finalMsg), toolCallCount, chatterMem)
	}()
	return outReader
}

// extractToolMeta strips a FC_META prefix (if present) from a tool result and
// returns the remaining content plus the parsed metadata. Today the only
// signal is whether exec ran in a sandbox. Keeping the helper shared so all
// tool-result handoff paths emit the same shape to the frontend.
func extractToolMeta(result string) (string, map[string]any) {
	if strings.HasPrefix(result, tools.MetaSandboxPrefix) {
		return strings.TrimPrefix(result, tools.MetaSandboxPrefix), map[string]any{"sandbox": true}
	}
	return result, nil
}

// writtenPathRE matches the path token in write_file / deliver_file result
// strings ("Written N bytes to <path>" / "Delivered N bytes to <path>").
// The leading verb alternation keeps it permissive across phrasings while
// the capture group is the raw path token.
var writtenPathRE = regexp.MustCompile(`(?:to|→|->)\s+(\S+)`)

// classifyToolError 对失败的工具结果文本做分类 + recovery 提示。
// 非失败文本（无 error 信号）返回 ("", "")。纯函数，便于测试。
func classifyToolError(content string) (category, hint string) {
	c := strings.ToLower(content)
	switch {
	case strings.Contains(c, "command not found") ||
		strings.Contains(c, "no such file or directory") ||
		strings.Contains(c, "executable file not found") ||
		strings.Contains(c, "not recognized as an internal or external command") ||
		strings.Contains(c, "no such file"):
		return "env_missing", "依赖的命令/文件在当前环境缺失；可换替代命令（如 Windows 用 powershell 替代 bash）、跳过该步并告知用户，或调 deliver_file 投递已有产物"
	case strings.Contains(c, "permission denied") ||
		strings.Contains(c, "access is denied") ||
		strings.Contains(c, "access denied"):
		return "permission", "权限不足；确认路径/权限，或换可见域内路径"
	case strings.Contains(c, "service unavailable") ||
		strings.Contains(c, "503") || strings.Contains(c, "500 internal") ||
		strings.Contains(c, "upstream_error") || strings.Contains(c, "timeout") ||
		strings.Contains(c, "context deadline exceeded") ||
		strings.Contains(c, "http 5") || strings.Contains(c, "http 4"):
		return "external", "外部服务错误；可退避重试、换备用服务，或告知用户稍后再试"
	case strings.Contains(c, "invalid argument") ||
		strings.Contains(c, "missing required") ||
		strings.Contains(c, "parse args") || strings.Contains(c, "bad request"):
		return "logic", "参数/逻辑错误；检查参数格式、路径合法性，或换实现方式"
	default:
		return "", ""
	}
}

// annotateReachability 对落盘型工具产物判定可见性，不可见则追加裁决行。
// 纯函数，便于测试。只在 SideWritesFile 工具上生效；其他工具原样返回。
func annotateReachability(toolName, resultContent string, reg *tools.Registry) string {
	if reg.SideEffectOf(toolName) != tools.SideWritesFile {
		return resultContent
	}
	visibleRoot := reg.UserRoot()
	var notes []string
	for _, m := range writtenPathRE.FindAllStringSubmatch(resultContent, -1) {
		p := strings.TrimRight(m[1], ".,;:\"'")
		visible, _ := reg.ReachabilityVerdict(p)
		if !visible {
			notes = append(notes, fmt.Sprintf(
				"[产物 %s 不在用户可见域 %s；可调 deliver_file(src=%q) 投递到可见域供用户查看]",
				p, visibleRoot, p))
		}
	}
	if len(notes) == 0 {
		return resultContent
	}
	return resultContent + "\n" + strings.Join(notes, "\n")
}

// capReachedNudge is the system message we append before the forced
// final delivery turn. Spells out two things: (a) tools are disabled
// for this call so don't try, (b) deliver the structured output the
// user asked for from whatever was already gathered, marking gaps
// explicitly rather than skipping fields. The model was generally
// burning the entire budget on exploration without ever circling back
// to synthesis — surfacing the constraint explicitly is the cheapest
// nudge that produces a usable artifact.
// looksTruncated is the Phase-3 heuristic for "the model hit max_tokens and
// got cut off mid-answer". FastClaw's provider.Response carries no finish_
// reason, so we infer from token usage: when maxTokens is set and the
// response consumed ≥90% of it without emitting any tool calls, the answer
// was very likely truncated. Conservative on purpose — the downside of a
// false positive is one harmless "please continue" round; the downside of a
// false negative is a half-delivered reply. The 90% floor avoids firing on
// ordinary long answers that happen to use a lot of tokens but still
// finished cleanly well under the cap.
func looksTruncated(resp *provider.Response, maxTokens int) bool {
	if resp == nil || maxTokens <= 0 {
		return false
	}
	if resp.HasToolCalls() {
		// A truncated tool_call is a different (and trickier) failure —
		// Phase 3 only handles text truncation. Tool-call truncation
		// surfaces as a JSON-parse failure in the executor and is
		// already classified by classifyToolError.
		return false
	}
	out := resp.Usage.OutputTokens
	if out <= 0 {
		// Provider didn't report usage — can't infer, don't fire.
		return false
	}
	return out >= maxTokens*9/10
}

// continuationNudge is the system message appended after a truncated
// assistant reply so the model finishes it on the next round. The just-
// produced (partial) assistant message is already in `messages` before this
// nudge, so the model sees its own cutoff text and can pick up exactly
// where it stopped rather than restarting the answer.
func continuationNudge() provider.Message {
	return provider.Message{
		Role: "system",
		Content: "Your previous response appears to have been cut off by the maximum output length. Continue exactly where you left off — do not repeat what you already wrote, just complete the remaining content and finish the answer.",
	}
}

// emptyResponseNudge is the system message appended when the model returns
// a completely empty response (no text, no tool calls). The empty turn is
// deliberately NOT recorded in `messages` — there is nothing to continue
// from — so the nudge asks for a direct answer from what the model already
// gathered this turn. Mirrors continuationNudge's transient lifecycle
// (messages-only, never persisted to the session).
func emptyResponseNudge() provider.Message {
	return provider.Message{
		Role: "system",
		Content: "Your previous response came back completely empty (no text and no tool calls), so the user received nothing and is still waiting. Answer the user's latest message now in plain text using the information you already have from this turn. Do not call any more tools unless the answer is impossible without one, and do not return an empty response again.",
	}
}

func capReachedNudge(maxIterations int) provider.Message {
	return provider.Message{
		Role: "system",
		Content: fmt.Sprintf(
			"You've used all %d tool-call iterations available for this turn. Tools are now disabled for this final response — do not attempt to call any. Synthesize what you've already gathered into the most complete deliverable you can: if the user asked for a structured artifact (table, list, ICP summary, email drafts, etc.), produce it now from the existing tool results. For any fields you couldn't resolve, mark them as 'unknown' / 'not found' / 'partial' rather than dropping rows or skipping the structure — give the user something usable plus an honest note about what's missing. Do not apologize without delivering content.",
			maxIterations,
		),
	}
}

// iterationCapMetadata is the assistant-side metadata stamped on the
// forced final-delivery message so the UI can badge the bubble. Kept
// as a constructor so the key name stays canonical across the streaming
// and non-streaming paths.
func iterationCapMetadata(maxIterations int) map[string]any {
	return map[string]any{
		"iterationCapReached": true,
		"iterationCapValue":   maxIterations,
	}
}

// stringStream creates a StreamReader that yields a single string.
func (a *Agent) stringStream(text string) *provider.StreamReader {
	ch := make(chan provider.StreamChunk, 2)
	go func() {
		ch <- provider.StreamChunk{Content: text, Done: true}
		close(ch)
	}()
	return provider.NewStreamReader(ch)
}

// HomePath returns the agent's home directory (identity/metadata).
func (a *Agent) HomePath() string {
	return a.homePath
}

// SplitReplies returns the effective per-agent split-reply setting
// — used by the gateway when constructing OutboundMessage so the WeChat
// adapter knows whether to honor SplitMessageMarker. Populated at
// agent boot from the merged config (per-agent override else system
// WeChatCfg.SplitReplies); refreshed on UpdateConfig.
func (a *Agent) SplitReplies() bool {
	return a.splitReplies
}

// RegisteredTools returns the live tool registry projection — name +
// description + source — for the dashboard's Tools tab. Reflects what
// THIS agent currently has loaded: built-ins always, plus any MCP or
// plugin tools attached at boot / hot-reload. Order is stable (builtins
// first, then MCP, then plugin, sorted by name within each group).
//
// Returns the FULL registry. Mode-based filtering happens client-side
// in the dashboard so the operator can see "what would be active in
// chatbot mode" without committing.
func (a *Agent) RegisteredTools() []tools.ToolInfo {
	if a.registry == nil {
		return nil
	}
	return a.registry.RegisteredTools()
}

// chatbotBuiltinAllowlist is the curated set of built-in tools exposed
// to the LLM in chatbot mode. Picked for IM-native companion / customer-
// support / role-play products:
//
//   - image_gen     : self-generated images (registered only if a
//     provider is configured; absence is fine)
//   - tts           : voice messages (same conditional registration)
//   - write_file    : persist USER.md / MEMORY.md when the LLM learns
//     something worth keeping. Routing in
//     systemFileUserID sends USER.md/MEMORY.md to the
//     per-chatter row, so each chatter accrues their
//     own profile / memory. Path resolution rejects
//     arbitrary paths via identityFileBlocked +
//     workspace scoping, so this isn't a general
//     "let the chatbot write anywhere" hole — just
//     the canonical per-chatter notes.
//   - edit_file     : same rationale; preferred over write_file when
//     surgically updating MEMORY.md so the model
//     doesn't accidentally clobber prior entries.
//
// Notably absent: `read_file` / `list_dir` — chatbot mode shouldn't
// browse the filesystem; USER.md / MEMORY.md content is already loaded
// into the system prompt by the bootstrap pass, so read tools would
// only enable poking at things the chatter shouldn't see. apply_patch
// is also out (multi-file batch is agent-mode territory).
//
// Also notably absent: `memory_search`. It scans
// <workspace>/memory/logs/*.jsonl, which chatbot mode never writes —
// so the tool ALWAYS returns "No matching entries found" and the
// model reads that as "I have no memory of you", overriding the
// in-prompt MEMORY.md section it should have trusted. Removing it
// forces the model to rely on the USER.md / MEMORY.md sections
// rendered into the system prompt, which is the only persistence
// path chatbot mode actually exposes.
//
// Notably absent — the `message` tool. The main reply is emitted via
// the LLM's normal `content` channel (the gateway's task callback turns
// that into an OutboundMessage automatically) and multi-bubble output
// uses SplitMessageMarker inline, not tool calls. Letting `message`
// into chatbot mode tempts the LLM into agent-style "I'll send a
// 'thinking...' message first, then my real reply" patterns that look
// jarring in a companion product. Operators who need OOB messaging
// (cron-triggered greetings, multi-recipient broadcasts) should fall
// back to `agent` mode or write a plugin.
//
// Still absent: scheduling (create_cron_job), delegation (delegate_task),
// start_app_preview — agent-loop machinery that doesn't belong in a
// chat persona. Add new built-ins here only when they're universally
// useful for chatbot products; everything else belongs in a plugin.
var chatbotBuiltinAllowlist = []string{
	"image_gen",
	"tts",
	"write_file",
	"edit_file",
	// set_timezone keeps "their local time" right for chat (greetings,
	// "晚安" timing) — chatbots need it as much as full agents do.
	"set_timezone",
	// Web tools let the chatbot answer real-time questions (weather,
	// news, prices, etc.) without requiring full agent mode.
	"web_search",
	"web_fetch",
	// exec + load_skill let the chatbot invoke installed skills
	// (e.g. image generation, data lookup). Skills are the primary
	// extension mechanism — without exec the chatbot can't run them.
	"exec",
	"load_skill",
}

// builtinAllowForMode returns the built-in tool name allowlist for the
// given prompt mode. Plugin / MCP tools are always included regardless
// — see Registry.DefinitionsForMode. nil means "all built-ins";
// []string{} means "no built-ins"; a non-empty slice means "only these".
func builtinAllowForMode(mode string) []string {
	switch mode {
	case config.PromptModeChatbot:
		return chatbotBuiltinAllowlist
	case config.PromptModeCustomize:
		return []string{} // explicit empty — no built-ins
	default: // agent (or empty/unknown — defaults to agent for back-compat)
		return nil // nil = all built-ins exposed
	}
}

// WorkspacePath returns the agent's working directory for user-facing files.
func (a *Agent) WorkspacePath() string {
	return a.workspacePath
}

// chatterLocation resolves the effective timezone for a chatter via
// scope prefs (chatter pref → agent default → system default). Server-
// local when no relational store is wired or nothing is configured —
// the legacy single-tenant behavior. Passed to the ContextBuilder as
// the tzResolver so the system prompt's date line renders in the
// chatter's wall clock; the cron tool runs the same resolution at
// job-creation time.
func (a *Agent) chatterLocation(chatterUID string) *time.Location {
	// USER.md is the chatter-authoritative source: the deployment clock is
	// UTC and inbound timestamps are UTC, so the only place the chatter's
	// real timezone lives is what they (or the agent) recorded in their
	// profile — "东八区", "UTC+8", "Asia/Shanghai". Parse it and let it win
	// over the DB prefs, so editing USER.md is enough to fix the clock
	// without also having to run set_timezone.
	if a.memory != nil {
		if profile := a.memory.LoadUserFile(); profile != "" {
			if loc := scope.LocationFromText(profile); loc != nil {
				return loc
			}
		}
	}
	if a.dataStore == nil {
		return time.Local
	}
	tz := scope.Timezone(context.Background(), a.dataStore, chatterUID, a.agentID)
	return scope.LoadLocationOrLocal(tz)
}

const conversationGapThreshold = 24 * time.Hour

// withConversationGapContext keeps message bodies clean while still telling
// the model when the latest turn resumes a stale conversation. Timestamps stay
// in message metadata and are never rendered as user-visible text.
//
// It also filters out compaction-notice bubbles (UI-only assistant turns whose
// text would otherwise reach the LLM as a fake assistant turn and pollute
// context); Metadata itself is stripped by provider serializers, but the
// Content would still leak.
func withConversationGapContext(msgs []provider.Message) []provider.Message {
	filtered := make([]provider.Message, 0, len(msgs))
	for _, m := range msgs {
		if _, ok := m.Metadata["compactionNotice"]; ok {
			continue
		}
		filtered = append(filtered, m)
	}
	msgs = filtered

	if len(msgs) < 2 {
		return msgs
	}
	latest := msgs[len(msgs)-1]
	if latest.Role != "user" || latest.Timestamp <= 0 {
		return msgs
	}

	var previousTimestamp int64
	for i := len(msgs) - 2; i >= 0; i-- {
		if msgs[i].Timestamp > 0 && (msgs[i].Role == "user" || msgs[i].Role == "assistant") {
			previousTimestamp = msgs[i].Timestamp
			break
		}
	}
	gap := time.Duration(latest.Timestamp-previousTimestamp) * time.Millisecond
	if previousTimestamp == 0 || gap < conversationGapThreshold {
		return msgs
	}

	note := fmt.Sprintf(
		"Conversation timing context: the latest user message arrived after %s of inactivity. "+
			"Treat it as a resumed conversation in the current moment. Use earlier messages as background, "+
			"but do not assume their time-sensitive situation is still current and do not repeat an earlier answer unless the user asks for it. "+
			"Keep this timing context silent; do not mention the gap or report timestamps.",
		formatConversationGap(gap),
	)
	out := make([]provider.Message, 0, len(msgs)+1)
	out = append(out, provider.Message{Role: "system", Content: note})
	out = append(out, msgs...)
	return out
}

func formatConversationGap(gap time.Duration) string {
	days := int(gap / (24 * time.Hour))
	if days >= 2 {
		return fmt.Sprintf("about %d days", days)
	}
	return "more than a day"
}

// UpdateConfig updates the agent's runtime config (model, temperature, etc.)
func (a *Agent) UpdateConfig(rc config.ResolvedAgent) {
	a.model = rc.Model
	a.maxTokens = rc.MaxTokens
	a.temperature = rc.Temperature
	a.maxToolIterations = rc.MaxToolIterations
	a.maxParallelToolCalls = rc.MaxParallelToolCalls
	a.language = rc.Language
	// Phase 2 compaction fields — keep in sync with buildAgent injection.
	a.contextWindow = rc.ContextWindow
	a.compactionMode = rc.CompactionMode
	a.compactionThreshold = rc.CompactionThreshold
	// Sandbox flags drive the system prompt's "Working Directory" / "home
	// dir" description and the sandbox-capabilities block. Without this
	// propagation an agent that existed before sandbox was enabled keeps
	// telling the LLM its home is the host absolute path, even after the
	// executor itself has been swapped to Docker — model dutifully calls
	// list_dir /Users/idoubi/.fluctio/agents/<id>/agent and 404s in the
	// container.
	a.ctxBuilder.sandboxEnabled = rc.Sandbox.Enabled
	a.ctxBuilder.sandboxBackend = rc.Sandbox.Backend
	// Propagate per-agent prompt mode updates from dashboard saves.
	// Without this, an operator switching an agent to chatbot mode in
	// the UI would have to restart the binary for the change to take
	// effect. The tool filter follows promptMode automatically via
	// builtinAllowForMode at request time, so no separate hot-reload
	// hook is needed for the tool surface.
	a.promptMode = rc.PromptMode
	a.ctxBuilder.SetPromptMode(rc.PromptMode)
	// Per-agent WeChat split-replies. Nil override = keep whatever the
	// system layer initialized at boot (don't reset to false). Non-nil
	// = authoritative for this agent.
	if rc.SplitReplies != nil {
		a.splitReplies = *rc.SplitReplies
	}
}

// chatterUserID picks the per-message chatter identity, falling back
// to the agent owner when the inbound message doesn't carry one
// (legacy channels, system-injected events, …). This is what we use
// as the per-user skills bucket key and the sandbox bind-mount target,
// so two different chatters of the same agent each see their own
// personal skill set and write installs into their own host dir.
// sessionTriple returns the (channel, accountID, chatID, projectID)
// arguments for sessions.Get. When SharedIdentity is enabled on the
// inbound message, the triple is replaced with a virtual one so all
// channels converge on the same session.
func sessionTriple(msg bus.InboundMessage, projectID string) (string, string, string, string) {
	ch, acc, cid := msg.SessionTriple()
	return ch, acc, cid, projectID
}

func (a *Agent) chatterUserID(msg bus.InboundMessage) string {
	// Single-user flatten: every chatter is the owner. Kept as a thin
	// shim so call sites compile while the surrounding chatter plumbing
	// (sandbox.WithUserID / memory.WithUserID / registry.SetChatterUserID)
	// is removed in follow-up commits.
	return a.ownerUserID
}

// buildSkillGate projects a loaded-skill slice into the gate map that
// load_skill consumes. We build this from the SAME []Skill that
// BuildSkillsSummary already read from — so load_skill's gating banner
// always matches the system-prompt catalog (single source of truth:
// SkillsLoader → CheckGating). Re-calling LoadSkills here would parse
// frontmatter twice and could race with a concurrent OSS sync.
func buildSkillGate(skills []Skill) map[string]tools.SkillGate {
	if len(skills) == 0 {
		return nil
	}
	out := make(map[string]tools.SkillGate, len(skills))
	for _, s := range skills {
		if !s.Gated && s.OnMissing == "" {
			continue
		}
		out[s.Name] = tools.SkillGate{
			Gated:     s.Gated,
			Reason:    s.GateReason,
			OnMissing: s.OnMissing,
		}
	}
	return out
}

// refreshSkillsFromStore mirrors OSS-hosted skills (global, per-agent,
// and per-user) to the local filesystem and rebuilds the skills summary
// baked into the system prompt. No-op when no workspace store is
// configured. Called at the top of every turn so a skill uploaded
// after pod start — or on a sibling replica — becomes visible here on
// the next message instead of requiring a pod restart.
//
// userID identifies whose per-user skill bucket to merge into the set;
// pass the chatter (not the agent owner) so a skill chatter A installs
// is visible only to chatter A even when both chat the same agent. Empty
// disables the per-user layer.
func (a *Agent) refreshSkillsFromStore(userID string) {
	if a.workspaceStore == nil {
		// IM-vs-web "missing agent skills" diagnostic: when this fires
		// on an IM turn but not the matching web turn for the same
		// agent, the chatter's UserSpace was built without a workspace
		// store, so agent-scope OSS skills never hydrate. Warn (not
		// debug) so it surfaces in default-level prod logs.
		slog.Warn("refresh skills skipped: no workspace store",
			"agent", a.name, "agentID", a.agentID, "user", userID)
		return
	}
	loader := NewSkillsLoaderWithGlobal(a.homeDir, a.homePath, a.skillsCfg, a.globalSkillsCfg).
		WithObjectStore(a.workspaceStore, a.agentID)
	skills := loader.LoadSkills()
	summary := loader.BuildSkillsSummary(skills)
	a.ctxBuilder.SetSkillsSummary(summary)
	tools.RegisterLoadSkill(a.registry, loader.AllSkillDirs(), buildSkillGate(skills))
	// Phase 4 write-approval gate: same wiring as the boot-time site above.
	// a.homePath == rc.Home (the agent home containing skills/).
	tools.RegisterSkillManage(a.registry, a.homePath, "fluctio skill approve",
		func(b []byte) *tools.SkillManifest {
			fm := parseFrontmatterFromBytes(b)
			if fm == nil {
				return nil
			}
			var meta *SkillMetadata
			if fm.Metadata.Kind != 0 {
				meta = parseMetadata(&fm.Metadata)
			}
			gated, reason := CheckGating(meta)
			return &tools.SkillManifest{
				Name:        fm.Name,
				Description: fm.Description,
				Gated:       gated,
				GateReason:  reason,
			}
		})
	// Per-turn fingerprint of the skill set the system prompt will
	// ship. Lets us diff IM vs web for the same (agent, chatter) and
	// confirm — or rule out — that agent-scope skills are reaching
	// every channel. count==bundled-only is the "missing agent skills"
	// signature.
	names := make([]string, 0, len(skills))
	for _, s := range skills {
		names = append(names, s.Name)
	}
	slog.Info("skills summary refreshed",
		"agent", a.name, "agentID", a.agentID, "user", userID,
		"count", len(skills), "summary_bytes", len(summary), "names", names)
}

// ReloadWorkspaceFiles re-reads workspace .md files (SOUL.md, AGENTS.md, etc.)
// and rebuilds the context builder.
func (a *Agent) ReloadWorkspaceFiles() {
	if a.memoryStore != nil {
		a.memory = NewMemoryWithStoreForUser(a.homePath, a.memoryStore, a.ownerUserID, a.name)
	} else {
		a.memory = NewMemory(a.homePath)
	}
	// Rebuild skills summary. When a workspace store is configured,
	// LoadSkills first hydrates global + per-agent + per-user skill dirs
	// from object storage so skills uploaded on another replica (or
	// post-boot on this one) become visible.
	loader := NewSkillsLoaderWithGlobal(a.homeDir, a.homePath, a.skillsCfg, a.globalSkillsCfg)
	if a.workspaceStore != nil {
		loader.WithObjectStore(a.workspaceStore, a.agentID)
	}
	skills := loader.LoadSkills()
	skillsSummary := loader.BuildSkillsSummary(skills)
	tools.RegisterLoadSkill(a.registry, loader.AllSkillDirs(), buildSkillGate(skills))
	// Phase 4 write-approval gate: same wiring as the boot-time site above.
	tools.RegisterSkillManage(a.registry, a.homePath, "fluctio skill approve",
		func(b []byte) *tools.SkillManifest {
			fm := parseFrontmatterFromBytes(b)
			if fm == nil {
				return nil
			}
			var meta *SkillMetadata
			if fm.Metadata.Kind != 0 {
				meta = parseMetadata(&fm.Metadata)
			}
			gated, reason := CheckGating(meta)
			return &tools.SkillManifest{
				Name:        fm.Name,
				Description: fm.Description,
				Gated:       gated,
				GateReason:  reason,
			}
		})
	a.ctxBuilder = NewContextBuilder(a.homePath, a.memory, skillsSummary)
	a.ctxBuilder.SetWorkspace(a.workspacePath)
	a.ctxBuilder.SetPromptMode(a.promptMode)
	a.ctxBuilder.SetDisplayName(a.displayName)
	// Preserve Store-backed identity reads across reload; without this,
	// Postgres-mode pods silently fall back to pod-local filesystem.
	// userID must also be re-pinned — the DB store requires a non-empty
	// user_id to scope the SOL/IDENTITY/AGENTS reads, and without it
	// the ContextBuilder's loadFile pass would fail on every shared
	// identity file after a reload (manifest as an "agent without a
	// name/soul" greeting).
	if a.memoryStore != nil {
		a.ctxBuilder.store = a.memoryStore
		a.ctxBuilder.agentID = a.agentID
		a.ctxBuilder.userID = a.ownerUserID
	}
	// Chatter-timezone date line — same re-apply rule as the Store
	// wiring above: the rebuilt ContextBuilder starts with a nil
	// resolver and would silently fall back to server-local time.
	if a.dataStore != nil {
		a.ctxBuilder.SetTimezoneResolver(a.chatterLocation)
	}
}

// downloadMediaURL fetches a media URL (图床, e.g. image_gen output) with a
// timeout and returns the bytes. Used by fetchImageBytes for inbound image
// attachments (data: URLs are base64-decoded separately in fetchImageBytes).
// SSRF-guarded client + the attachment size cap: the URL comes from message
// content / tool output, either of which an attacker can influence.
func downloadMediaURL(url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := attachmentFetchClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxAttachmentBytes+1))
}

// mcpSafeName mirrors mcp.prefixToolName's server-name sanitization so the
// agent loop can reverse-resolve a server config from a prefixed MCP tool
// name without reaching into the mcp package's unexported helper. Keep in
// sync with mcp.prefixToolName when changing that.
func mcpSafeName(server string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return '_'
	}, server)
}

// lookupMCPServer 反查 toolName（形如 mcp_<safeServer>_<raw>）所属的 server
// 配置。返回 ("", 零值) 表示未匹配——调用方应回退到 SideExternal 默认启发式。
// 由于 safeServer 把非字母数字字符也替换成 '_'，server 名本身包含 '_' 时理论
// 上会有歧义；按 a.mcpServers 的 key 顺序取首个匹配前缀。生产配置中 server
// 名通常唯一可识别（如 "Playwright"、"zread"），歧义极低。
func lookupMCPServer(toolName string, servers map[string]config.MCPServerConfig) (string, config.MCPServerConfig) {
	for name, cfg := range servers {
		if strings.HasPrefix(toolName, "mcp_"+mcpSafeName(name)+"_") {
			return name, cfg
		}
	}
	return "", config.MCPServerConfig{}
}

// mcpDefaultEffect 决定一个 MCP 工具的副作用：
//
//	cfg.ToolEffects[rawToolName]  >  cfg.Effect  >  启发式
//
// 启发式：toolName 小写包含 "screenshot" / "browser_take"（Playwright 截图类）
// 视为 SideWritesFile（annotateReachability 会检查其产物是否在可见域）；
// 其余 MCP 工具默认 SideExternal。
// rawToolName = 去掉 "mcp_<safeServer>_" 前缀。
func mcpDefaultEffect(serverName, toolName string, cfg config.MCPServerConfig) tools.SideEffect {
	raw := strings.TrimPrefix(toolName, "mcp_"+mcpSafeName(serverName)+"_")
	if e, ok := cfg.ToolEffects[raw]; ok {
		// 显式 "pure" 才生效；其他无法识别的值（parseSideEffect 默认 SidePure）
		// 视为未配置，继续向后回退。
		if se := parseSideEffect(e); se != tools.SidePure || e == "pure" {
			return se
		}
	}
	if cfg.Effect != "" {
		return parseSideEffect(cfg.Effect)
	}
	low := strings.ToLower(toolName)
	if strings.Contains(low, "screenshot") || strings.Contains(low, "browser_take") {
		return tools.SideWritesFile
	}
	return tools.SideExternal
}

// parseSideEffect 把配置中的字符串副作用声明映射到 SideEffect 枚举。
// 无法识别的值返回 SidePure（与零值一致；mcpDefaultEffect 会区分 "pure" 与
// 未配置）。
func parseSideEffect(s string) tools.SideEffect {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "writes_file":
		return tools.SideWritesFile
	case "emits_inline":
		return tools.SideEmitsInline
	case "external":
		return tools.SideExternal
	default:
		return tools.SidePure
	}
}
