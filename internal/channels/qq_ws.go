package channels

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/fluctio-ai/fluctio/internal/bus"
)

// QQ WS adapter (Phase 1 skeleton).
//
// Implements channels.Channel for the Tencent QQ Official Bot Platform
// (api.sgroup.qq.com). Follows the openclaw-qqbot state-machine shape
// (specs/qq-openclaw-contract.md §1 + §5.2) — gorilla/websocket v1.5.3
// replaces the npm `ws` package; no SDK black-box. This file owns:
//
//   - getAccessToken / getGatewayUrl REST calls (token cache deferred to
//     Phase 2 — re-fetched on every reconnect).
//   - wss connection lifecycle: Hello → Identify/Resume → Heartbeat →
//     Dispatch routing → close-code driven reconnect / fatal exit.
//   - Inbound dispatch for GROUP_AT_MESSAGE_CREATE + C2C_MESSAGE_CREATE
//     with the 「群绑群」 UserID mapping (contract §5.7, overridden per
//     Phase 1 brief: group messages set UserID=group_openid so one
//     /claim authorizes the entire group; member_openid lands in
//     SenderName so the chatter is still identifiable in UI).
//
// Phase 1 deliberately does NOT implement:
//   - Send / SendMessage / SendTyping (stubs returning nil — Phase 3).
//   - Token cache + background refresh (Phase 2).
//   - FailureReporter wiring (Phase 2 — OnFailed stores the callback,
//     fireFailed calls it, but no internal trigger fires in Phase 1).
//   - claim-side allowFrom gate (Phase 2).

// ---------------------------------------------------------------------------
// QQ protocol constants (contract §1 + §3).
// ---------------------------------------------------------------------------

const (
	// API hosts.
	qqAPIBase    = "https://api.sgroup.qq.com"
	qqTokenURL   = "https://bots.qq.com/app/getAppAccessToken"
	qqGatewayURL = qqAPIBase + "/gateway"

	// Outbound Authorization header prefix — NOT Bearer (contract §3.1).
	qqAuthScheme = "QQBot"

	// User-Agent sent on both REST + WS handshake (contract §1.2).
	qqUserAgent = "FluctioQQ/1.0"

	// Intents bit mask (contract §1.4). Covers public guild messages,
	// DMs, group+C2C events, and interactions. The Go expression evaluates
	// to 1174409216 — the old 1082146956 in openclaw-qqbot source was an
	// arithmetic typo (contract §1.4 note).
	qqIntentsFull = (1 << 30) | (1 << 12) | (1 << 25) | (1 << 26) // = 1174409216

	// Reconnect policy (contract §1.8).
	qqMaxReconnectAttempts    = 100
	qqFastDisconnectWindow    = 5 * time.Second
	qqFastDisconnectPenalty   = 60 * time.Second
	qqFastDisconnectThreshold = 3
	qqRateLimitWait           = 60 * time.Second
)

// QQ OP codes (contract §1.3, source gateway.ts:1896-2163).
const (
	qqOpHello          = 10 // S→C: carries heartbeat_interval
	qqOpIdentify       = 2  // C→S: new session login
	qqOpResume         = 6  // C→S: resume existing session
	qqOpDispatch       = 0  // S→C: business event, t/d/s
	qqOpHeartbeat      = 1  // C→S: periodic heartbeat
	qqOpHeartbeatACK   = 11 // S→C: heartbeat ack
	qqOpReconnect      = 7  // S→C: server-requested reconnect
	qqOpInvalidSession = 9  // S→C: d=bool (true=may resume, false=clear)
)

// WS close codes (contract §1.8, source gateway.ts:2170-2262).
const (
	qqCloseNormal         = 1000
	qqCloseAuthFailed     = 4004 // token invalid → refresh
	qqCloseSessionInvalid = 4006
	qqCloseSeqInvalid     = 4007
	qqCloseRateLimited    = 4008 // wait 60s
	qqCloseSessionTimeout = 4009
	qqCloseInternalStart  = 4900
	qqCloseInternalEnd    = 4913
	qqCloseRobotOffline   = 4914 // fatal: robot delisted
	qqCloseRobotBanned    = 4915 // fatal: robot banned
)

// Backoff ladder (contract §1.8): [1s, 2s, 5s, 10s, 30s, 60s], capped at 60s.
var qqBackoffSteps = [...]time.Duration{
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
	60 * time.Second,
}

// ---------------------------------------------------------------------------
// QQChannel
// ---------------------------------------------------------------------------

// QQChannel is the QQ Official Bot WebSocket adapter for one account.
// Implements channels.Channel + FailureReporter (Phase 2 wires OnFailed
// into markChannelFailed; Phase 1 stores the callback but never fires it
// internally).
type QQChannel struct {
	accountID  string
	appID      string
	appSecret  string
	mb         *bus.MessageBus
	httpClient *http.Client

	// WS state machine (protected by seqMu where shared across goroutines).
	ws        *websocket.Conn
	sessionID string
	lastSeq   *int
	seqMu     sync.Mutex

	// accessToken is the AppAccessToken bound to the current wss
	// connection — populated at the top of each dialAndRun cycle by
	// qqGetToken (Phase 2: cached + background-refreshed). Read by the
	// Identify/Resume payload builder (handleHello → currentToken) in
	// the same Start goroutine that wrote it — no mutex needed.
	accessToken string

	// tokenFetchFails counts consecutive qqGetToken errors in Start.
	// When it reaches qqTokenFetchFailureThreshold the adapter gives up
	// and fires OnFailed("token_refresh_failed") so the framework can
	// mark the account failed (contract §5.6).
	tokenFetchFails int

	// allowedChecker gates inbound messages by openid (contract §5.5
	// claim复用). Set by gateway registration via SetAllowedChecker;
	// the closure reads the owning agent's Admins["qq"] list. nil =
	// allow all (Phase 1 behavior — preserved so early dev /claim-less
	// smoke tests still pass).
	allowedChecker func(openid string) bool

	// useMarkdown toggles the outbound msg_type (contract §6.3):
	// false (default) → msg_type=0 with downgraded markdown;
	// true → msg_type=2 raw markdown. QQ markdown requires separate
	// platform approval, so false is the safe default. Set via
	// SetUseMarkdown at registration (Phase 4 wires from AccountConfig).
	useMarkdown bool

	// peerKinds records the PeerKind ("group" or "dm") observed on the
	// most recent inbound event per ChatID, so outbound SendMessage can
	// pick the right REST endpoint (/v2/groups vs /v2/users). ChatIDs
	// we've never seen inbound from default to "users" (DM) — the agent
	// rarely sends first, and a wrong-group POST fails cleanly.
	peerKinds sync.Map

	// Heartbeat goroutine lifecycle. Started on Hello; stopped when the
	// owning dialAndRun exits (connCtx cancel). heartbeatRunning guards
	// against a duplicate start if the server sends multiple Hello frames.
	heartbeatRunning bool
	heartbeatMu      sync.Mutex

	// Reconnect bookkeeping (owned by Start goroutine — single-writer).
	attempts          int // cumulative reconnect attempts
	fastDisconnects   int // consecutive <5s disconnects
	connEstablishedAt time.Time

	// FailureReporter (gateway wires this via OnFailed →
	// markChannelFailed so a dead account surfaces a reconnect prompt
	// in the UI and the next restart skips re-starting it).
	onFailed func(accountID, reason string)
}

// NewQQChannel builds a QQ adapter. token cache + background refresh land
// in Phase 2; for now every reconnect re-fetches via getAccessToken.
func NewQQChannel(appID, appSecret, accountID string, mb *bus.MessageBus) (*QQChannel, error) {
	if appID == "" || appSecret == "" {
		return nil, fmt.Errorf("qq: appID and appSecret required")
	}
	if accountID == "" {
		return nil, fmt.Errorf("qq: accountID required")
	}
	if mb == nil {
		return nil, fmt.Errorf("qq: message bus required")
	}
	return &QQChannel{
		accountID:  accountID,
		appID:      appID,
		appSecret:  appSecret,
		mb:         mb,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// OnFailed registers the framework callback (FailureReporter). The
// gateway wires this in registerQQChannels so a dead account surfaces
// a reconnect prompt in the UI instead of looping forever.
func (q *QQChannel) OnFailed(fn func(accountID, reason string)) {
	q.onFailed = fn
}

// SetAllowedChecker installs the claim-gate closure. The closure reads
// the owning agent's Admins["qq"] list (via AgentFileConfigLoader)
// and returns true iff the openid is authorized. Nil = allow all
// (Phase 1 behavior). Called once per registration; inbound messages
// consult the live closure on each dispatch so newly-claimed admins
// take effect without restarting the adapter.
//
// Contract §5.5 scheme A: claim复用 existing /claim + Admins["qq"],
// no openclaw-style static allowFrom.
func (q *QQChannel) SetAllowedChecker(fn func(openid string) bool) {
	q.allowedChecker = fn
}

func (q *QQChannel) Name() string        { return "qq" }
func (q *QQChannel) AccountID() string   { return q.accountID }
func (q *QQChannel) BotUsername() string { return "qqbot" } // sentinel: routeGroup's agentByMention checks BotUsername to recognize @bot on GROUP_AT_MESSAGE_CREATE (QQ events imply @bot; QQ has no real username)

// Send / SendMessage / SendTyping are implemented in qq_send.go
// (Phase 3 — contract §3.5 + §4.2 + §6.9).

// fireFailed invokes the registered FailureReporter callback. No-op in
// Phase 1 (no internal trigger), but defined here so Phase 2 can drop
// calls into the state machine without touching structure.
func (q *QQChannel) fireFailed(reason string) {
	if q.onFailed != nil {
		q.onFailed(q.accountID, reason)
	}
}

// ---------------------------------------------------------------------------
// Start: main reconnect loop (contract §5.2)
// ---------------------------------------------------------------------------

// Start runs the QQ WS connection until ctx is cancelled, the server
// closes with a fatal code (4914/4915), or the reconnect budget is
// exhausted. Mirrors openclaw gateway.ts connect().
func (q *QQChannel) Start(ctx context.Context) error {
	slog.Info("qq ws loop starting", "account", q.accountID, "appId", q.appID)

	for q.attempts = 0; q.attempts < qqMaxReconnectAttempts; q.attempts++ {
		if err := ctx.Err(); err != nil {
			return nil
		}

		// Backoff before any reconnect attempt (skipped on first try).
		if q.attempts > 0 {
			wait := q.nextBackoff()
			// Fast-disconnect penalty: 3 consecutive <5s disconnects → 60s.
			// Usually indicates auth failure / wrong AppID — give the
			// server a breather before we hammer it again.
			if q.fastDisconnects >= qqFastDisconnectThreshold && wait < qqFastDisconnectPenalty {
				wait = qqFastDisconnectPenalty
			}
			slog.Info("qq ws reconnect backoff",
				"account", q.accountID, "attempt", q.attempts, "wait", wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil
			}
		}

		// Phase 2: cached token via qqGetToken. Background refresh is
		// already running (started by qqRefreshToken inside qqGetToken),
		// so long-lived WS connections stay valid for hours without a
		// mid-turn 401 (contract §6.7).
		token, err := q.getAccessToken(ctx)
		if err != nil {
			q.tokenFetchFails++
			slog.Warn("qq getAccessToken failed",
				"account", q.accountID,
				"attempt", q.attempts,
				"consecutiveFails", q.tokenFetchFails,
				"error", err)
			// Contract §5.6: persistent token refresh failure is fatal.
			// Don't let the loop hammer bots.qq.com forever on a bad
			// secret — fire OnFailed and exit so the user sees a
			// reconnect prompt.
			if q.tokenFetchFails >= qqTokenFetchFailureThreshold {
				slog.Error("qq token fetch failure threshold reached — stopping adapter",
					"account", q.accountID, "fails", q.tokenFetchFails)
				q.fireFailed("token_refresh_failed")
				return nil
			}
			continue
		}
		q.tokenFetchFails = 0

		url, err := q.getGatewayUrl(ctx, token)
		if err != nil {
			slog.Warn("qq getGatewayUrl failed",
				"account", q.accountID, "attempt", q.attempts, "error", err)
			continue
		}

		q.connEstablishedAt = time.Now()
		connErr := q.dialAndRun(ctx, url, token)
		connDuration := time.Since(q.connEstablishedAt)

		if connErr == nil {
			continue // ctx cancelled inside dialAndRun → loop will re-check ctx
		}
		if errors.Is(connErr, errQQFatalExit) {
			slog.Error("qq ws fatal close — stopping adapter",
				"account", q.accountID, "error", connErr)
			q.fireFailed("offline_banned")
			return nil
		}

		// Classify close code for session/token hygiene.
		var closeErr qqCloseError
		if errors.As(connErr, &closeErr) {
			switch classifyQQClose(closeErr.Code) {
			case qqCloseActionNormal:
				// 1000: graceful close, don't reconnect.
				return nil
			case qqCloseActionFatal:
				// 4914/4915: should have gone through errQQFatalExit above,
				// but treat defensively.
				q.fireFailed("offline_banned")
				return nil
			case qqCloseActionClearSession:
				q.seqMu.Lock()
				q.sessionID = ""
				q.lastSeq = nil
				q.seqMu.Unlock()
			case qqCloseActionRateLimit:
				select {
				case <-time.After(qqRateLimitWait):
				case <-ctx.Done():
					return nil
				}
				// Don't clear session — server didn't invalidate it.
				continue
			case qqCloseActionRefreshToken:
				// 4004: server rejected the token. Clear the cache so
				// qqGetToken on the next iteration actually re-fetches
				// instead of returning the same dead token (which would
				// loop until backoff exhausts — contract §1.8).
				qqClearToken(q.appID)
			case qqCloseActionRetry:
				// Unknown / transient — keep session + token, reconnect.
			}

			// Fast-disconnect tracking (applies to any non-fatal,
			// non-normal close).
			if connDuration < qqFastDisconnectWindow {
				q.fastDisconnects++
			} else {
				q.fastDisconnects = 0
			}
		}

		// Server-initiated reconnect (op:7 / op:9): the conn wasn't
		// "closed" by the server in the close-handshake sense, but the
		// state machine asked us to tear down + reconnect. Session is
		// already cleared if needed (op:9 d=false). Don't count as a
		// fast disconnect — server moved us, not a crash.
	}

	// Exhausted reconnect budget.
	slog.Error("qq ws reconnect attempts exhausted",
		"account", q.accountID, "attempts", q.attempts)
	q.fireFailed("reconnect_exhausted")
	return nil
}

// nextBackoff returns the wait for the upcoming reconnect attempt.
// Linear walk up the ladder, capped at 60s.
func (q *QQChannel) nextBackoff() time.Duration {
	idx := q.attempts - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(qqBackoffSteps) {
		return qqBackoffSteps[len(qqBackoffSteps)-1]
	}
	return qqBackoffSteps[idx]
}

// ---------------------------------------------------------------------------
// Connection lifecycle
// ---------------------------------------------------------------------------

// dialAndRun opens one wss connection and runs its ReadMessage loop until
// the server closes, the state machine requests a reconnect (op:7/9),
// or ctx is cancelled. Returns a typed error so Start can classify.
func (q *QQChannel) dialAndRun(ctx context.Context, url, token string) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	hdr := http.Header{}
	hdr.Set("User-Agent", qqUserAgent)
	// gorilla/websocket doesn't let us set the Authorization header on
	// the WS handshake itself for QQ (the protocol relies on the Identify
	// payload carrying the token, not a subprotocol header). The UA is
	// the only handshake header QQ cares about (contract §1.2).

	conn, _, err := dialer.DialContext(ctx, url, hdr)
	if err != nil {
		return fmt.Errorf("qq ws dial: %w", err)
	}
	q.ws = conn
	// Stash the token so handleHello can fill the Identify/Resume payload
	// per contract §1.4 / §1.6 (token field is "QQBot <access_token>",
	// the live token fetched for this connection).
	q.accessToken = token

	// connCtx bounds the heartbeat goroutine for THIS connection only.
	// When dialAndRun returns, heartbeat stops even if the outer ctx is
	// still alive.
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.Close()
	defer q.stopHeartbeat()

	slog.Info("qq ws connected",
		"account", q.accountID, "url", url, "has_session", q.sessionID != "")

	for {
		if connCtx.Err() != nil {
			return nil
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return wrapQQCloseError(err)
		}
		if err := q.handleServerMessage(connCtx, raw, conn.WriteJSON); err != nil {
			if errors.Is(err, errQQServerReconnect) {
				// op:7 / op:9 — server asked us to reconnect. Clean
				// teardown, Start will re-dial.
				return err
			}
			slog.Warn("qq ws frame handler error",
				"account", q.accountID, "error", err)
			// Don't bail on a single bad frame — keep the conn alive.
		}
	}
}

// sendFunc is the minimal write path tests can stub. Real code passes
// q.ws.WriteJSON (gorilla/websocket signature).
type sendFunc func(v any) error

// handleServerMessage routes one inbound JSON frame: OP-code dispatch
// per contract §1.3. Side effects:
//   - op:10 Hello → starts heartbeat goroutine + sends Identify or Resume.
//   - op:0 Dispatch → updates lastSeq, routes by t (READY/GROUP_…/C2C_…).
//   - op:11 HeartbeatACK → noop.
//   - op:7 Reconnect → returns errQQServerReconnect.
//   - op:9 InvalidSession → d=false clears session; returns errQQServerReconnect.
//
// `send` is pluggable so tests don't need a real websocket.Conn.
func (q *QQChannel) handleServerMessage(ctx context.Context, raw []byte, send sendFunc) error {
	var frame qqFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		return fmt.Errorf("unmarshal frame: %w", err)
	}

	// DEBUG (Phase 8 e2e): log every inbound frame to distinguish "WS
	// alive but platform pushes no Dispatch" (capability/sandbox issue)
	// from "Dispatch arriving but mishandled". Remove after diagnosis.

	switch frame.Op {
	case qqOpHello:
		return q.handleHello(ctx, frame.D, send)

	case qqOpDispatch:
		// Persist seq BEFORE routing — Resume uses the freshest seq.
		if frame.S != nil {
			s := *frame.S
			q.seqMu.Lock()
			q.lastSeq = &s
			q.seqMu.Unlock()
		}
		return q.routeDispatch(frame.T, frame.D)

	case qqOpHeartbeatACK:
		// Server acknowledged our heartbeat — nothing to do.
		return nil

	case qqOpReconnect:
		// Server-initiated reconnect. Keep session — Resume may work.
		slog.Info("qq ws server requested reconnect (op:7)",
			"account", q.accountID)
		return errQQServerReconnect

	case qqOpInvalidSession:
		// d is a bool: true = may Resume, false = must clear + Identify.
		var resumable bool
		if len(bytes.TrimSpace(frame.D)) > 0 {
			if err := json.Unmarshal(frame.D, &resumable); err != nil {
				// Treat unparseable as "clear" — safer, forces fresh auth.
				slog.Warn("qq op:9 unparseable d — clearing session",
					"account", q.accountID, "raw", string(frame.D))
				resumable = false
			}
		}
		if !resumable {
			q.seqMu.Lock()
			q.sessionID = ""
			q.lastSeq = nil
			q.seqMu.Unlock()
		}
		slog.Info("qq ws invalid session (op:9)",
			"account", q.accountID, "resumable", resumable)
		return errQQServerReconnect

	default:
		slog.Warn("qq ws unknown op code",
			"account", q.accountID, "op", frame.Op)
	}
	return nil
}

// handleHello processes op:10. Per contract §1.5 + §1.6: start heartbeat
// at d.heartbeat_interval, then send Resume (op:6) if we have a valid
// session, else Identify (op:2).
func (q *QQChannel) handleHello(ctx context.Context, d json.RawMessage, send sendFunc) error {
	var hello qqHelloData
	if err := json.Unmarshal(d, &hello); err != nil {
		return fmt.Errorf("unmarshal hello: %w", err)
	}

	q.startHeartbeat(ctx, hello.HeartbeatInterval, send)

	q.seqMu.Lock()
	hasSession := q.sessionID != "" && q.lastSeq != nil
	lastSeq := q.lastSeq
	q.seqMu.Unlock()

	if hasSession {
		if lastSeq == nil {
			// Defensive — hasSession required lastSeq != nil above.
			lastSeq = new(int)
		}
		payload := qqResumePayload{
			Op: qqOpResume,
			D: qqResumeData{
				Token:     qqAuthScheme + " " + q.currentToken(),
				SessionID: q.sessionID,
				Seq:       *lastSeq,
			},
		}
		slog.Info("qq ws sending Resume", "account", q.accountID, "seq", *lastSeq)
		return send(payload)
	}

	payload := qqIdentifyPayload{
		Op: qqOpIdentify,
		D: qqIdentifyData{
			Token:   qqAuthScheme + " " + q.currentToken(),
			Intents: qqIntentsFull,
			Shard:   [2]int{0, 1},
		},
	}
	slog.Info("qq ws sending Identify", "account", q.accountID, "intents", qqIntentsFull)
	return send(payload)
}

// currentToken returns the access token bound to this connection. It's
// populated by dialAndRun right after a successful wss Dial — handleHello
// runs in the same Start goroutine a few ms later (driven by ReadMessage),
// so the plain read is race-free. Phase 2 will swap this for a cache reader.
func (q *QQChannel) currentToken() string {
	return q.accessToken
}

// ---------------------------------------------------------------------------
// Heartbeat (contract §1.5)
// ---------------------------------------------------------------------------

// startHeartbeat spawns the periodic heartbeat goroutine for this
// connection. It exits when connCtx is cancelled. Double-start guarded
// — if a duplicate Hello arrives the existing ticker keeps running.
func (q *QQChannel) startHeartbeat(ctx context.Context, intervalMS int, send sendFunc) {
	q.heartbeatMu.Lock()
	if q.heartbeatRunning {
		q.heartbeatMu.Unlock()
		return
	}
	q.heartbeatRunning = true
	q.heartbeatMu.Unlock()

	interval := time.Duration(intervalMS) * time.Millisecond
	if interval <= 0 {
		interval = 30 * time.Second // QQ typical default; avoids hot-loop if server sends 0
	}

	go func() {
		defer func() {
			q.heartbeatMu.Lock()
			q.heartbeatRunning = false
			q.heartbeatMu.Unlock()
		}()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		slog.Info("qq heartbeat started",
			"account", q.accountID, "interval", interval)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				q.seqMu.Lock()
				lastSeq := q.lastSeq
				q.seqMu.Unlock()
				// d = lastSeq or null. json.Marshal((*int)(nil)) → "null".
				var d any
				if lastSeq != nil {
					d = *lastSeq
				}
				if err := send(qqHeartbeatPayload{Op: qqOpHeartbeat, D: d}); err != nil {
					slog.Warn("qq heartbeat send failed",
						"account", q.accountID, "error", err)
					return
				}
			}
		}
	}()
}

// stopHeartbeat is a no-op marker — the heartbeat goroutine exits via
// connCtx cancellation in dialAndRun's defer. Kept as a method so the
// dialAndRun defer reads symmetrically (stopHeartbeat ↔ startHeartbeat).
func (q *QQChannel) stopHeartbeat() {}

// ---------------------------------------------------------------------------
// Dispatch routing (contract §1.7 + §5.7)
// ---------------------------------------------------------------------------

// routeDispatch handles op:0 events by t (event type). Phase 1 covers
// the events that produce inbound user messages + the session lifecycle
// events that need state side-effects.
func (q *QQChannel) routeDispatch(t string, d json.RawMessage) error {
	switch t {
	case "READY":
		var ready qqReadyData
		if err := json.Unmarshal(d, &ready); err != nil {
			return fmt.Errorf("unmarshal READY: %w", err)
		}
		q.seqMu.Lock()
		q.sessionID = ready.SessionID
		q.seqMu.Unlock()
		slog.Info("qq ws READY",
			"account", q.accountID, "session", ready.SessionID, "version", ready.Version)
		return nil

	case "RESUMED":
		// No state update — we already have session + lastSeq.
		slog.Info("qq ws RESUMED", "account", q.accountID)
		return nil

	case "GROUP_AT_MESSAGE_CREATE":
		return q.handleGroupAtMessage(d)

	case "C2C_MESSAGE_CREATE":
		return q.handleC2CMessage(d)

	case "GROUP_MESSAGE_CREATE":
		// Non-@ group message. fastclaw default policy (contract §6.6)
		// is to NOT respond — silently drop. Phase 2 may surface this as
		// a configurable behavior.
		slog.Debug("qq GROUP_MESSAGE_CREATE (non-@) — ignoring",
			"account", q.accountID)
		return nil

	case "GROUP_ADD_ROBOT", "GROUP_DEL_ROBOT", "GROUP_MSG_REJECT", "GROUP_MSG_RECEIVE":
		slog.Info("qq group lifecycle event",
			"account", q.accountID, "t", t)
		return nil

	default:
		// Interaction events, guild events, etc. — not handled in
		// Phase 1. Log at debug so a mis-routed event isn't silent.
		slog.Debug("qq ws unhandled dispatch type",
			"account", q.accountID, "t", t)
	}
	return nil
}

// handleGroupAtMessage maps a GROUP_AT_MESSAGE_CREATE payload to a
// bus.InboundMessage. Per Phase 1 brief, UserID is set to group_openid
// (NOT member_openid) — this is the 群绑群技巧: one /claim in the group
// authorizes everyone in that group, since slash_claim binds by
// (channel, UserID) and fastclaw treats all group members as the same
// "user" from the claim system's POV. member_openid surfaces as
// SenderName so the chatter is still identifiable in the UI.
//
// Divergence from contract §5.7 (which lists member_openid → UserID):
// intentional, documented in task brief + Phase 2 plan.
//
// Phase 3 additions (contract §2.3):
//   - recordPeerKind stores "group" for this ChatID so outbound
//     SendMessage routes to /v2/groups/ (not /v2/users/).
//   - Image attachments (content_type starts with image/) are
//     downloaded via qqSafeDownload (SSRF-guarded) and surfaced as
//     PhotoURLs + MediaItems so the agent can reason about them.
func (q *QQChannel) handleGroupAtMessage(d json.RawMessage) error {
	var ev qqGroupAtMessage
	if err := json.Unmarshal(d, &ev); err != nil {
		return fmt.Errorf("unmarshal GROUP_AT_MESSAGE_CREATE: %w", err)
	}

	content := strings.TrimSpace(ev.Content)

	// Claim gate (contract §5.5 scheme A). UserID=group_openid means
	// the closure checks against the group, not the individual member —
	// one /claim in the group authorizes everyone in it (群绑群技巧).
	// Phase 1 left this nil (allow all); Phase 2 wires it via
	// SetAllowedChecker at registration time.
	//
	// /claim exemption (chicken-and-egg, slash_claim.go:17-19): /claim is
	// NOT in writeSlashCommands — the owner MUST be able to redeem a code
	// BEFORE being bound as admin. Without this exemption the FIRST /claim
	// in a fresh install (Admins["qq"] empty → allowedChecker returns
	// false) would be dropped at the adapter layer and never reach
	// slash_claim.go, so the binding could never happen. The 6-digit code
	// in the message body IS the abuse gate. Other unbound messages are
	// still dropped.
	if q.allowedChecker != nil && !q.allowedChecker(ev.GroupOpenID) && !strings.HasPrefix(content, "/claim") {
		slog.Debug("qq group @ message dropped — group not claimed",
			"account", q.accountID, "group_openid", ev.GroupOpenID)
		return nil
	}

	// Phase 3: remember this chat is a group so outbound SendMessage
	// uses the /v2/groups/ endpoint.
	q.recordPeerKind(ev.GroupOpenID, "group")

	// Phase 3 (contract §2.3): download image attachments so the agent
	// can reason about them. URLs have short TTL — fetch eagerly.
	photoURLs, mediaItems := q.collectAttachments(ev.Attachments)

	senderName := ev.Author.Username
	if senderName == "" {
		senderName = ev.Author.MemberOpenID
	}

	slog.Info("qq group @ message",
		"account", q.accountID,
		"group_openid", ev.GroupOpenID,
		"member_openid", ev.Author.MemberOpenID,
		"len", len(content),
		"attachments", len(ev.Attachments),
		"images", len(photoURLs))

	q.mb.Inbound <- bus.InboundMessage{
		Channel:    "qq",
		AccountID:  q.accountID,
		ChatID:     ev.GroupOpenID, // group conversation key
		UserID:     ev.GroupOpenID, // ★ 群绑群: same UserID for all members
		PeerKind:   "group",
		MessageID:  ev.ID,
		Text:       content,
		SenderName: senderName,        // member_openid or username for UI display
		Mentions:   []string{"qqbot"}, // GROUP_AT_MESSAGE_CREATE implies @bot; lets routeGroup agentByMention recognize the trigger
		PhotoURLs:  photoURLs,
		MediaItems: mediaItems,
	}
	return nil
}

// handleC2CMessage maps a C2C_MESSAGE_CREATE payload to a bus.InboundMessage.
// Private chat: UserID = user_openid (stable per bot), ChatID = user_openid,
// PeerKind = dm. claim binds directly to this user.
//
// Phase 3 additions mirror the group path: recordPeerKind("dm") +
// attachment download.
func (q *QQChannel) handleC2CMessage(d json.RawMessage) error {
	var ev qqC2CMessage
	if err := json.Unmarshal(d, &ev); err != nil {
		return fmt.Errorf("unmarshal C2C_MESSAGE_CREATE: %w", err)
	}

	content := strings.TrimSpace(ev.Content)

	// Claim gate (contract §5.5 scheme A). Private chat: the user's
	// user_openid is what /claim bound to Admins["qq"].
	//
	// /claim exemption — see handleGroupAtMessage for full rationale.
	// Owner's first /claim in a fresh install (allowedChecker returns
	// false) must reach slash_claim.go to bind. Code is the abuse gate.
	if q.allowedChecker != nil && !q.allowedChecker(ev.Author.UserOpenID) && !strings.HasPrefix(content, "/claim") {
		slog.Debug("qq c2c message dropped — user not claimed",
			"account", q.accountID, "user_openid", ev.Author.UserOpenID)
		return nil
	}

	// Phase 3: C2C chat → /v2/users/ on outbound.
	q.recordPeerKind(ev.Author.UserOpenID, "dm")

	photoURLs, mediaItems := q.collectAttachments(ev.Attachments)

	senderName := ev.Author.Username
	if senderName == "" {
		senderName = ev.Author.UserOpenID
	}

	slog.Info("qq c2c message",
		"account", q.accountID,
		"user_openid", ev.Author.UserOpenID,
		"len", len(content),
		"attachments", len(ev.Attachments),
		"images", len(photoURLs))

	q.mb.Inbound <- bus.InboundMessage{
		Channel:    "qq",
		AccountID:  q.accountID,
		ChatID:     ev.Author.UserOpenID,
		UserID:     ev.Author.UserOpenID,
		PeerKind:   "dm",
		MessageID:  ev.ID,
		Text:       content,
		SenderName: senderName,
		PhotoURLs:  photoURLs,
		MediaItems: mediaItems,
	}
	return nil
}

// collectAttachments downloads each image attachment (content_type
// starting with "image/") via qqSafeDownload. Non-image attachments are
// logged at debug and skipped (Phase 3 only handles images; voice/video
// will land in a later phase if needed).
//
// Returns parallel slices: photoURLs for back-compat with the single-
// image PhotoURL/PhotoURLs fields, and mediaItems carrying the bytes
// for the agent loop to materialize into the session workspace.
func (q *QQChannel) collectAttachments(atts []qqAttachment) ([]string, []bus.MediaItem) {
	if len(atts) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var photoURLs []string
	var mediaItems []bus.MediaItem
	for _, a := range atts {
		if a.URL == "" {
			continue
		}
		ct := strings.ToLower(strings.TrimSpace(a.ContentType))
		if !strings.HasPrefix(ct, "image/") {
			slog.Debug("qq skipping non-image attachment",
				"account", q.accountID,
				"content_type", a.ContentType,
				"filename", a.Filename)
			continue
		}
		bytes, sniffedCT, err := qqAttachmentFetcher(ctx, q.httpClient, a.URL)
		if err != nil {
			slog.Warn("qq attachment download failed",
				"account", q.accountID, "url", a.URL, "error", err)
			// QQ attachment URLs are short-lived signed links — if the
			// eager download failed, the URL will be dead by the time
			// web/agent retries it, so don't surface it. Skip.
			continue
		}
		finalCT := ct
		if sniffedCT != "" {
			finalCT = sniffedCT
		}
		// Build a data: URL from the downloaded bytes. Two reasons:
		//  (1) QQ signed URLs expire fast — a data URL is durable.
		//  (2) PhotoURLs flows through loop.go's user-message image path
		//      (Role: user, ContentParts image_url) so the image renders
		//      on the USER side (right bubble) + reaches the model as
		//      vision input. MediaItems would route through workspace
		//      [Attached:] which web renders assistant-side (left).
		photoURLs = append(photoURLs, "data:"+finalCT+";base64,"+base64.StdEncoding.EncodeToString(bytes))
	}
	return photoURLs, mediaItems
}

// ---------------------------------------------------------------------------
// Close-code classification (contract §1.8)
// ---------------------------------------------------------------------------

type qqCloseAction int

const (
	qqCloseActionRetry        qqCloseAction = iota // unknown / transient — keep session, reconnect
	qqCloseActionNormal                            // 1000: graceful, don't reconnect
	qqCloseActionFatal                             // 4914/4915: robot offline/banned, exit
	qqCloseActionClearSession                      // 4006/4007/4009/4900-4913: clear session, Identify
	qqCloseActionRefreshToken                      // 4004: token invalid, refresh + retry
	qqCloseActionRateLimit                         // 4008: rate-limited, wait 60s
)

// classifyQQClose maps a QQ WS close code to a reconnect action. Pure
// function so tests can exhaustively cover the table.
func classifyQQClose(code int) qqCloseAction {
	switch {
	case code == qqCloseNormal:
		return qqCloseActionNormal
	case code == qqCloseAuthFailed:
		return qqCloseActionRefreshToken
	case code == qqCloseSessionInvalid, code == qqCloseSeqInvalid, code == qqCloseSessionTimeout:
		return qqCloseActionClearSession
	case code == qqCloseRateLimited:
		return qqCloseActionRateLimit
	case code >= qqCloseInternalStart && code <= qqCloseInternalEnd:
		return qqCloseActionClearSession
	case code == qqCloseRobotOffline || code == qqCloseRobotBanned:
		return qqCloseActionFatal
	default:
		// Unknown / transient network error — retry, keep session.
		return qqCloseActionRetry
	}
}

// ---------------------------------------------------------------------------
// REST: token + gateway URL (Phase 1 — no caching)
// ---------------------------------------------------------------------------

// qqExpiresInSeconds tolerates QQ returning expires_in as either a JSON
// number (7200) or a quoted string ("7200") — the live API has been
// observed returning the string form, which breaks a plain int field.
type qqExpiresInSeconds int

func (e *qqExpiresInSeconds) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "null" {
		return nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		*e = qqExpiresInSeconds(n)
		return nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if n, err := strconv.Atoi(s[1 : len(s)-1]); err == nil {
			*e = qqExpiresInSeconds(n)
			return nil
		}
	}
	return fmt.Errorf("qq: invalid expires_in %q", s)
}

type qqAccessTokenResponse struct {
	AccessToken string             `json:"access_token"`
	ExpiresIn   qqExpiresInSeconds `json:"expires_in,omitempty"`
}

// getAccessToken returns a live access token from the package-level
// cache (qq_token.go). Phase 2: the cache isolates by appID, single-
// flights concurrent callers, and runs a background refresh goroutine
// 5min before expiry (contract §3.2 + §6.7). On a hard miss it POSTs
// bots.qq.com/app/getAppAccessToken.
func (q *QQChannel) getAccessToken(ctx context.Context) (string, error) {
	return qqGetToken(ctx, q.appID, q.appSecret)
}

type qqGatewayResponse struct {
	URL string `json:"url"`
}

// getGatewayUrl fetches wss URL from /gateway. Single-shard, so we use
// /gateway (not /gateway/bot) per contract §1.1.
func (q *QQChannel) getGatewayUrl(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, qqGatewayURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", qqAuthScheme+" "+token)
	req.Header.Set("User-Agent", qqUserAgent)

	resp, err := q.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("gateway http: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("gateway read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gateway HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed qqGatewayResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("gateway parse: %w", err)
	}
	if parsed.URL == "" {
		return "", fmt.Errorf("gateway URL empty in response")
	}
	return parsed.URL, nil
}

// ---------------------------------------------------------------------------
// Error + wire types
// ---------------------------------------------------------------------------

// Sentinel errors for control-flow signalling inside Start ↔ dialAndRun.
var (
	errQQServerReconnect = errors.New("qq server requested reconnect")
	errQQFatalExit       = errors.New("qq fatal close code")
)

// qqCloseError wraps a read-side close error with the extracted close
// code. gorilla/websocket returns *websocket.CloseError via errors.As
// when the server sent a close frame; non-close errors (network drops,
// ctx-cancel) yield Code = -1.
type qqCloseError struct {
	Code int
	Err  error
}

func (e qqCloseError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("qq ws close code=%d: %v", e.Code, e.Err)
	}
	return fmt.Sprintf("qq ws close code=%d", e.Code)
}

func (e qqCloseError) Unwrap() error { return e.Err }

// wrapQQCloseError extracts the close code from a gorilla/websocket
// error. Returns the original error unwrapped if it's not a close.
func wrapQQCloseError(err error) error {
	if err == nil {
		return nil
	}
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return qqCloseError{Code: ce.Code, Err: err}
	}
	// Non-close error (TCP reset, ctx cancel, etc.). Classify as retry.
	return qqCloseError{Code: -1, Err: err}
}

// qqFrame is the inbound envelope (contract §1.3 + §1.7).
type qqFrame struct {
	Op int             `json:"op"`
	S  *int            `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
	D  json.RawMessage `json:"d,omitempty"`
}

// qqHelloData is op:10 d.
type qqHelloData struct {
	HeartbeatInterval int `json:"heartbeat_interval"`
}

// qqIdentifyPayload is the op:2 outbound body (contract §1.4).
type qqIdentifyPayload struct {
	Op int            `json:"op"`
	D  qqIdentifyData `json:"d"`
}

type qqIdentifyData struct {
	Token   string `json:"token"`
	Intents int    `json:"intents"`
	Shard   [2]int `json:"shard"`
}

// qqResumePayload is the op:6 outbound body (contract §1.6).
type qqResumePayload struct {
	Op int          `json:"op"`
	D  qqResumeData `json:"d"`
}

type qqResumeData struct {
	Token     string `json:"token"`
	SessionID string `json:"session_id"`
	Seq       int    `json:"seq"`
}

// qqHeartbeatPayload is the op:1 outbound body. D is lastSeq or nil.
// Using `any` + json.Marshal gives us `null` when D is nil — matches
// contract §1.5.
type qqHeartbeatPayload struct {
	Op int `json:"op"`
	D  any `json:"d"`
}

// qqReadyData is the READY dispatch payload (contract §1.7).
type qqReadyData struct {
	SessionID string `json:"session_id"`
	Version   int    `json:"version,omitempty"`
}

// qqAttachment is one entry in the inbound event's attachments[] list
// (contract §2.3). ContentType drives the Phase 3 download path:
// image/* prefixes are fetched via qqSafeDownload; others are logged
// and skipped (later phases may handle voice/video).
type qqAttachment struct {
	ContentType string `json:"content_type,omitempty"`
	Filename    string `json:"filename,omitempty"`
	URL         string `json:"url,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	Size        int    `json:"size,omitempty"`
}

// qqGroupAtMessage is GROUP_AT_MESSAGE_CREATE d (contract §2.1).
// Phase 3 adds Attachments for image download.
type qqGroupAtMessage struct {
	ID          string `json:"id"`
	GroupOpenID string `json:"group_openid"`
	Content     string `json:"content"`
	Author      struct {
		ID           string `json:"id,omitempty"`
		MemberOpenID string `json:"member_openid,omitempty"`
		Username     string `json:"username,omitempty"`
	} `json:"author"`
	Attachments []qqAttachment `json:"attachments,omitempty"`
	Timestamp   string         `json:"timestamp,omitempty"`
}

// qqC2CMessage is C2C_MESSAGE_CREATE d (contract §2.2).
type qqC2CMessage struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Author  struct {
		ID         string `json:"id,omitempty"`
		UserOpenID string `json:"user_openid,omitempty"`
		Username   string `json:"username,omitempty"`
	} `json:"author"`
	Attachments []qqAttachment `json:"attachments,omitempty"`
	Timestamp   string         `json:"timestamp,omitempty"`
}
