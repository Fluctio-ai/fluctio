package setup

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/channels"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/users"
)

// notifyTestServer builds a server over a real (temp) DBStore plus a real
// WebChannel, which is everything the IM→app mirror touches. Returns the
// owner's user id: the queue is user-addressed, and LookupSessionOwner
// (deployment-global under the single-user flatten) resolves to the
// super_admin row — without one every push silently no-ops.
func notifyTestServer(t *testing.T) (*Server, *store.DBStore, *channels.WebChannel, context.Context, string) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fluctio.db")
	st, err := store.NewDBStore("sqlite", "file:"+dbPath+"?cache=shared")
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	accts, err := users.NewAccounts(st)
	if err != nil {
		t.Fatalf("NewAccounts: %v", err)
	}
	owner, err := accts.Create(ctx, users.CreateInput{
		Username: "owner", Email: "owner@example.test", Password: "password",
		Role: users.RoleSuperAdmin,
	})
	if err != nil {
		t.Fatalf("Create owner: %v", err)
	}
	if err := st.SaveAgent(ctx, &store.AgentRecord{ID: "a1", Name: "Tester"}); err != nil {
		t.Fatalf("SaveAgent: %v", err)
	}

	wc := channels.NewWebChannel()
	s := NewServer(0)
	s.SetStore(st)
	s.SetWebChannel(wc)

	// The throttle is package-global and keyed per (agent, session); a
	// fresh map keeps each test's enqueue independent of its neighbours.
	chatReplyThrottle.Lock()
	chatReplyThrottle.last = map[string]time.Time{}
	chatReplyThrottle.Unlock()
	return s, st, wc, ctx, owner.ID
}

func imTestSession(t *testing.T, st *store.DBStore, ctx context.Context) string {
	t.Helper()
	if err := st.SaveSession(ctx, "a1", "sess-tg-1", &store.SessionRecord{
		Channel: "telegram", AccountID: "bot1", ChatID: "12345",
	}); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	return "sess-tg-1"
}

func imTestMessage() bus.OutboundMessage {
	return bus.OutboundMessage{
		Channel: "telegram", AccountID: "bot1", ChatID: "12345",
		AgentID: "a1", Text: "迁移方案已按三个阶段发给你了",
	}
}

func pendingNotifications(t *testing.T, st *store.DBStore, ctx context.Context, userID string) []store.NotificationRecord {
	t.Helper()
	rows, err := st.ListNotifications(ctx, userID, 0, 50)
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	return rows
}

// TestIMPushMirrorsReplyToQueue is the core of the IM→app mirror: a reply
// on a chat surface the phone is not part of still reaches the queue, with
// the resolved session key as its deep-link target.
func TestIMPushMirrorsReplyToQueue(t *testing.T) {
	s, st, _, ctx, uid := notifyTestServer(t)
	sessionKey := imTestSession(t, st, ctx)

	s.handleIMPushOutbound(imTestMessage())

	rows := pendingNotifications(t, st, ctx, uid)
	if len(rows) != 1 {
		t.Fatalf("queued notifications = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.Kind != "im_reply" {
		t.Fatalf("kind = %q, want im_reply", got.Kind)
	}
	if want := "/agents/a1/chat/" + sessionKey; got.DeepLink != want {
		t.Fatalf("deep link = %q, want %q", got.DeepLink, want)
	}
	if got.Title != "Tester" {
		t.Fatalf("title = %q, want the agent name", got.Title)
	}
	if got.Body == "" {
		t.Fatal("body is empty")
	}
}

func TestIMPushSkippedWhenWatching(t *testing.T) {
	s, st, wc, ctx, uid := notifyTestServer(t)
	sessionKey := imTestSession(t, st, ctx)

	// A live subscriber on the same session means the user is reading it
	// in the dashboard/app — no ring.
	_, cancel := wc.Subscribe("a1", sessionKey)
	defer cancel()

	s.handleIMPushOutbound(imTestMessage())

	if rows := pendingNotifications(t, st, ctx, uid); len(rows) != 0 {
		t.Fatalf("queued notifications = %d, want 0 while the session is being viewed", len(rows))
	}
}

func TestIMPushSkippedWhenKindDisabled(t *testing.T) {
	s, st, _, ctx, uid := notifyTestServer(t)
	imTestSession(t, st, ctx)

	agent, err := st.GetAgent(ctx, "a1")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	agent.Config = map[string]interface{}{
		"notifications": map[string]interface{}{"imReply": false},
	}
	if err := st.SaveAgent(ctx, agent); err != nil {
		t.Fatalf("SaveAgent: %v", err)
	}

	s.handleIMPushOutbound(imTestMessage())

	if rows := pendingNotifications(t, st, ctx, uid); len(rows) != 0 {
		t.Fatalf("queued notifications = %d, want 0 when imReply is off", len(rows))
	}
}

func TestIMPushIgnoresOtherChannels(t *testing.T) {
	s, st, _, ctx, uid := notifyTestServer(t)
	imTestSession(t, st, ctx)

	for _, channel := range []string{"web", "api", "heartbeat", "some-plugin"} {
		msg := imTestMessage()
		msg.Channel = channel
		s.handleIMPushOutbound(msg)
	}
	if rows := pendingNotifications(t, st, ctx, uid); len(rows) != 0 {
		t.Fatalf("queued notifications = %d, want 0 for non-IM channels", len(rows))
	}
}
