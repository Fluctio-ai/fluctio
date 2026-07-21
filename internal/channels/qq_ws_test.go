package channels

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/bus"
)

// captureSend returns a sendFunc that records every WriteJSON argument
// into the returned slice. Tests inspect the captured payloads to
// assert on Identify/Resume/Heartbeat shape.
func captureSend() (sendFunc, *[]any) {
	var mu sync.Mutex
	got := make([]any, 0)
	fn := func(v any) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, v)
		return nil
	}
	return fn, &got
}

// newTestQQ constructs a QQChannel backed by a fresh buffered bus.
func newTestQQ(t *testing.T) (*QQChannel, *bus.MessageBus) {
	t.Helper()
	mb := bus.New()
	q, err := NewQQChannel("app-123", "secret-456", "acct-1", mb)
	if err != nil {
		t.Fatalf("NewQQChannel: %v", err)
	}
	return q, mb
}

// drainInbound pulls one InboundMessage off the bus with a timeout so a
// missing dispatch fails the test fast instead of hanging.
func drainInbound(t *testing.T, mb *bus.MessageBus, timeout time.Duration) bus.InboundMessage {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case m := <-mb.Inbound:
		return m
	case <-timer.C:
		t.Fatalf("drainInbound: timeout after %v", timeout)
		return bus.InboundMessage{}
	}
}

// ----- Hello → Identify vs Resume ---------------------------------------------

func TestQQHelloIdentifyWhenNoSession(t *testing.T) {
	q, _ := newTestQQ(t)
	send, got := captureSend()

	helloRaw, _ := json.Marshal(qqFrame{
		Op: qqOpHello,
		D:  json.RawMessage(`{"heartbeat_interval":50}`),
	})
	if err := q.handleServerMessage(context.Background(), helloRaw, send); err != nil {
		t.Fatalf("handleServerMessage Hello: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("expected 1 send (Identify), got %d", len(*got))
	}
	id, ok := (*got)[0].(qqIdentifyPayload)
	if !ok {
		t.Fatalf("expected qqIdentifyPayload, got %T", (*got)[0])
	}
	if id.Op != qqOpIdentify {
		t.Errorf("Identify op = %d, want %d", id.Op, qqOpIdentify)
	}
	if id.D.Intents != qqIntentsFull {
		t.Errorf("Intents = %d, want %d", id.D.Intents, qqIntentsFull)
	}
	if id.D.Shard != [2]int{0, 1} {
		t.Errorf("Shard = %v, want [0 1]", id.D.Shard)
	}
}

func TestQQHelloResumeWhenSessionExists(t *testing.T) {
	q, _ := newTestQQ(t)
	// Seed session state — pretend we got READY previously.
	q.sessionID = "SESSION_ABC"
	seq := 42
	q.lastSeq = &seq

	send, got := captureSend()
	helloRaw, _ := json.Marshal(qqFrame{
		Op: qqOpHello,
		D:  json.RawMessage(`{"heartbeat_interval":100}`),
	})
	if err := q.handleServerMessage(context.Background(), helloRaw, send); err != nil {
		t.Fatalf("handleServerMessage Hello: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("expected 1 send (Resume), got %d", len(*got))
	}
	res, ok := (*got)[0].(qqResumePayload)
	if !ok {
		t.Fatalf("expected qqResumePayload, got %T", (*got)[0])
	}
	if res.Op != qqOpResume {
		t.Errorf("Resume op = %d, want %d", res.Op, qqOpResume)
	}
	if res.D.SessionID != "SESSION_ABC" {
		t.Errorf("Resume session_id = %q, want SESSION_ABC", res.D.SessionID)
	}
	if res.D.Seq != 42 {
		t.Errorf("Resume seq = %d, want 42", res.D.Seq)
	}
}

func TestQQHelloResumeFallsBackToIdentifyWhenSessionOnly(t *testing.T) {
	// sessionID set but lastSeq nil — can't Resume without a seq.
	// Should Identify, not Resume.
	q, _ := newTestQQ(t)
	q.sessionID = "SESSION_X"
	q.lastSeq = nil

	send, got := captureSend()
	helloRaw, _ := json.Marshal(qqFrame{
		Op: qqOpHello,
		D:  json.RawMessage(`{"heartbeat_interval":100}`),
	})
	if err := q.handleServerMessage(context.Background(), helloRaw, send); err != nil {
		t.Fatalf("handleServerMessage Hello: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("expected 1 send, got %d", len(*got))
	}
	if _, ok := (*got)[0].(qqIdentifyPayload); !ok {
		t.Fatalf("expected Identify when lastSeq nil, got %T", (*got)[0])
	}
}

// ----- Dispatch READY / RESUMED -----------------------------------------------

func TestQQDispatchREADYCapturesSessionID(t *testing.T) {
	q, _ := newTestQQ(t)
	send, _ := captureSend()

	raw, _ := json.Marshal(qqFrame{
		Op: qqOpDispatch,
		T:  "READY",
		S:  intPtr(1),
		D:  json.RawMessage(`{"session_id":"NEW_SESSION","version":1}`),
	})
	if err := q.handleServerMessage(context.Background(), raw, send); err != nil {
		t.Fatalf("READY: %v", err)
	}
	q.seqMu.Lock()
	defer q.seqMu.Unlock()
	if q.sessionID != "NEW_SESSION" {
		t.Errorf("sessionID = %q, want NEW_SESSION", q.sessionID)
	}
	if q.lastSeq == nil || *q.lastSeq != 1 {
		t.Errorf("lastSeq = %v, want 1", q.lastSeq)
	}
}

func TestQQDispatchRESUMEDNoStateChange(t *testing.T) {
	q, _ := newTestQQ(t)
	q.sessionID = "KEEP"
	seq := 99
	q.lastSeq = &seq
	send, _ := captureSend()

	raw, _ := json.Marshal(qqFrame{
		Op: qqOpDispatch,
		T:  "RESUMED",
		S:  intPtr(100),
	})
	if err := q.handleServerMessage(context.Background(), raw, send); err != nil {
		t.Fatalf("RESUMED: %v", err)
	}
	q.seqMu.Lock()
	defer q.seqMu.Unlock()
	if q.sessionID != "KEEP" {
		t.Errorf("sessionID changed: %q", q.sessionID)
	}
	// lastSeq DOES update — RESUMED frames carry a seq like any dispatch.
	if q.lastSeq == nil || *q.lastSeq != 100 {
		t.Errorf("lastSeq = %v, want 100", q.lastSeq)
	}
}

// ----- Inbound dispatch: group @ vs c2c ---------------------------------------

func TestQQDispatchGroupAtMessage(t *testing.T) {
	q, mb := newTestQQ(t)
	send, _ := captureSend()

	payload := `{
		"id": "MSG_GROUP_1",
		"group_openid": "GROUP_OID_HEX",
		"content": " 你好",
		"author": {
			"member_openid": "MEMBER_OID_HEX",
			"username": "Alice"
		},
		"timestamp": "1700000000"
	}`
	raw, _ := json.Marshal(qqFrame{
		Op: qqOpDispatch,
		T:  "GROUP_AT_MESSAGE_CREATE",
		S:  intPtr(7),
		D:  json.RawMessage(payload),
	})
	if err := q.handleServerMessage(context.Background(), raw, send); err != nil {
		t.Fatalf("group @: %v", err)
	}
	msg := drainInbound(t, mb, time.Second)

	// 群绑群技巧: UserID=group_openid (same for all members of the group)
	// so one /claim authorizes everyone. member_openid surfaces as SenderName.
	if msg.Channel != "qq" {
		t.Errorf("Channel = %q, want qq", msg.Channel)
	}
	if msg.AccountID != "acct-1" {
		t.Errorf("AccountID = %q, want acct-1", msg.AccountID)
	}
	if msg.ChatID != "GROUP_OID_HEX" {
		t.Errorf("ChatID = %q, want GROUP_OID_HEX", msg.ChatID)
	}
	if msg.UserID != "GROUP_OID_HEX" {
		t.Errorf("UserID = %q, want GROUP_OID_HEX (群绑群技巧)", msg.UserID)
	}
	if msg.PeerKind != "group" {
		t.Errorf("PeerKind = %q, want group", msg.PeerKind)
	}
	if msg.MessageID != "MSG_GROUP_1" {
		t.Errorf("MessageID = %q, want MSG_GROUP_1", msg.MessageID)
	}
	if msg.Text != "你好" {
		t.Errorf("Text = %q, want 你好 (leading space trimmed)", msg.Text)
	}
	if msg.SenderName != "Alice" {
		t.Errorf("SenderName = %q, want Alice", msg.SenderName)
	}
}

func TestQQDispatchGroupAtMessageSenderNameFallback(t *testing.T) {
	// When username is missing, SenderName should fall back to member_openid
	// so the UI still has something readable.
	q, mb := newTestQQ(t)
	send, _ := captureSend()

	payload := `{
		"id": "MSG_X",
		"group_openid": "G1",
		"content": "hi",
		"author": {"member_openid": "MEM_777"}
	}`
	raw, _ := json.Marshal(qqFrame{
		Op: qqOpDispatch,
		T:  "GROUP_AT_MESSAGE_CREATE",
		S:  intPtr(1),
		D:  json.RawMessage(payload),
	})
	if err := q.handleServerMessage(context.Background(), raw, send); err != nil {
		t.Fatalf("group @: %v", err)
	}
	msg := drainInbound(t, mb, time.Second)
	if msg.SenderName != "MEM_777" {
		t.Errorf("SenderName fallback = %q, want MEM_777", msg.SenderName)
	}
	if msg.UserID != "G1" {
		t.Errorf("UserID = %q, want G1 (group_openid)", msg.UserID)
	}
}

func TestQQDispatchC2CMessage(t *testing.T) {
	q, mb := newTestQQ(t)
	send, _ := captureSend()

	payload := `{
		"id": "MSG_DM_1",
		"content": "hello",
		"author": {
			"user_openid": "USER_OID_HEX",
			"username": "Bob"
		},
		"timestamp": "1700000001"
	}`
	raw, _ := json.Marshal(qqFrame{
		Op: qqOpDispatch,
		T:  "C2C_MESSAGE_CREATE",
		S:  intPtr(9),
		D:  json.RawMessage(payload),
	})
	if err := q.handleServerMessage(context.Background(), raw, send); err != nil {
		t.Fatalf("c2c: %v", err)
	}
	msg := drainInbound(t, mb, time.Second)

	if msg.ChatID != "USER_OID_HEX" {
		t.Errorf("ChatID = %q, want USER_OID_HEX", msg.ChatID)
	}
	if msg.UserID != "USER_OID_HEX" {
		t.Errorf("UserID = %q, want USER_OID_HEX", msg.UserID)
	}
	if msg.PeerKind != "dm" {
		t.Errorf("PeerKind = %q, want dm", msg.PeerKind)
	}
	if msg.MessageID != "MSG_DM_1" {
		t.Errorf("MessageID = %q, want MSG_DM_1", msg.MessageID)
	}
	if msg.Text != "hello" {
		t.Errorf("Text = %q, want hello", msg.Text)
	}
	if msg.SenderName != "Bob" {
		t.Errorf("SenderName = %q, want Bob", msg.SenderName)
	}
}

func TestQQDispatchC2CFallbackSenderName(t *testing.T) {
	q, mb := newTestQQ(t)
	send, _ := captureSend()

	payload := `{
		"id": "M",
		"content": "x",
		"author": {"user_openid": "U_OID"}
	}`
	raw, _ := json.Marshal(qqFrame{
		Op: qqOpDispatch,
		T:  "C2C_MESSAGE_CREATE",
		S:  intPtr(1),
		D:  json.RawMessage(payload),
	})
	if err := q.handleServerMessage(context.Background(), raw, send); err != nil {
		t.Fatalf("c2c: %v", err)
	}
	msg := drainInbound(t, mb, time.Second)
	if msg.SenderName != "U_OID" {
		t.Errorf("SenderName = %q, want U_OID (fallback)", msg.SenderName)
	}
}

// ----- Heartbeat ACK + lastSeq progression ------------------------------------

func TestQQHeartbeatACKIsNoop(t *testing.T) {
	q, _ := newTestQQ(t)
	send, got := captureSend()

	raw, _ := json.Marshal(qqFrame{Op: qqOpHeartbeatACK})
	if err := q.handleServerMessage(context.Background(), raw, send); err != nil {
		t.Fatalf("HeartbeatACK: %v", err)
	}
	if len(*got) != 0 {
		t.Errorf("HeartbeatACK should not trigger any send, got %d sends", len(*got))
	}
}

func TestQQLastSeqUpdatesAcrossDispatches(t *testing.T) {
	q, _ := newTestQQ(t)
	send, _ := captureSend()

	for _, s := range []int{1, 5, 12, 42} {
		raw, _ := json.Marshal(qqFrame{
			Op: qqOpDispatch,
			T:  "SOME_EVENT",
			S:  intPtr(s),
		})
		if err := q.handleServerMessage(context.Background(), raw, send); err != nil {
			t.Fatalf("dispatch s=%d: %v", s, err)
		}
		q.seqMu.Lock()
		got := -1
		if q.lastSeq != nil {
			got = *q.lastSeq
		}
		q.seqMu.Unlock()
		if got != s {
			t.Errorf("after dispatch s=%d: lastSeq = %d", s, got)
		}
	}
}

func TestQQDispatchWithoutSeqDoesNotResetLastSeq(t *testing.T) {
	// A dispatch with no s field should NOT advance lastSeq (resume uses it).
	q, _ := newTestQQ(t)
	send, _ := captureSend()

	// First, establish lastSeq=10.
	raw1, _ := json.Marshal(qqFrame{
		Op: qqOpDispatch, T: "READY", S: intPtr(10),
		D: json.RawMessage(`{"session_id":"S"}`),
	})
	if err := q.handleServerMessage(context.Background(), raw1, send); err != nil {
		t.Fatalf("READY: %v", err)
	}

	// Then a dispatch with no s field (rare but possible for some events).
	raw2, _ := json.Marshal(qqFrame{
		Op: qqOpDispatch, T: "RESUMED",
	})
	if err := q.handleServerMessage(context.Background(), raw2, send); err != nil {
		t.Fatalf("RESUMED: %v", err)
	}

	q.seqMu.Lock()
	defer q.seqMu.Unlock()
	if q.lastSeq == nil || *q.lastSeq != 10 {
		t.Errorf("lastSeq = %v, want 10 (preserved when s absent)", q.lastSeq)
	}
}

// ----- op:7 / op:9 reconnect control ------------------------------------------

func TestQQReconnectOpReturnsSentinel(t *testing.T) {
	q, _ := newTestQQ(t)
	send, _ := captureSend()

	raw, _ := json.Marshal(qqFrame{Op: qqOpReconnect})
	err := q.handleServerMessage(context.Background(), raw, send)
	if !errors.Is(err, errQQServerReconnect) {
		t.Errorf("op:7 err = %v, want errQQServerReconnect", err)
	}
}

func TestQQInvalidSessionTrueKeepsSession(t *testing.T) {
	q, _ := newTestQQ(t)
	q.sessionID = "KEEP"
	seq := 5
	q.lastSeq = &seq
	send, _ := captureSend()

	raw, _ := json.Marshal(qqFrame{
		Op: qqOpInvalidSession,
		D:  json.RawMessage(`true`),
	})
	err := q.handleServerMessage(context.Background(), raw, send)
	if !errors.Is(err, errQQServerReconnect) {
		t.Errorf("op:9 err = %v, want errQQServerReconnect", err)
	}
	q.seqMu.Lock()
	defer q.seqMu.Unlock()
	if q.sessionID != "KEEP" {
		t.Errorf("op:9 d=true should keep session, got %q", q.sessionID)
	}
	if q.lastSeq == nil || *q.lastSeq != 5 {
		t.Errorf("op:9 d=true should keep lastSeq, got %v", q.lastSeq)
	}
}

func TestQQInvalidSessionFalseClears(t *testing.T) {
	q, _ := newTestQQ(t)
	q.sessionID = "DROP"
	seq := 5
	q.lastSeq = &seq
	send, _ := captureSend()

	raw, _ := json.Marshal(qqFrame{
		Op: qqOpInvalidSession,
		D:  json.RawMessage(`false`),
	})
	err := q.handleServerMessage(context.Background(), raw, send)
	if !errors.Is(err, errQQServerReconnect) {
		t.Errorf("op:9 err = %v, want errQQServerReconnect", err)
	}
	q.seqMu.Lock()
	defer q.seqMu.Unlock()
	if q.sessionID != "" {
		t.Errorf("op:9 d=false should clear session, got %q", q.sessionID)
	}
	if q.lastSeq != nil {
		t.Errorf("op:9 d=false should nil lastSeq, got %v", *q.lastSeq)
	}
}

func TestQQInvalidSessionUnparseableClears(t *testing.T) {
	// Unparseable d → safer to clear (forces fresh Identify).
	q, _ := newTestQQ(t)
	q.sessionID = "OLD"
	send, _ := captureSend()

	raw, _ := json.Marshal(qqFrame{
		Op: qqOpInvalidSession,
		D:  json.RawMessage(`"not-a-bool"`),
	})
	if err := q.handleServerMessage(context.Background(), raw, send); err == nil {
		t.Fatalf("expected error for unparseable op:9")
	}
	q.seqMu.Lock()
	defer q.seqMu.Unlock()
	if q.sessionID != "" {
		t.Errorf("session should be cleared on unparseable op:9, got %q", q.sessionID)
	}
}

// ----- Heartbeat payload construction -----------------------------------------

// TestQQHeartbeatSendsLastSeq drives the real heartbeat goroutine with
// a tiny interval and asserts the captured payload matches the contract:
// `{op:1, d:<lastSeq or null>}`. The goroutine is the production code
// path — no extracted helper, on purpose.
func TestQQHeartbeatSendsLastSeq(t *testing.T) {
	q, _ := newTestQQ(t)
	send, got := captureSend()

	// Pre-seed lastSeq so the heartbeat payload carries it.
	seq := 77
	q.lastSeq = &seq

	// 30ms interval — fast enough for a test, slow enough that the
	// goroutine won't spin.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.startHeartbeat(ctx, 30, send)

	// Wait for at least one heartbeat tick.
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case <-deadline:
			t.Fatalf("heartbeat never fired within 500ms")
		default:
		}
		if len(*got) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Inspect the first heartbeat payload.
	first := (*got)[0]
	hb, ok := first.(qqHeartbeatPayload)
	if !ok {
		t.Fatalf("expected qqHeartbeatPayload, got %T", first)
	}
	if hb.Op != qqOpHeartbeat {
		t.Errorf("hb op = %d, want %d", hb.Op, qqOpHeartbeat)
	}
	if hb.D != 77 {
		t.Errorf("hb d = %v, want 77 (lastSeq)", hb.D)
	}
}

func TestQQHeartbeatSendsNullWhenNoSeq(t *testing.T) {
	q, _ := newTestQQ(t)
	send, got := captureSend()

	// No lastSeq — heartbeat d should marshal as JSON null.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.startHeartbeat(ctx, 30, send)

	// Wait for first tick.
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case <-deadline:
			t.Fatalf("heartbeat never fired within 500ms")
		default:
		}
		if len(*got) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	first := (*got)[0]
	hb, ok := first.(qqHeartbeatPayload)
	if !ok {
		t.Fatalf("expected qqHeartbeatPayload, got %T", first)
	}
	if hb.D != nil {
		t.Errorf("hb d = %v, want nil (no lastSeq yet)", hb.D)
	}
	// Marshal to confirm it serializes as JSON null (contract §1.5).
	out, _ := json.Marshal(hb)
	if string(out) != `{"op":1,"d":null}` {
		t.Errorf("hb JSON = %s, want {\"op\":1,\"d\":null}", string(out))
	}
}

// lenSync / syncGet helpers removed — direct slice reads are fine for
// test assertions (captureSend's mutex guards the write side; we read
// after the goroutine has observably written via the len check).

// ----- Close code classification (table-driven, all codes) --------------------

func TestClassifyQQClose(t *testing.T) {
	cases := []struct {
		code int
		want qqCloseAction
	}{
		{1000, qqCloseActionNormal},
		{4004, qqCloseActionRefreshToken},
		{4006, qqCloseActionClearSession},
		{4007, qqCloseActionClearSession},
		{4008, qqCloseActionRateLimit},
		{4009, qqCloseActionClearSession},
		{4900, qqCloseActionClearSession},
		{4905, qqCloseActionClearSession},
		{4913, qqCloseActionClearSession},
		{4914, qqCloseActionFatal},
		{4915, qqCloseActionFatal},
		{4999, qqCloseActionRetry}, // unknown high → retry
		{4010, qqCloseActionRetry}, // unknown 4xxx → retry
		{-1, qqCloseActionRetry},   // no close frame (network drop)
	}
	for _, c := range cases {
		got := classifyQQClose(c.code)
		if got != c.want {
			t.Errorf("classifyQQClose(%d) = %v, want %v", c.code, got, c.want)
		}
	}
}

// ----- Backoff ladder ---------------------------------------------------------

func TestQQNextBackoffProgression(t *testing.T) {
	q, _ := newTestQQ(t)
	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		5 * time.Second,
		10 * time.Second,
		30 * time.Second,
		60 * time.Second,
	}
	for i, w := range want {
		q.attempts = i + 1 // attempts > 0 triggers backoff selection
		if got := q.nextBackoff(); got != w {
			t.Errorf("nextBackoff(attempt=%d) = %v, want %v", i+1, got, w)
		}
	}
	// Past the ladder → capped at 60s.
	q.attempts = 50
	if got := q.nextBackoff(); got != 60*time.Second {
		t.Errorf("nextBackoff(attempt=50) = %v, want 60s (capped)", got)
	}
	q.attempts = 100
	if got := q.nextBackoff(); got != 60*time.Second {
		t.Errorf("nextBackoff(attempt=100) = %v, want 60s (capped)", got)
	}
}

// ----- Unknown OP / Dispatch handling ----------------------------------------

func TestQQUnknownOpDoesntCrash(t *testing.T) {
	q, _ := newTestQQ(t)
	send, got := captureSend()

	raw, _ := json.Marshal(qqFrame{Op: 999})
	if err := q.handleServerMessage(context.Background(), raw, send); err != nil {
		t.Errorf("unknown op should not error, got %v", err)
	}
	if len(*got) != 0 {
		t.Errorf("unknown op should not send, got %d sends", len(*got))
	}
}

func TestQQUnhandledDispatchType(t *testing.T) {
	q, mb := newTestQQ(t)
	send, _ := captureSend()

	// INTERACTION_CREATE — not implemented in Phase 1.
	raw, _ := json.Marshal(qqFrame{
		Op: qqOpDispatch,
		T:  "INTERACTION_CREATE",
		S:  intPtr(1),
		D:  json.RawMessage(`{"id":"X"}`),
	})
	if err := q.handleServerMessage(context.Background(), raw, send); err != nil {
		t.Errorf("unhandled dispatch should not error, got %v", err)
	}
	// No inbound should be produced.
	select {
	case m := <-mb.Inbound:
		t.Errorf("unhandled dispatch should not emit inbound, got %+v", m)
	case <-time.After(50 * time.Millisecond):
		// Good — no message arrived.
	}
}

// ----- Malformed input --------------------------------------------------------

func TestQQMalformedJSONReturnsError(t *testing.T) {
	q, _ := newTestQQ(t)
	send, _ := captureSend()

	err := q.handleServerMessage(context.Background(), []byte(`not json`), send)
	if err == nil {
		t.Fatalf("expected error for malformed JSON")
	}
}

func TestQQEmptyFrameNoOp(t *testing.T) {
	q, _ := newTestQQ(t)
	send, got := captureSend()

	// `{}` — no op field. json.Unmarshal into qqFrame leaves Op=0,
	// which is qqOpDispatch, but with empty T — falls through to
	// unhandled dispatch debug log.
	raw := []byte(`{}`)
	if err := q.handleServerMessage(context.Background(), raw, send); err != nil {
		t.Errorf("empty frame should not error, got %v", err)
	}
	if len(*got) != 0 {
		t.Errorf("empty frame should not send, got %d sends", len(*got))
	}
}

// ----- Stubs (Phase 3 placeholders) -------------------------------------------

func TestQQStubsReturnNil(t *testing.T) {
	q, _ := newTestQQ(t)
	if err := q.Send("chat", "hi"); err != nil {
		t.Errorf("Send stub should return nil, got %v", err)
	}
	if err := q.SendMessage(bus.OutboundMessage{ChatID: "c", Text: "x"}); err != nil {
		t.Errorf("SendMessage stub should return nil, got %v", err)
	}
	if err := q.SendTyping("c"); err != nil {
		t.Errorf("SendTyping stub should return nil, got %v", err)
	}
}

// ----- Constructor validation -------------------------------------------------

func TestQQNewValidation(t *testing.T) {
	mb := bus.New()
	cases := []struct {
		name     string
		appID    string
		secret   string
		account  string
		wantErr  string
	}{
		{"missing appID", "", "s", "a", "appID"},
		{"missing secret", "a", "", "a", "appSecret"},
		{"missing account", "a", "s", "", "accountID"},
		{"nil bus", "a", "s", "a", "message bus"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var busRef *bus.MessageBus
			if c.wantErr != "message bus" {
				busRef = mb
			}
			_, err := NewQQChannel(c.appID, c.secret, c.account, busRef)
			if err == nil {
				t.Fatalf("expected error containing %q", c.wantErr)
			}
			if !contains(err.Error(), c.wantErr) {
				t.Errorf("err = %q, want substring %q", err.Error(), c.wantErr)
			}
		})
	}
}

func TestQQInterfaceMethods(t *testing.T) {
	q, _ := newTestQQ(t)
	if q.Name() != "qq" {
		t.Errorf("Name = %q, want qq", q.Name())
	}
	if q.AccountID() != "acct-1" {
		t.Errorf("AccountID = %q, want acct-1", q.AccountID())
	}
	if q.BotUsername() != "" {
		t.Errorf("BotUsername = %q, want empty (QQ has no username concept)", q.BotUsername())
	}
}

// ----- FailureReporter -------------------------------------------------------

func TestQQOnFailedCallback(t *testing.T) {
	q, _ := newTestQQ(t)
	var mu sync.Mutex
	got := ""
	q.OnFailed(func(accountID, reason string) {
		mu.Lock()
		defer mu.Unlock()
		got = accountID + ":" + reason
	})
	q.fireFailed("offline_banned")
	if got != "acct-1:offline_banned" {
		t.Errorf("OnFailed got %q, want acct-1:offline_banned", got)
	}
}

// ----- Channel interface compile-time check -----------------------------------

var _ Channel = (*QQChannel)(nil)
var _ FailureReporter = (*QQChannel)(nil)

// ----- helpers ----------------------------------------------------------------

func intPtr(n int) *int { return &n }

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
