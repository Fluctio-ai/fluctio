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
// sweep periodically scans llm_call_diag (time cursor = "already scanned"
// mark, so each failure is looked at exactly once) for sessions with
// recent LLM failures, analyzes the tail of each session's working set for
// an UNRESOLVED failure chain, and then:
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
// All retry state lives in the session history itself (the chain of 🔁
// turns + notices) — nothing extra to persist, restarts and multi-replica
// deployments can't lose or double-count it.

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
	retries    int       // auto-retries already spent (🔁 turns in the chain)
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
	if tail.Origin != provider.OriginTurnError {
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
		if notice.Origin != provider.OriginTurnError {
			return failureChain{}, false // malformed — treat as no chain
		}
		if toolsExecutedOf(notice) > 0 {
			chain.replayable = false
		}
		user := msgs[i-1]
		if user.Role != "user" || user.Origin == provider.OriginTurnError {
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

// runFailureSweep boots the sweep (clearing the backlog the cursor marks
// un-scanned) and re-runs it every 10 minutes.
func (g *Gateway) runFailureSweep(ctx context.Context) {
	g.runEvery(ctx, 10*time.Minute, g.sweepFailures)
}

// sweepFailures runs one incremental pass: scan failures past the cursor,
// group by session, act on each unresolved chain, advance the cursor.
func (g *Gateway) sweepFailures(ctx context.Context) {
	dbs, ok := g.store.(*store.DBStore)
	if !ok || dbs == nil {
		return
	}
	since := loadFailureCursor(ctx, dbs)
	rows, err := dbs.ListFailedLLMCalls(ctx, since, "", 500)
	if err != nil {
		slog.Warn("failure sweep: list failed calls", "error", err)
		return
	}
	// Advance the cursor over everything just scanned. Newest-first rows:
	// the first row's CreatedAt is the high-water mark. Overlap-safe: the
	// chain analysis is idempotent, so re-seeing a boundary row after a
	// restart costs one wasted session read, not a double replay. An empty
	// page writes nothing — it carries no information worth a DB write.
	if len(rows) > 0 {
		saveFailureCursor(ctx, dbs, rows[0].CreatedAt)
	}
	g.pruneFailureNotified()

	// Distinct (agent, session) pairs — one chain analysis per session.
	// The agent records and tuning knobs resolve once per pass.
	base, maxAttempts := failureRetryRuntime(ctx, g.store)
	agents := map[string]*store.AgentRecord{}
	type sessRef struct{ agentID, key string }
	seen := map[sessRef]bool{}
	for _, r := range rows {
		if r.AgentID == "" || r.SessionKey == "" {
			continue
		}
		ref := sessRef{r.AgentID, r.SessionKey}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		g.handleFailureSession(ctx, dbs, r.AgentID, r.SessionKey, base, maxAttempts, agents)
	}
}

// pruneFailureNotified drops notification-dedupe entries past their 6h
// window so the map doesn't grow with the deployment's session count.
func (g *Gateway) pruneFailureNotified() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, at := range g.failureNotified {
		if time.Since(at) >= 6*time.Hour {
			delete(g.failureNotified, k)
		}
	}
}

// handleFailureSession loads one session's working set tail and acts on
// its unresolved failure chain: replay (backoff-gated, cap-gated) or
// notify. base/maxAttempts come from failureRetryRuntime (per-pass).
func (g *Gateway) handleFailureSession(ctx context.Context, dbs *store.DBStore, agentID, sessionKey string, base time.Duration, maxAttempts int, agents map[string]*store.AgentRecord) {
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
	chain, ok := analyzeFailureChain(sess.GetMessages())
	if !ok {
		return // no unresolved failure at the tail
	}

	if !chain.replayable {
		// Died mid-tool: a replay could double-execute side effects.
		// Surface it once instead — the user must judge what to redo.
		g.notifyFailureOnce(ctx, dbs, rec, sessionKey, "not_replayable",
			"任务中途失败，无法自动重试",
			fmt.Sprintf("一条任务在执行过程中失败（类别 %s，已有部分工具执行，自动重试可能重复副作用）。请打开会话确认进度后手动重发。", chain.category))
		return
	}
	if chain.retries >= maxAttempts {
		// Cap reached: stop retrying, tell the user what was spent.
		g.notifyFailureOnce(ctx, dbs, rec, sessionKey, "gave_up",
			"自动重试已放弃",
			fmt.Sprintf("你的消息「%s」已自动重试 %d 次仍失败（类别 %s）。不再继续尝试——请检查模型配置或额度后手动重发。", truncateRunes(chain.userText, 40), chain.retries, chain.category))
		return
	}
	// Backoff before the next attempt: base << retries since the newest
	// notice (base for the first retry, doubling per further attempt).
	if !time.Now().After(chain.lastAt.Add(base << chain.retries)) {
		return // not due yet — next tick re-evaluates
	}
	g.replayFailedTurn(ctx, dbs, rec, ag, sess, chain)
}

// replayFailedTurn re-enters the original request through HandleMessage
// on the SAME session. The replayed turn is prefixed with the retry tag
// (attempt number included) so the chain analysis counts attempts and the
// LLM knows the context. A successful replay appends a normal assistant
// reply after the chain — the tail is no longer a notice, so the chain is
// resolved for free; a failed replay appends a fresh notice and the chain
// grows by one counted link.
func (g *Gateway) replayFailedTurn(ctx context.Context, dbs *store.DBStore, rec *store.AgentRecord, ag *agent.Agent, sess *session.Session, chain failureChain) {
	channel, accountID, chatID, projectID := sess.RouteInfo()
	attempt := chain.retries + 1
	text := fmt.Sprintf("%s第%d次·%s", failureRetryTag, attempt, chain.userText)
	slog.Info("failure sweep: replaying failed turn",
		"agent", ag.Name(), "session", sess.Key(), "attempt", attempt, "category", chain.category)

	reply := ag.HandleMessage(ctx, bus.InboundMessage{
		Channel:    channel,
		AccountID:  accountID,
		ChatID:     chatID,
		ProjectID:  projectID,
		AgentID:    ag.ID(),
		Text:       text,
		Source:     bus.SourceFailureRetry,
		OwnerUserID: rec.UserID,
	})

	// Success = the session tail is no longer a failure notice. Judging by
	// the persisted chain (not the reply string) keeps this independent of
	// the notice wording and survives i18n edits; a failed replay appended
	// a fresh notice, so the chain grew by one counted link.
	if _, failed := analyzeFailureChain(sess.GetMessages()); failed {
		return
	}
	g.notifyFailureOnce(ctx, dbs, rec, sess.Key(), "recovered",
		"失败消息已自动恢复",
		fmt.Sprintf("你之前那条「%s」在服务恢复后已自动重试成功。回复（摘要）：%s",
			truncateRunes(chain.userText, 40), truncateRunes(reply, 160)))
}

// notifyFailureOnce delivers a sweep notification through the shared
// three-leg policy (IM best-effort + notification queue + session archive)
// with a 6h per-(session,event) dedupe so a stuck chain doesn't spam every
// 10-minute tick.
func (g *Gateway) notifyFailureOnce(ctx context.Context, dbs *store.DBStore, rec *store.AgentRecord, sessionKey, event, title, body string) {
	dedupeKey := rec.ID + "|" + sessionKey + "|" + event
	g.mu.Lock()
	if g.failureNotified == nil {
		g.failureNotified = map[string]time.Time{}
	}
	if last, ok := g.failureNotified[dedupeKey]; ok && time.Since(last) < 6*time.Hour {
		g.mu.Unlock()
		return
	}
	g.failureNotified[dedupeKey] = time.Now()
	g.mu.Unlock()

	uid := sweepOwnerUID(ctx, dbs, *rec)
	// "" lets resolveSweepTarget fall back to the configured default
	// channel instead of pinning wechat (the other sweeps pass "" too).
	ch, chatID, targetKey, _ := g.resolveSweepTarget(ctx, dbs, rec.ID, "", "failure sweep")
	g.deliverSweepNudge(ctx, dbs, *rec, uid, ch, chatID, targetKey,
		"llm_failure", title, body, "/agents/"+rec.ID+"/chat", "llm_failure", "failure sweep")
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

// failureSweepLookback bounds the FIRST pass (no cursor yet): only recent
// failures are considered, so a fresh deployment doesn't replay/alert on
// every failure still inside the diag retention window.
const failureSweepLookback = 15 * time.Minute

// loadFailureCursor reads the persisted scan cursor ("already looked at
// everything before this") from the configs table. No cursor yet → the
// sweep starts with a short lookback rather than the full retention
// window.
func loadFailureCursor(ctx context.Context, dbs *store.DBStore) time.Time {
	if rec, err := dbs.GetConfigByName(ctx, "sweep", "", "failure_sweep_cursor"); err == nil && rec != nil {
		if s, _ := rec.Data["ts"].(string); s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t
			}
		}
	}
	return time.Now().Add(-failureSweepLookback)
}

// saveFailureCursor persists the scan cursor.
func saveFailureCursor(ctx context.Context, dbs *store.DBStore, t time.Time) {
	_ = dbs.SaveConfig(ctx, &store.ConfigRecord{
		Kind:    "sweep",
		Name:    "failure_sweep_cursor",
		Enabled: true,
		Data:    map[string]any{"ts": t.UTC().Format(time.RFC3339)},
	})
}

// truncateRunes caps a string for notification bodies.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
