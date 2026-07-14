package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/usage"
)

// slashResult holds the result of a slash command.
//
// continuationQueued flags slashes that pushed a follow-up message onto
// bus.Inbound (currently /goal foo and /goal resume). HandleMessage uses
// it to emit a `turn_pending` event instead of `done`, which keeps the
// caller's SSE stream open until the continuation's own `done` arrives —
// so the typing indicator stays visible during the model-thinking gap.
type slashResult struct {
	handled            bool
	reply              string
	continuationQueued bool
	// continueToLoop: when true, the slash still enters the agent loop
	// after its reply (rather than short-circuiting). Used by /yes (and
	// /auto / /yolo when they approve pending calls) so drainApprovedPending
	// runs and the authorized calls execute immediately, then the LLM
	// continues the task with their results.
	continueToLoop bool
}

// handleSlashCommand checks if the message is a slash command and handles it.
func (a *Agent) handleSlashCommand(msg bus.InboundMessage) slashResult {
	text := strings.TrimSpace(msg.Text)
	if !strings.HasPrefix(text, "/") {
		return slashResult{}
	}

	parts := strings.Fields(text)
	cmd := strings.ToLower(parts[0])
	// Strip @botname suffix: /status@mybot → /status
	if idx := strings.Index(cmd, "@"); idx > 0 {
		cmd = cmd[:idx]
	}
	args := parts[1:]

	// Owner-only gate for write commands. Read-only inspections (/status,
	// /usage, /insights, /help, /version, /start, /whoami) stay open so
	// any group member can self-serve info. Mutators that change the
	// agent's runtime state (model, personality) or shared group-session
	// history are restricted to the agent owner + per-channel admin
	// allowlist. A DM chatter may start a fresh copy of their own session
	// with /new or /reset; those commands don't affect anybody else there.
	if slashRequiresAdmin(cmd, msg) && !a.isAdminChatter(msg) {
		return slashResult{
			handled: true,
			reply:   slashTf(msg.Lang, "admin.denied", cmd, msg.Channel),
		}
	}

	switch cmd {
	case "/start":
		return slashResult{
			handled: true,
			reply:   slashTf(msg.Lang, "start.greeting", a.name),
		}

	case "/claim":
		return a.slashClaim(msg)

	case "/new", "/reset":
		// Clear any goal attached to the OLD session_key — design
		// §6 chose "fresh session = clean state" over "goal follows
		// chat". Runs before the web short-circuit too, so frontend-
		// driven /new also reaps the goal row.
		if a.goalStore != nil {
			oldKey := a.resolveSessionKey(msg)
			a.clearGoalForSession(oldKey)
		}
		if msg.Channel == "web" {
			// For web channel, don't delete the session file — frontend handles new session creation
			return slashResult{handled: true, reply: "__NEW_SESSION__"}
		}
		// Snapshot the soon-to-be-closed session into cross-session
		// recall before minting the fresh one.
		if oldSess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID); oldSess != nil {
			a.maybeExtractSummary(oldSess, "new_session")
		}
		// Mint a fresh session under the same (channel, account, chat)
		// triple so this conversation thread starts blank but the prior
		// thread is preserved as history. Subsequent inbound messages
		// resolve to the new (max updated_at) row via Manager.Get's
		// active-session lookup.
		a.sessions.OpenNewSession(msg.Channel, msg.AccountID, msg.ChatID)
		return slashResult{handled: true, reply: slashT(msg.Lang, "new.done")}

	case "/retry":
		return a.slashRetry(msg)

	case "/undo":
		return a.slashUndo(msg)

	case "/compact":
		return a.slashCompact(msg)

	case "/yes":
		return a.slashAuthReply(msg, true)
	case "/no":
		return a.slashAuthReply(msg, false)
	case "/yolo", "/auto", "/ask":
		return a.slashSetAuthMode(msg, strings.TrimPrefix(cmd, "/"))

	case "/status":
		return a.slashStatus(msg)

	case "/usage":
		return a.slashUsage(msg)

	case "/insights":
		days := 7
		if len(args) > 0 {
			fmt.Sscanf(args[0], "%d", &days)
		}
		return a.slashInsights(msg, days)

	case "/personality":
		if len(args) == 0 {
			return a.slashPersonalityList(msg)
		}
		return a.slashPersonalitySet(msg, args[0])

	case "/model":
		if len(args) == 0 {
			return slashResult{handled: true, reply: slashTf(msg.Lang, "model.current", a.model)}
		}
		return a.slashModel(msg, args[0])

	case "/goal":
		return a.slashGoal(msg, args)

	case "/plan":
		return a.slashPlan(msg, args)

	case "/help":
		return slashResult{handled: true, reply: a.slashHelp(msg.Lang)}

	case "/version":
		return slashResult{handled: true, reply: slashTf(msg.Lang, "version", a.name, a.model)}

	case "/whoami":
		return slashResult{
			handled: true,
			reply: slashTf(msg.Lang, "whoami", msg.Channel, msg.UserID, msg.SenderName, msg.Channel),
		}

	default:
		return slashResult{}
	}
}

// slashAuthReply handles /yes and /no. /yes pops the waiting calls and
// marks them approved — the agent loop executes them at the top of THIS
// turn (the /yes message itself drives the continuation via continueToLoop)
// and feeds results back to the LLM. /no clears them. A stray /yes with
// nothing pending tells the user there's nothing to confirm, so it
// doesn't look like it silently did nothing.
func (a *Agent) slashAuthReply(msg bus.InboundMessage, approved bool) slashResult {
	sess := a.sessions.Get(sessionTriple(msg, msg.ProjectID))
	if sess == nil {
		return slashResult{handled: true, reply: slashT(msg.Lang, "auth.no_session")}
	}
	pending := sess.PopPendingCalls()
	if len(pending) == 0 {
		if approved {
			return slashResult{handled: true, reply: slashT(msg.Lang, "auth.nothing_pending_yes")}
		}
		return slashResult{handled: true, reply: slashT(msg.Lang, "auth.nothing_pending_no")}
	}
	if approved {
		sess.SetApprovedPending(pending)
		return slashResult{handled: true, continueToLoop: true, reply: slashTf(msg.Lang, "auth.approved", len(pending))}
	}
	return slashResult{handled: true, reply: slashTf(msg.Lang, "auth.denied", len(pending))}
}

// slashSetAuthMode switches the authorization mode (ask/auto/yolo) and
// re-judges any pending calls under it: the ones the new mode allows get
// approved (executed immediately via drainApprovedPending on this
// continuation turn), the rest are dropped. /yes semantics for survivors
// still apply.
func (a *Agent) slashSetAuthMode(msg bus.InboundMessage, mode string) slashResult {
	sess := a.sessions.Get(sessionTriple(msg, msg.ProjectID))
	if sess == nil {
		return slashResult{handled: true, reply: slashT(msg.Lang, "auth.no_session")}
	}
	a.authMode = mode
	desc := slashT(msg.Lang, "authmode.desc."+mode)
	// Re-judge pending calls under the new mode (yolo→all, auto→none,
	// ask→re-prompt on next attempt). Newly-allowed ones execute
	// immediately via drainApprovedPending on this continuation turn.
	var approved []provider.ToolCall
	if a.authGate != nil {
		pending := sess.PopPendingCalls()
		for _, tc := range pending {
			dec := a.authGate.evaluateCall(tc.Function.Name, tc.Function.Arguments, mode)
			if dec.action == authAllow {
				approved = append(approved, tc)
			}
		}
		if len(approved) > 0 {
			sess.SetApprovedPending(approved)
		}
	}
	reply := slashTf(msg.Lang, "authmode.set", mode, desc)
	return slashResult{handled: true, continueToLoop: len(approved) > 0, reply: reply}
}

// writeSlashCommands are the slash commands that mutate the agent's runtime
// state or session history and therefore need the owner/admin gate. Anything
// not in this set is treated as read-only and runs unrestricted.
var writeSlashCommands = map[string]bool{
	"/new":         true,
	"/reset":       true,
	"/undo":        true,
	"/retry":       true,
	"/compact":     true,
	"/yolo":        true,
	"/auto":        true,
	"/ask":         true,
	"/yes":         true,
	"/no":          true,
	"/model":       true,
	"/personality": true,
}

// slashRequiresAdmin keeps agent-wide mutations owner/admin-only and also
// protects shared group history. Starting a fresh private session is a
// per-chatter operation, so /new and /reset stay available outside groups.
func slashRequiresAdmin(cmd string, msg bus.InboundMessage) bool {
	if !writeSlashCommands[cmd] {
		return false
	}
	if (cmd == "/new" || cmd == "/reset") && msg.PeerKind != "group" {
		return false
	}
	return true
}

// isAdminChatter decides whether the chatter is allowed to run a write-mode
// slash command on this channel.
//
// Web / api: the chatter's UserID is the Fluctio user UUID — owner is
// identified by direct equality with the agent's ownerUserID. No
// per-platform allowlist needed.
//
// IM channels (discord, telegram, slack, ...): UserID is the platform's
// own user ID (Discord snowflake, Telegram numeric ID, ...), which has
// no inherent link to the agent's Fluctio owner. The owner registers
// platform IDs in agent.json's `admins[channel]` to grant access — and,
// to keep single-user dev installs from being locked out of their own
// agent, an empty/absent allowlist for the channel falls through to
// "anyone can run it" (the legacy behavior). Operators who care about
// group-chat protection populate the list to lock it down.
func (a *Agent) isAdminChatter(msg bus.InboundMessage) bool {
	// Web / api carry Fluctio UUIDs directly; owner check is sufficient.
	if msg.Channel == "web" || msg.Channel == "api" {
		return msg.UserID != "" && msg.UserID == a.ownerUserID
	}
	list, ok := a.admins[msg.Channel]
	if !ok || len(list) == 0 {
		// No allowlist configured for this channel. Fall back to
		// ownership check: if the IM chatter's resolved Fluctio
		// user_id matches the agent owner, they're admin. Otherwise
		// deny — an unconfigured allowlist should NOT grant admin
		// to every anonymous chatter on a public-facing IM channel.
		return msg.UserID != "" && msg.UserID == a.ownerUserID
	}
	for _, id := range list {
		if id == msg.UserID {
			return true
		}
	}
	return false
}

// slashRetry re-runs the last user message, discarding the last assistant response.
func (a *Agent) slashRetry(msg bus.InboundMessage) slashResult {
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	msgs := sess.GetMessages()

	// Find the last user message
	lastUserIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx < 0 {
		return slashResult{handled: true, reply: slashT(msg.Lang, "retry.empty")}
	}

	// Save snapshot for undo
	sess.Snapshot()

	// Trim to just before the last user message
	sess.ReplaceMessages(msgs[:lastUserIdx])

	// Re-inject the user message as a new inbound
	lastUserText := msgs[lastUserIdx].Content
	retryMsg := msg
	retryMsg.Text = lastUserText

	// Signal that we want to re-process this message (return not-handled so gateway retries)
	// But we return handled here to avoid double-processing — gateway should re-send
	return slashResult{
		handled: true,
		reply:   slashTf(msg.Lang, "retry.doing", truncateSlash(lastUserText, 80)),
	}
}

// slashUndo reverts the last assistant response.
func (a *Agent) slashUndo(msg bus.InboundMessage) slashResult {
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)

	if !sess.HasSnapshot() {
		// No snapshot — try to remove last user+assistant turn manually
		msgs := sess.GetMessages()
		if len(msgs) < 2 {
			return slashResult{handled: true, reply: slashT(msg.Lang, "undo.nothing")}
		}
		// Trim trailing assistant messages + the user message before them
		end := len(msgs)
		for end > 0 && msgs[end-1].Role == "assistant" {
			end--
		}
		if end > 0 && msgs[end-1].Role == "user" {
			end--
		}
		sess.ReplaceMessages(msgs[:end])
		return slashResult{handled: true, reply: slashT(msg.Lang, "undo.done_turn")}
	}

	if sess.Undo() {
		return slashResult{handled: true, reply: slashT(msg.Lang, "undo.done_action")}
	}
	return slashResult{handled: true, reply: "Nothing to undo."}
}

func (a *Agent) slashCompact(msg bus.InboundMessage) slashResult {
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	sessionMsgs := sess.GetMessages()

	if len(sessionMsgs) == 0 {
		return slashResult{handled: true, reply: slashT(msg.Lang, "compact.empty")}
	}

	result, err := CompactMessages(sessionMsgs, a.homePath, a.provider, a.model)
	if err != nil {
		return slashResult{handled: true, reply: slashTf(msg.Lang, "compact.error", err)}
	}
	if result != nil && result.Pruned {
		sess.ReplaceMessages(result.Messages)
		a.maybeExtractSummary(sess, "compaction")
		return slashResult{handled: true, reply: slashTf(msg.Lang, "compact.done", len(sessionMsgs), len(result.Messages))}
	}
	return slashResult{handled: true, reply: slashT(msg.Lang, "compact.skip")}
}

func (a *Agent) slashStatus(msg bus.InboundMessage) slashResult {
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	sessionMsgs := sess.GetMessages()

	memContent := a.memory.LoadMemory()
	memLines := 0
	if memContent != "" {
		memLines = strings.Count(memContent, "\n") + 1
	}

	soul := a.loadSoulName()

	status := slashTf(msg.Lang, "status",
		a.name, a.model, soul,
		a.maxTokens, a.temperature, a.maxToolIterations,
		len(sessionMsgs), memLines, a.homePath,
	)
	return slashResult{handled: true, reply: status}
}

func (a *Agent) slashUsage(msg bus.InboundMessage) slashResult {
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	msgs := sess.GetMessages()

	userTurns, asstTurns, toolTurns := 0, 0, 0
	for _, m := range msgs {
		switch m.Role {
		case "user":
			userTurns++
		case "assistant":
			asstTurns++
		case "tool":
			toolTurns++
		}
	}

	reply := a.billingUsageText(context.Background(), msg.Lang)
	if reply != "" {
		reply += "\n\n"
	}
	reply += slashTf(msg.Lang, "usage.session",
		userTurns, asstTurns, toolTurns, len(msgs),
	)

	// Append cost tracking info from SDK engine
	if a.costTracker != nil {
		stats := a.costTracker.Stats()
		reply += "\n" + slashTf(msg.Lang, "usage.cost",
			a.costTracker.FormatCost(),
			stats["totalInputTokens"],
			stats["totalOutputTokens"],
			stats["totalAPIDurationMs"],
			stats["totalToolDurationMs"],
		)
	}

	return slashResult{handled: true, reply: reply}
}

func (a *Agent) billingUsageText(ctx context.Context, lang string) string {
	if a.meter == nil {
		return ""
	}
	userID := a.ownerUserID
	if userID == "" {
		return ""
	}
	if a.quotaStore != nil {
		if _, qerr := a.quotaStore.GetQuota(ctx, userID); qerr == nil {
			if status, err := usage.CheckQuota(ctx, a.quotaStore, a.meter, userID); err == nil && status != nil {
				return slashTf(lang, "usage.billing.quota",
					userID,
					status.TokensUsed, usageLimitText(status.MonthlyTokenLimit, lang),
					status.RequestsUsed, usageLimitText(status.MonthlyRequestLimit, lang),
					remainingText(status.MonthlyTokenLimit, status.TokensUsed, lang),
					remainingText(status.MonthlyRequestLimit, status.RequestsUsed, lang),
					status.Allowed, emptyDash(status.ResetsAt))
			}
		}
	}
	totals, err := a.meter.TotalsForUser(ctx, userID, usage.LastN(30))
	if err != nil {
		return ""
	}
	tokens := totals.Input + totals.Output + totals.CacheRead + totals.CacheCreation
	return slashTf(lang, "usage.billing.unlimited", userID, tokens, totals.Requests)
}

func usageLimitText(limit int64, lang string) string {
	if limit <= 0 {
		return slashT(lang, "usage.unlimited")
	}
	return fmt.Sprintf("%d", limit)
}

func remainingText(limit, used int64, lang string) string {
	if limit <= 0 {
		return slashT(lang, "usage.unlimited")
	}
	left := limit - used
	if left < 0 {
		left = 0
	}
	return fmt.Sprintf("%d", left)
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (a *Agent) slashInsights(msg bus.InboundMessage, days int) slashResult {
	logDir := filepath.Join(a.homePath, "memory", "logs")
	cutoff := time.Now().AddDate(0, 0, -days)

	files, _ := filepath.Glob(filepath.Join(logDir, "*.jsonl"))
	totalFiles, recentFiles := 0, 0
	for _, f := range files {
		totalFiles++
		info, err := os.Stat(f)
		if err == nil && info.ModTime().After(cutoff) {
			recentFiles++
		}
	}

	reply := slashTf(msg.Lang, "insights",
		days, totalFiles, recentFiles,
		func() string {
			info, err := os.Stat(filepath.Join(a.homePath, "MEMORY.md"))
			if err != nil {
				return slashT(msg.Lang, "insights.memfile.notfound")
			}
			return slashTf(msg.Lang, "insights.memfile.fmt", float64(info.Size())/1024, info.ModTime().Format("2006-01-02 15:04"))
		}(),
		a.homePath,
	)
	return slashResult{handled: true, reply: reply}
}

// slashPersonalityList lists available SOUL.md presets.
func (a *Agent) slashPersonalityList(msg bus.InboundMessage) slashResult {
	presets := a.listPersonalities()
	if len(presets) == 0 {
		return slashResult{handled: true, reply: slashT(msg.Lang, "personality.empty")}
	}
	current := a.loadSoulName()
	var sb strings.Builder
	sb.WriteString(slashT(msg.Lang, "personality.list_header"))
	mark := slashT(msg.Lang, "personality.current_mark")
	for _, p := range presets {
		if p == current {
			sb.WriteString(fmt.Sprintf("• %s%s\n", p, mark))
		} else {
			sb.WriteString(fmt.Sprintf("• %s\n", p))
		}
	}
	sb.WriteString(slashT(msg.Lang, "personality.usage_line"))
	return slashResult{handled: true, reply: sb.String()}
}

// slashPersonalitySet switches the active SOUL.md.
func (a *Agent) slashPersonalitySet(msg bus.InboundMessage, name string) slashResult {
	// Look for SOUL-<name>.md in workspace
	srcPath := filepath.Join(a.homePath, fmt.Sprintf("SOUL-%s.md", name))
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return slashResult{handled: true, reply: slashTf(msg.Lang, "personality.not_found", name, srcPath)}
	}

	data, err := os.ReadFile(srcPath)
	if err != nil {
		return slashResult{handled: true, reply: slashTf(msg.Lang, "personality.read_err", err)}
	}

	destPath := filepath.Join(a.homePath, "SOUL.md")
	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		return slashResult{handled: true, reply: slashTf(msg.Lang, "personality.write_err", err)}
	}

	return slashResult{handled: true, reply: slashTf(msg.Lang, "personality.set_done", name)}
}

// slashModel switches the active model for this agent session.
func (a *Agent) slashModel(msg bus.InboundMessage, model string) slashResult {
	old := a.model
	a.model = model
	return slashResult{handled: true, reply: slashTf(msg.Lang, "model.switched", old, model)}
}

// listPersonalities finds SOUL-<name>.md files in workspace.
func (a *Agent) listPersonalities() []string {
	pattern := filepath.Join(a.homePath, "SOUL-*.md")
	files, _ := filepath.Glob(pattern)
	var names []string
	for _, f := range files {
		base := filepath.Base(f)
		// SOUL-<name>.md → <name>
		name := strings.TrimPrefix(base, "SOUL-")
		name = strings.TrimSuffix(name, ".md")
		names = append(names, name)
	}
	return names
}

// loadSoulName returns the current personality name (default if standard SOUL.md).
func (a *Agent) loadSoulName() string {
	// Check if current SOUL.md is a known preset
	for _, p := range a.listPersonalities() {
		srcPath := filepath.Join(a.homePath, fmt.Sprintf("SOUL-%s.md", p))
		soulPath := filepath.Join(a.homePath, "SOUL.md")
		srcData, err1 := os.ReadFile(srcPath)
		soulData, err2 := os.ReadFile(soulPath)
		if err1 == nil && err2 == nil && string(srcData) == string(soulData) {
			return p
		}
	}
	return "default"
}

func (a *Agent) slashHelp(lang string) string {
	return slashT(lang, "help")
}

// slashPlan handles `/plan <task>`: republish the rest of the message
// onto bus.Inbound with planMode=true so the regular HandleMessage path
// routes it into handlePlanMode. Manual replacement for the auto-plan
// heuristic — users opt in explicitly per turn rather than the server
// guessing from message shape.
func (a *Agent) slashPlan(msg bus.InboundMessage, args []string) slashResult {
	task := strings.TrimSpace(strings.Join(args, " "))
	if task == "" {
		return slashResult{handled: true, reply: slashT(msg.Lang, "plan.usage")}
	}

	// Clone the inbound msg so routing fields (channel, account, chat,
	// project, user, sender, owner) carry over verbatim. Rewrite only
	// Text and Params — the plan-mode flag is what handlePlanMode keys
	// on (see isPlanMode in loop.go).
	out := msg
	out.Text = task
	params := map[string]any{}
	for k, v := range msg.Params {
		params[k] = v
	}
	params["planMode"] = true
	out.Params = params

	select {
	case a.messageBus.Inbound <- out:
		return slashResult{handled: true, reply: "", continuationQueued: true}
	default:
		return slashResult{handled: true, reply: slashT(msg.Lang, "plan.bus_full")}
	}
}

func truncateSlash(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
