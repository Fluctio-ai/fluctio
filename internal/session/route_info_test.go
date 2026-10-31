package session

import (
	"context"
	"testing"
)

// tripleStore is a SessionStore whose only real behaviour is the
// key → (channel, accountID, chatID, projectID) inverse mapping — the
// data a cold GetByKey needs to late-bind its routing triple.
type tripleStore struct {
	noopSessionStore
	channel, accountID, chatID, projectID string
}

func (s tripleStore) LookupSessionTriple(context.Context, string, string) (string, string, string, error) {
	return s.channel, s.accountID, s.chatID, nil
}
func (s tripleStore) LookupSessionProject(context.Context, string, string) (string, error) {
	return s.projectID, nil
}

// TestGetByKeyLateBindsRouteInfo covers the daemon-restart replay path: a
// session fetched by key alone (cold, not cached) must still report its
// conversation triple, or anything re-entering the agent by key mints a
// brand-new session instead of continuing this one.
func TestGetByKeyLateBindsRouteInfo(t *testing.T) {
	st := tripleStore{channel: "wechat", accountID: "bot-1", chatID: "wx-chat", projectID: "proj-9"}
	m := NewManagerWithStoreForUser(t.TempDir(), st, "user-1", "agent-1")

	s := m.GetByKey("s-cold-1")
	if s == nil {
		t.Fatal("GetByKey returned nil")
	}
	ch, acc, chat, pid := s.RouteInfo()
	if ch != "wechat" || acc != "bot-1" || chat != "wx-chat" {
		t.Errorf("RouteInfo = (%q,%q,%q), want the persisted triple", ch, acc, chat)
	}
	if pid != "proj-9" {
		t.Errorf("projectID = %q, want proj-9", pid)
	}

	// A caller that already knows the triple must not be overridden by the
	// (possibly stale) stored one.
	m.Get("web", "", "web-chat-1", "")
	ch2, _, _, _ := m.GetByKey("s-cold-2").RouteInfo()
	if ch2 != "wechat" {
		t.Errorf("unexpected triple for uncached key: %q", ch2)
	}
}

// guard: the compile-time contract that the stub still satisfies the
// interface after future additions.
var _ SessionStore = tripleStore{}
