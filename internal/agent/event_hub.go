package agent

import (
	"context"
	"sync"
)

// EventEnvelope is a ChatEvent stamped with the persistent seq the
// store assigned at append time. Subscribers use Seq to dedup against
// events they've already replayed via ListSessionEventsSince.
type EventEnvelope struct {
	Seq   int64
	Event ChatEvent
}

// EventHub is the in-process pub/sub for live chat events. Subscribers
// (the SSE chat-subscribe handler) register per (userID, agentID,
// sessionKey); publishers (emitEvent on the agent loop, fanned out to
// the hub) push envelopes that include the persisted seq so reconnect
// resume can stitch back together cleanly.
//
// In-memory only — multi-pod deploys need to swap this for redis
// pub/sub or similar (same shape as the WebChannel limitation called
// out elsewhere).
type EventHub struct {
	mu   sync.RWMutex
	subs map[string][]chan EventEnvelope
}

// NewEventHub returns an empty hub.
func NewEventHub() *EventHub {
	return &EventHub{subs: make(map[string][]chan EventEnvelope)}
}

// Subscribe registers a buffered channel for one (user, agent,
// session) tuple. The cleanup func MUST be deferred — without it the
// hub leaks goroutines and channels on reconnect churn.
func (h *EventHub) Subscribe(userID, agentID, sessionKey string) (<-chan EventEnvelope, func()) {
	key := hubKey(userID, agentID, sessionKey)
	ch := make(chan EventEnvelope, 256)
	h.mu.Lock()
	h.subs[key] = append(h.subs[key], ch)
	h.mu.Unlock()
	cleanup := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		list := h.subs[key]
		for i, c := range list {
			if c == ch {
				h.subs[key] = append(list[:i], list[i+1:]...)
				close(ch)
				break
			}
		}
		if len(h.subs[key]) == 0 {
			delete(h.subs, key)
		}
	}
	return ch, cleanup
}

// Publish fans an envelope out to every current subscriber. Slow
// consumers (full buffer) are skipped for persisted event types — a
// stuck client can't stall the agent loop and the event is replayed
// from session_events on reconnect anyway.
//
// content_delta is the exception: it is NOT persisted (seq=-1) and is
// the sole source of the live typing animation. A dropped delta is a
// permanently missing token in the active bubble, so we block here
// (back-pressure to the agent loop) until the subscriber drains or ctx
// ends. The active POST handler is the only real consumer — the
// subscribe handler skips content_delta — so this never stalls
// unrelated tabs. GLM-class thinking models burst content_delta after
// a long reasoning phase; without back-pressure the buffer can fill
// before the browser (rendering a long markdown bubble) drains it,
// which surfaced as "GLM 漏字, 刷新才完整".
func (h *EventHub) Publish(ctx context.Context, userID, agentID, sessionKey string, env EventEnvelope) {
	key := hubKey(userID, agentID, sessionKey)
	h.mu.RLock()
	subs := append([]chan EventEnvelope(nil), h.subs[key]...)
	h.mu.RUnlock()
	for _, ch := range subs {
		h.deliver(ch, env, ctx)
	}
}

// deliver sends env to one subscriber. The recover guards against the
// race where a subscriber's cleanup closes ch between Publish's RUnlock
// and this send — a panic on a closed channel would otherwise kill the
// agent goroutine. The blocking content_delta path widens that window,
// making the recover load-bearing rather than decorative.
func (h *EventHub) deliver(ch chan EventEnvelope, env EventEnvelope, ctx context.Context) {
	defer func() { recover() }()
	if env.Event.Type == "content_delta" {
		select {
		case ch <- env:
		case <-ctx.Done():
		}
		return
	}
	select {
	case ch <- env:
	default:
	}
}

func hubKey(userID, agentID, sessionKey string) string {
	return userID + "/" + agentID + "/" + sessionKey
}

// EventSink is the persistence side of the chat-events pipeline. The
// store.Store interface's AppendSessionEvent satisfies this exactly, so
// the gateway can pass its store as-is.
type EventSink interface {
	AppendSessionEvent(ctx context.Context, agentID, sessionKey, eventType string, data []byte) (int64, error)
}

// streamCtx carries the per-turn handles emitEvent reaches for:
// the legacy in-memory ChatEvent channel (consumed by handleChatStream
// while the client is connected), the persistent sink, the hub, and
// the address keys (userID, agentID, sessionKey) — these last three
// can't be derived from the agent struct because the agent runs on
// behalf of the chatter, not its owner.
type streamCtx struct {
	channel    chan<- ChatEvent
	sink       EventSink
	hub        *EventHub
	userID     string
	agentID    string
	sessionKey string
}

type streamCtxKey struct{}

// ContextWithStream attaches the streaming pipeline to ctx. emitEvent
// reads it and persists / publishes / forwards to the legacy channel
// in one place.
func ContextWithStream(ctx context.Context, channel chan<- ChatEvent, sink EventSink, hub *EventHub, userID, agentID, sessionKey string) context.Context {
	return context.WithValue(ctx, streamCtxKey{}, &streamCtx{
		channel:    channel,
		sink:       sink,
		hub:        hub,
		userID:     userID,
		agentID:    agentID,
		sessionKey: sessionKey,
	})
}

func streamFromContext(ctx context.Context) *streamCtx {
	s, _ := ctx.Value(streamCtxKey{}).(*streamCtx)
	return s
}
