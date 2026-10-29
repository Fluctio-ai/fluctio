package setup

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// TestEnqueueChatReply covers the queue leg of handleWebPushOutbound: the
// row shape (kind/deep link/first-line body) and the per-session 60s
// throttle that coalesces a turn's bubble burst. APNs needs real keys —
// that leg stays e2e territory.
func TestEnqueueChatReply(t *testing.T) {
	dbs, err := store.NewDBStore("sqlite", "file::memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer dbs.Close()
	if err := dbs.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	if err := dbs.CreateUser(ctx, &store.UserRecord{ID: "u1", Username: "owner", Email: "o@x", Role: "super_admin"}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := dbs.SaveAgent(ctx, &store.AgentRecord{ID: "agt_1", Name: "bot"}); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	s := &Server{dataStore: dbs}
	// Global throttle map: reset so a prior test's timing can't leak in,
	// and clear again for the next one.
	chatReplyThrottle.Lock()
	chatReplyThrottle.last = map[string]time.Time{}
	chatReplyThrottle.Unlock()
	t.Cleanup(func() {
		chatReplyThrottle.Lock()
		chatReplyThrottle.last = map[string]time.Time{}
		chatReplyThrottle.Unlock()
	})

	msg := bus.OutboundMessage{Channel: "web", AgentID: "agt_1", ChatID: "web-sess", Text: "第一行回复\n第二行更多内容"}
	s.handleWebPushOutbound(msg)
	// Same session, second bubble within the window: swallowed.
	s.handleWebPushOutbound(bus.OutboundMessage{Channel: "web", AgentID: "agt_1", ChatID: "web-sess", Text: "第二条气泡"})
	// A different session rings independently.
	s.handleWebPushOutbound(bus.OutboundMessage{Channel: "web", AgentID: "agt_1", ChatID: "other-sess", Text: "另一个会话"})

	rows, err := dbs.ListNotifications(ctx, "u1", 0, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (throttle merged the burst), got %d: %+v", len(rows), rows)
	}
	first := rows[0]
	if first.Kind != "chat_reply" || first.Title != "bot" {
		t.Fatalf("row kind/title wrong: %+v", first)
	}
	if first.Body != "第一行回复" {
		t.Fatalf("body must be the first line, got %q", first.Body)
	}
	if first.DeepLink != "/agents/agt_1/chat/web-sess" {
		t.Fatalf("deep link wrong: %q", first.DeepLink)
	}
	if !strings.HasSuffix(rows[1].DeepLink, "/chat/other-sess") {
		t.Fatalf("second row deep link wrong: %q", rows[1].DeepLink)
	}
}
