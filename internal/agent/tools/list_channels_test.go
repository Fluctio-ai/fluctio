package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// newTestStore opens an isolated sqlite file under a per-test temp dir.
// We avoid the "file::memory:?cache=shared" DSN that cron_test uses
// because every test sharing that DSN in the same process would share one
// in-memory database and cross-contaminate.
func newTestStore(t *testing.T) *store.DBStore {
	t.Helper()
	db, err := store.NewDBStore("sqlite", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// saveTestSession inserts a session whose message_count is derived from
// msgs (the store sets message_count = len(messages) on save). updated_at
// is set by the store to now().
func saveTestSession(t *testing.T, db *store.DBStore, agentID, key, channel, accountID, chatID string, msgs int) {
	t.Helper()
	s := &store.SessionRecord{
		Channel:   channel,
		AccountID: accountID,
		ChatID:    chatID,
		UpdatedAt: time.Now(),
	}
	for i := 0; i < msgs; i++ {
		s.Messages = append(s.Messages, store.SessionMessage{Role: "user", Content: "x", Timestamp: time.Now()})
	}
	if err := db.SaveSession(context.Background(), agentID, key, s); err != nil {
		t.Fatalf("SaveSession %s: %v", key, err)
	}
}

func TestListChannels(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	// Two channels on agent-1 + one on agent-2 that must NOT leak.
	for _, ch := range []store.ChannelRecord{
		{ID: "ch-wx", UserID: "u1", AgentID: "agent-1", Type: "wechat", AccountID: "bot-wx", Enabled: true, Data: map[string]interface{}{}},
		{ID: "ch-tg", UserID: "u1", AgentID: "agent-1", Type: "telegram", AccountID: "bot-tg", Enabled: true, Data: map[string]interface{}{}},
		{ID: "ch-other", UserID: "u2", AgentID: "agent-2", Type: "wechat", AccountID: "bot-other", Enabled: true, Data: map[string]interface{}{}},
	} {
		if err := db.SaveChannel(ctx, &ch); err != nil {
			t.Fatalf("save channel %s: %v", ch.ID, err)
		}
	}

	// wx-chat-A spans two session keys (a /new in the same chat) → must
	// merge into one entry with summed message_count. wx-chat-B is a
	// separate chat. s-other belongs to agent-2 and must not appear.
	saveTestSession(t, db, "agent-1", "s1", "wechat", "bot-wx", "wx-chat-A", 5)
	saveTestSession(t, db, "agent-1", "s1b", "wechat", "bot-wx", "wx-chat-A", 3)
	saveTestSession(t, db, "agent-1", "s2", "wechat", "bot-wx", "wx-chat-B", 2)
	saveTestSession(t, db, "agent-1", "s3", "telegram", "bot-tg", "tg-123", 8)
	saveTestSession(t, db, "agent-2", "s-other", "wechat", "bot-other", "wx-x", 1)

	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterListChannelsTool(r, db, "agent-1")

	out, err := r.Execute(ctx, "list_channels", "{}")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var res listChannelsResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal (%s): %v", out, err)
	}
	if len(res.Channels) != 2 {
		t.Fatalf("got %d channels, want 2: %s", len(res.Channels), out)
	}
	// Sorted by channel type → telegram, then wechat.
	if res.Channels[0].Channel != "telegram" || res.Channels[1].Channel != "wechat" {
		t.Fatalf("order=%s,%s; want telegram,wechat", res.Channels[0].Channel, res.Channels[1].Channel)
	}
	wx := res.Channels[1]
	if len(wx.Chats) != 2 {
		t.Fatalf("wechat chats=%d, want 2 (A merged, B): %+v", len(wx.Chats), wx.Chats)
	}
	var merged *channelChat
	for i := range wx.Chats {
		if wx.Chats[i].ChatID == "wx-chat-A" {
			merged = &wx.Chats[i]
		}
	}
	if merged == nil {
		t.Fatalf("wx-chat-A missing: %+v", wx.Chats)
	}
	if merged.MessageCount != 8 { // 5 + 3
		t.Fatalf("merged messageCount=%d, want 8", merged.MessageCount)
	}

	if strings.Contains(out, "wx-x") || strings.Contains(out, "bot-other") {
		t.Fatalf("result leaks another agent's data: %s", out)
	}
}

func TestListChannelsEmpty(t *testing.T) {
	db := newTestStore(t)
	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterListChannelsTool(r, db, "agent-x")
	out, err := r.Execute(context.Background(), "list_channels", "{}")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "No IM channels") {
		t.Fatalf("empty result = %q", out)
	}
}

func TestValidateChannelTarget(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	saveTestSession(t, db, "agent-1", "s1", "wechat", "bot-wx", "wx-chat-A", 1)

	// Valid: matches a session that exists for this agent.
	if err := validateChannelTarget(ctx, db, "agent-1", "wechat", "bot-wx", "wx-chat-A"); err != nil {
		t.Fatalf("expected accept, got: %v", err)
	}
	// Reject: chatId this agent never talked to.
	if err := validateChannelTarget(ctx, db, "agent-1", "wechat", "bot-wx", "foreign"); err == nil {
		t.Fatal("expected reject for foreign chatId")
	}
	// Reject: known chatId but wrong channel.
	if err := validateChannelTarget(ctx, db, "agent-1", "telegram", "bot-tg", "wx-chat-A"); err == nil {
		t.Fatal("expected reject for wrong channel")
	}
	// Reject: a chatId that exists only under another agent.
	saveTestSession(t, db, "agent-2", "s2", "wechat", "bot-wx", "shared-chat", 1)
	if err := validateChannelTarget(ctx, db, "agent-1", "wechat", "bot-wx", "shared-chat"); err == nil {
		t.Fatal("expected reject for other-agent chat")
	}
}

// TestCreateCronJobCrossChannelTarget exercises the create_cron_job path
// end-to-end: while the current chat is telegram, scheduling a push to a
// whitelisted wechat chat must store the wechat triple on the job, and a
// non-whitelisted chat must be rejected before saving.
func TestCreateCronJobCrossChannelTarget(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	saveTestSession(t, db, "agent-1", "s-wx", "wechat", "bot-wx", "wx-chat-A", 1)

	r := NewRegistry(t.TempDir(), t.TempDir())
	r.SetOwnerUserID("user-1")
	r.SetMessageContext("telegram", "bot-tg", "tg-current") // current chat is telegram
	RegisterCronTools(r, db, "user-1", "agent-1")

	// Reject: chatId not on this agent.
	bad, _ := json.Marshal(createCronJobArgs{
		Name: "bad", Type: "once",
		Schedule: time.Now().Add(time.Hour).Format(time.RFC3339),
		Message: "hi", Channel: "wechat", AccountID: "bot-wx", ChatID: "foreign",
	})
	if _, err := r.Execute(ctx, "create_cron_job", string(bad)); err == nil {
		t.Fatal("expected reject for non-whitelisted chatId")
	}

	// Accept: known wechat chat → job stores the wechat triple, not the current telegram one.
	good, _ := json.Marshal(createCronJobArgs{
		Name: "wx-push", Type: "once",
		Schedule: time.Now().Add(time.Hour).Format(time.RFC3339),
		Message: "hi", Channel: "wechat", AccountID: "bot-wx", ChatID: "wx-chat-A",
	})
	if _, err := r.Execute(ctx, "create_cron_job", string(good)); err != nil {
		t.Fatalf("create with valid target: %v", err)
	}
	jobs, err := db.ListCronJobsByAgent(ctx, "agent-1")
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	j := jobs[0]
	if j.Channel != "wechat" || j.AccountID != "bot-wx" || j.ChatID != "wx-chat-A" {
		t.Fatalf("job target=(%s,%s,%s); want wechat,bot-wx,wx-chat-A", j.Channel, j.AccountID, j.ChatID)
	}
}
