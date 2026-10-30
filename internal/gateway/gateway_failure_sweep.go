package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/agent"
	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/session"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// Failure-retry sweep: the watchdog that closes the loop the loop-level
// self-heal can't. When a turn dies after all retries, the agent loop
// persists a turn-error notice (OriginTurnError) into the session. This
// sweep periodically scans llm_call_diag for sessions with recent LLM
// failures, analyzes the tail of each session's working set for an
// UNRESOLVED failure chain, and then:
//
//   - replayable chains (every link failed with ZERO tool results — no
//     partial side effects, so a replay is safe) are auto-retried through
//     the normal HandleMessage path, with exponential backoff between
//     attempts and a configurable attempt cap. Success is announced to the
//     user ("your message was recovered, here's the reply"); the chain
//     breaks naturally because a successful turn appends an assistant
//     reply after the notice.
//   - non-replayable failures (died mid-tool) and chains that exhausted
//     the attempt cap are surfaced to the user once via the notification
//     queue + IM leg, with the cause and the number of attempts spent.
//
// The session history is the source of truth: the chain of retry-tagged
// turns + notices carries the attempt count and the last failure time, so
// the retry schedule is derivable at any moment — a restart loses nothing.
// failureScan below is a CACHE, not a second source of truth: it only
// remembers which diag rows were already looked at (and when the next
// attempt is due) so a settled session isn't re-loaded every tick. Dropping
// it costs one extra pass, never a missed or doubled retry.

// failureRetryTag prefixes the replayed user turn so the LLM, the history
// renderer, and analyzeFailureChain can all recognize a system replay.
const failureRetryTag = "🔁 [系统自动重试] "

// failureRetryMax returns the auto-replay attempt cap per failed turn from
// the FLUCTIO_FAILURE_RETRY_MAX env knob (min 1), default 3. The runtime
// source is failureRetryRuntime — this is its env fallback layer.
func failureRetryMax() int {
	if v := os.Getenv("FLUCTIO_FAILURE_RETRY_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	return 3
}

// failureRetryBase returns the base backoff from
// FLUCTIO_FAILURE_RETRY_BASE_SECS (min 60), default 900 (15min). The
// runtime source is failureRetryRuntime — this is its env fallback layer.
func failureRetryBase() time.Duration {
	if v := os.Getenv("FLUCTIO_FAILURE_RETRY_BASE_SECS"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 60 {
			return time.Duration(secs) * time.Second
		}
	}
	return 15 * time.Minute
}

// failureRetryRuntime resolves the sweep's tuning knobs for one pass:
// system-scope "failure" setting row first (dashboard-adjustable, hot —
// read per tick, no restart), then the env knob, then the built-in
// default. Kept per-pass so a settings change lands on the next tick.
func failureRetryRuntime(ctx context.Context, st store.Store) (base time.Duration, max int) {
	base, max = 0, 0
	if st != nil {
		var c config.FailureRetryCfg
		if err := scope.SettingInto(ctx, st, config.FailureNamespace, "", "", &c); err == nil {
			if c.BackoffBaseMin >= 1 {
				base = time.Duration(c.BackoffBaseMin) * time.Minute
			}
			if c.MaxAttempts >= 1 {
				max = c.MaxAttempts
			}
		}
	}
	if base == 0 {
		base = failureRetryBase()
	}
	if max == 0 {
		max = failureRetryMax()
	}
	return base, max
}

// failureChain is the distilled state of a session tail that ends in an
// unanswered failure. Produced by analyzeFailureChain.
type failureChain struct {
	userText   string    // the ORIGINAL user request (first link), verbatim
	retries    int       // auto-retries already spent (tagged turns in the chain)
	replayable bool      // every link died with zero tool results
	lastAt     time.Time // newest notice timestamp — the backoff anchor
	category   string    // newest notice failure category
}

// analyzeFailureChain walks a session working set backwards from the tail
// collecting the unresolved failure chain: alternating [user, turn-error
// notice] pairs where the innermost user turn is the original request and
// any deeper user turns are system replays (failureRetryTag). ok=false
// when the tail isn't a failure notice (normal answered tail, in-flight
// turn, clean session) — the sweep then leaves the session alone. A user
// turn that arrives after the notice breaks the chain by construction,
// which is exactly the "user already handled it" escape hatch.
func analyzeFailureChain(msgs []provider.Message) (failureChain, bool) {
	n := len(msgs)
	if n < 2 {
		return failureChain{}, false
	}
	// Tail must be a turn-error notice.
	tail := msgs[n-1]
	if !provider.IsFailureNotice(tail.Origin) {
		return failureChain{}, false
	}
	var chain failureChain
	chain.lastAt = time.UnixMilli(tail.Timestamp)
	if cat, ok := tail.Metadata["turnError"].(string); ok {
		chain.category = cat
	}
	chain.replayable = true // any link with tool results poisons it below

	// Walk [notice, user] pairs backwards.
	i := n - 1
	var originalText string
	for i >= 1 {
		notice := msgs[i]
		if !provider.IsFailureNotice(notice.Origin) {
			return failureChain{}, false // malformed — treat as no chain
		}
		if toolsExecutedOf(notice) > 0 {
			chain.replayable = false
		}
		user := msgs[i-1]
		if user.Role != "user" || provider.IsFailureNotice(user.Origin) {
			return failureChain{}, false
		}
		text := user.TextContent()
		// The tag's attempt number is cosmetic — the chain length is the
		// truth, and a missing link breaks the walk anyway (so a parsed
		// number could never disagree).
		if strings.HasPrefix(text, failureRetryTag) {
			chain.retries++
		} else {
			originalText = text
			break // reached the original request — chain complete
		}
		i -= 2
	}
	if originalText == "" {
		return failureChain{}, false // ran off the front without the original
	}
	chain.userText = originalText
	return chain, true
}

// toolsExecutedOf reads the notice's toolsExecuted metadata (set by the
// loop's failure branch). Missing → 0 (pre-metadata notices were all
// first-call failures, the safe default for replay gating).
func toolsExecutedOf(m provider.Message) int {
	if v, ok := m.Metadata["toolsExecuted"].(int); ok {
		return v
	}
	// JSON round-trips (sessions table) turn ints into float64.
	if f, ok := m.Metadata["toolsExecuted"].(float64); ok {
		return int(f)
	}
	return 0
}

// failureScanWindow is how far back each pass looks for failures. It must
// exceed the longest backoff (base<<attempts — 105min at the defaults)
// plus a tick of slack so an unresolved chain keeps being seen; 6h also
// lines up with the notification dedupe window.
const failureScanWindow = 6 * time.Hour

// failureScanState is the per-session cache entry (see the file header):
// the newest diag row already analyzed, and — while a retry is pending —
// when to look again.
type failureScanState struct {
	handledUpTo int64     // newest llm_call_diag id already analyzed
	nextAt      time.Time // zero = settled (nothing pending)
	lastSeen    time.Time // for pruning
}

// runFailureSweep boots the sweep and re-runs it every 10 minutes.
func (g *Gateway) runFailureSweep(ctx context.Context) {
	if dbs, ok := g.store.(*store.DBStore); ok && dbs != nil {
		g.loadFailureNotified(ctx, dbs)
	}
	g.runEvery(ctx, 10*time.Minute, g.sweepFailures)
}

// sweepFailures runs one pass: read the window's failure rows, collapse
// them per session, and act on each session whose chain isn't settled yet.
func (g *Gateway) sweepFailures(ctx context.Context) {
	dbs, ok := g.store.(*store.DBStore)
	if !ok || dbs == nil {
		return
	}
	rows, err := dbs.ListRecoverableCallFailures(ctx, time.Now().Add(-failureScanWindow), 1000)
	if err != nil {
		slog.Warn("failure sweep: list failed calls", "error", err)
		return
	}

	// Newest row per session (rows come oldest-first, so overwrite).
	type sessRef struct{ agentID, key string }
	newest := map[sessRef]store.LLMCallDiagRow{}
	for _, r := range rows {
		newest[sessRef{r.AgentID, r.SessionKey}] = r
	}

	base, maxAttempts := failureRetryRuntime(ctx, g.store)
	agents := map[string]*store.AgentRecord{}
	now := time.Now()
	for ref, row := range newest {
		if !shouldScan(g.failureState(ref.agentID, ref.key), row.ID, now) {
			continue
		}
		g.handleFailureSession(ctx, dbs, ref.agentID, ref.key, row.ID, base, maxAttempts, agents)
	}
	g.pruneFailureStates()
}

// shouldScan decides, from the cache alone, whether a session needs a look
// this pass. False means: settled with no newer failure, or a retry that
// isn't due yet — either way the session isn't loaded.
func shouldScan(st failureScanState, newestRowID int64, now time.Time) bool {
	if !st.nextAt.IsZero() && now.Before(st.nextAt) {
		return false
	}
	return st.handledUpTo < newestRowID || !st.nextAt.IsZero()
}

// failureState returns the cached scan state for one session (zero value
// when absent, which reads as "never analyzed").
func (g *Gateway) failureState(agentID, sessionKey string) failureScanState {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.failureScan[agentID+"|"+sessionKey]
}

// setFailureState records the outcome of one session analysis.
func (g *Gateway) setFailureState(agentID, sessionKey string, st failureScanState) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failureScan == nil {
		g.failureScan = map[string]failureScanState{}
	}
	st.lastSeen = time.Now()
	g.failureScan[agentID+"|"+sessionKey] = st
}

// pruneFailureStates drops settled entries older than the scan window —
// they'd be skipped by the window anyway, and a fresh failure for that
// session simply re-creates the entry.
func (g *Gateway) pruneFailureStates() {
	cutoff := time.Now().Add(-failureScanWindow)
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, st := range g.failureScan {
		if !st.nextAt.IsZero() || st.lastSeen.After(cutoff) {
			continue
		}
		delete(g.failureScan, k)
	}
}

// handleFailureSession loads one session's tail and acts on its unresolved
// failure chain: replay (backoff-gated, cap-gated) or notify. base and
// maxAttempts come from failureRetryRuntime (per pass).
func (g *Gateway) handleFailureSession(ctx context.Context, dbs *store.DBStore, agentID, sessionKey string, upTo int64, base time.Duration, maxAttempts int, agents map[string]*store.AgentRecord) {
	rec, cached := agents[agentID]
	if !cached {
		rec = g.agentRecord(ctx, dbs, agentID)
		agents[agentID] = rec
	}
	if rec == nil {
		return
	}
	space, err := g.users.getOrLoad(ctx, rec.UserID)
	if err != nil || space == nil || space.Agents == nil {
		return
	}
	ag := space.Agents.AgentByID(agentID)
	if ag == nil {
		return
	}
	sess := ag.Sessions().GetByKey(sessionKey)
	if sess == nil {
		return
	}
	settled := failureScanState{handledUpTo: upTo}
	chain, ok := analyzeFailureChain(sess.GetMessages())
	if !ok {
		g.setFailureState(agentID, sessionKey, settled)
		return // chain resolved (or never existed) — nothing to do
	}

	if !chain.replayable {
		// Died mid-tool: a replay could double-execute side effects.
		// Surface it once instead — the user must judge what to redo.
		g.setFailureState(agentID, sessionKey, settled)
		g.notifyFailureOnce(ctx, dbs, rec, sessionKey, "not_replayable",
			"任务中途失败，无法自动重试",
			fmt.Sprintf("一条任务在执行过程中失败（类别 %s，已有部分工具执行，自动重试可能重复副作用）。请打开会话确认进度后手动重发。", chain.category))
		return
	}
	if chain.retries >= maxAttempts {
		// Cap reached: stop retrying, tell the user what was spent.
		g.setFailureState(agentID, sessionKey, settled)
		g.notifyFailureOnce(ctx, dbs, rec, sessionKey, "gave_up",
			"自动重试已放弃",
			fmt.Sprintf("你的消息「%s」已自动重试 %d 次仍失败（类别 %s）。不再继续尝试——请检查模型配置或额度后手动重发。", truncateRunes(chain.userText, 40), chain.retries, chain.category))
		return
	}
	// Backoff before the next attempt: base << retries since the newest
	// notice (base for the first retry, doubling per further attempt).
	if due := chain.lastAt.Add(base << chain.retries); time.Now().Before(due) {
		settled.nextAt = due
		g.setFailureState(agentID, sessionKey, settled)
		return
	}
	// A turn is already running on this session — replaying now would
	// interleave two turns in one conversation. Look again next pass (the
	// chain analysis is idempotent, so nothing is lost).
	if sess.TurnInFlight() {
		settled.nextAt = time.Now().Add(time.Minute)
		g.setFailureState(agentID, sessionKey, settled)
		return
	}
	g.replayFailedTurn(ctx, dbs, rec, ag, sess, chain)
	// Re-analyze next pass: a failed replay appends a fresh notice (new
	// retry count, new backoff anchor), a successful one ends on an
	// assistant reply and is announced then.
	g.setFailureState(agentID, sessionKey, failureScanState{handledUpTo: upTo})
}

// replayFailedTurn re-enters the original request through HandleMessage on
// the SAME session. The replayed turn is prefixed with the retry tag so the
// LLM knows the context and the chain analysis counts the attempt; a
// successful replay appends an assistant reply after the chain, which the
// next pass reads as "resolved".
func (g *Gateway) replayFailedTurn(ctx context.Context, dbs *store.DBStore, rec *store.AgentRecord, ag *agent.Agent, sess *session.Session, chain failureChain) {
	channel, accountID, chatID, projectID := sess.RouteInfo()
	attempt := chain.retries + 1
	text := fmt.Sprintf("%s第%d次·%s", failureRetryTag, attempt, chain.userText)
	slog.Info("failure sweep: replaying failed turn",
		"agent", ag.Name(), "session", sess.Key(), "attempt", attempt, "category", chain.category)

	reply := ag.HandleMessage(ctx, bus.InboundMessage{
		Channel:     channel,
		AccountID:   accountID,
		ChatID:      chatID,
		ProjectID:   projectID,
		AgentID:     ag.ID(),
		Text:        text,
		Source:      bus.SourceFailureRetry,
		OwnerUserID: rec.UserID,
	})

	// Success = the persisted chain no longer reads as unresolved. Judging
	// by the history (not the reply string) keeps this independent of the
	// notice wording.
	if _, failed := analyzeFailureChain(sess.GetMessages()); failed {
		return
	}
	g.notifyFailureOnce(ctx, dbs, rec, sess.Key(), "recovered",
		"失败消息已自动恢复",
		fmt.Sprintf("你之前那条「%s」在服务恢复后已自动重试成功。回复（摘要）：%s",
			truncateRunes(chain.userText, 40), truncateRunes(reply, 160)))
}

// notifyFailureOnce delivers a sweep notification through the shared
// three-leg policy (IM best-effort + notification queue + session archive).
// Dedupe keys are persisted (sweep/failure_notified) so a daemon restart
// doesn't re-announce a chain that was already reported.
func (g *Gateway) notifyFailureOnce(ctx context.Context, dbs *store.DBStore, rec *store.AgentRecord, sessionKey, event, title, body string) {
	if !g.claimFailureNotice(ctx, dbs, rec.ID+"|"+sessionKey+"|"+event) {
		return
	}
	uid := sweepOwnerUID(ctx, dbs, *rec)
	// "" lets resolveSweepTarget fall back to the configured default
	// channel instead of pinning wechat (the other sweeps pass "" too).
	ch, chatID, targetKey, _ := g.resolveSweepTarget(ctx, dbs, rec.ID, "", "failure sweep")
	g.deliverSweepNudge(ctx, dbs, *rec, uid, ch, chatID, targetKey,
		"llm_failure", title, body, "/agents/"+rec.ID+"/chat", "llm_failure", "failure sweep")
}

// claimFailureNotice marks dedupeKey as announced, returning false when it
// was announced within failureScanWindow. The in-memory map is the fast
// path; the sweep/failure_notified settings row is the durable copy that
// survives a restart.
func (g *Gateway) claimFailureNotice(ctx context.Context, dbs *store.DBStore, dedupeKey string) bool {
	now := time.Now()
	cutoff := now.Add(-failureScanWindow)
	g.mu.Lock()
	if g.failureNotified == nil {
		g.failureNotified = map[string]time.Time{}
	}
	if last, ok := g.failureNotified[dedupeKey]; ok && last.After(cutoff) {
		g.mu.Unlock()
		return false
	}
	g.failureNotified[dedupeKey] = now
	snapshot := make(map[string]any, len(g.failureNotified))
	for k, at := range g.failureNotified {
		if at.After(cutoff) {
			snapshot[k] = at.UTC().Format(time.RFC3339)
		}
	}
	g.mu.Unlock()

	// Write-through, best-effort: a lost write only costs one duplicate
	// notification after the next restart.
	_ = dbs.SaveConfig(ctx, &store.ConfigRecord{
		Kind:    "sweep",
		Name:    "failure_notified",
		Enabled: true,
		Data:    snapshot,
	})
	return true
}

// loadFailureNotified hydrates the in-memory dedupe map from the settings
// row at boot, so a restart doesn't re-announce already-reported chains.
func (g *Gateway) loadFailureNotified(ctx context.Context, dbs *store.DBStore) {
	rec, err := dbs.GetConfigByName(ctx, "sweep", "", "failure_notified")
	if err != nil || rec == nil {
		return
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failureNotified == nil {
		g.failureNotified = map[string]time.Time{}
	}
	for k, v := range rec.Data {
		s, _ := v.(string)
		t, err := time.Parse(time.RFC3339, s)
		if err != nil || !t.After(now.Add(-failureScanWindow)) {
			continue
		}
		g.failureNotified[k] = t
	}
}

// agentRecord resolves the store row for an agent ID (owner + config),
// nil when missing.
func (g *Gateway) agentRecord(ctx context.Context, dbs *store.DBStore, agentID string) *store.AgentRecord {
	rec, err := dbs.GetAgent(ctx, agentID)
	if err != nil || rec == nil {
		slog.Warn("failure sweep: agent record", "agent", agentID, "error", err)
		return nil
	}
	return rec
}

// truncateRunes caps a string for notification bodies.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
