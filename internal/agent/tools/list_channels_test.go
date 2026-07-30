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
// is set by the store to now(), so call order controls recency.
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

	// Two bound+enabled channels on agent-1 + one on agent-2 that must NOT
	// leak (different agent).
	for _, ch := range []store.ChannelRecord{
		{ID: "ch-wx", UserID: "u1", AgentID: "agent-1", Type: "wechat", AccountID: "bot-wx", Enabled: true, Data: map[string]interface{}{}},
		{ID: "ch-tg", UserID: "u1", AgentID: "agent-1", Type: "telegram", AccountID: "bot-tg", Enabled: true, Data: map[string]interface{}{}},
		{ID: "ch-other", UserID: "u2", AgentID: "agent-2", Type: "wechat", AccountID: "bot-other", Enabled: true, Data: map[string]interface{}{}},
	} {
		if err := db.SaveChannel(ctx, &ch); err != nil {
			t.Fatalf("save channel %s: %v", ch.ID, err)
		}
	}

	// wx-chat-A has two sessions (a /new in the same chat) → must collapse
	// to ONE row, reflecting the newest session. wx-chat-B is a separate
	// conversation. Save order controls last-active recency. s-other is a
	// different agent's and must not appear.
	saveTestSession(t, db, "agent-1", "s2", "wechat", "bot-wx", "wx-chat-B", 2)
	saveTestSession(t, db, "agent-1", "s1", "wechat", "bot-wx", "wx-chat-A", 5)
	saveTestSession(t, db, "agent-1", "s1b", "wechat", "bot-wx", "wx-chat-A", 7) // newest for wx-chat-A
	saveTestSession(t, db, "agent-1", "s3", "telegram", "bot-tg", "tg-123", 4)
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
	// Expect 3 rows (wx-chat-A collapsed): telegram/tg-123, wechat/wx-chat-A (newest, count 7), wechat/wx-chat-B.
	if len(res.Deliverable) != 3 {
		t.Fatalf("got %d deliverable, want 3: %s", len(res.Deliverable), out)
	}
	d := res.Deliverable
	if d[0].Channel != "telegram" || d[0].AccountID != "bot-tg" || d[0].ChatID != "tg-123" || d[0].MessageCount != 4 {
		t.Fatalf("[0]=%+v, want telegram/bot-tg/tg-123 count 4", d[0])
	}
	if d[1].Channel != "wechat" || d[1].AccountID != "bot-wx" || d[1].ChatID != "wx-chat-A" || d[1].MessageCount != 7 {
		t.Fatalf("[1]=%+v, want wechat/bot-wx/wx-chat-A count 7 (newest session)", d[1])
	}
	if d[2].Channel != "wechat" || d[2].ChatID != "wx-chat-B" || d[2].MessageCount != 2 {
		t.Fatalf("[2]=%+v, want wechat/bot-wx/wx-chat-B count 2", d[2])
	}

	if strings.Contains(out, "wx-x") || strings.Contains(out, "bot-other") {
		t.Fatalf("result leaks another agent's data: %s", out)
	}
}

// TestListChannelsPicksBoundAccount: when the same chat has sessions under
// both a bound account and an unbound one (e.g. WeChat rebound to a new
// bot), the row must use the bound account even if the unbound session is
// newer — delivery can only route through the registered adapter.
func TestListChannelsPicksBoundAccount(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	if err := db.SaveChannel(ctx, &store.ChannelRecord{
		ID: "c-wx", UserID: "u1", AgentID: "agent-1", Type: "wechat", AccountID: "bot-wx", Enabled: true, Data: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("save channel: %v", err)
	}
	// Same chat, two accounts: bound bot-wx (older) + unbound bot-old (newer).
	saveTestSession(t, db, "agent-1", "s1", "wechat", "bot-wx", "chat-X", 5)
	saveTestSession(t, db, "agent-1", "s2", "wechat", "bot-old", "chat-X", 10) // newer but unbound

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
	if len(res.Deliverable) != 1 {
		t.Fatalf("got %d rows, want 1: %s", len(res.Deliverable), out)
	}
	d := res.Deliverable[0]
	if d.AccountID != "bot-wx" {
		t.Fatalf("accountId=%q, want bound bot-wx (not the newer unbound bot-old)", d.AccountID)
	}
	if d.MessageCount != 5 {
		t.Fatalf("messageCount=%d, want 5 from the bound session", d.MessageCount)
	}
	if strings.Contains(out, "bot-old") {
		t.Fatalf("unbound account leaked: %s", out)
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
	if !strings.Contains(out, "No deliverable targets") {
		t.Fatalf("empty result = %q", out)
	}
}

// TestListChannelsSkipsUnbound: a chat whose every session is under an
// unbound account has no registered adapter → must not appear.
func TestListChannelsSkipsUnbound(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	// wechat: session under bot-wx, but NO channel row → unbound.
	saveTestSession(t, db, "agent-1", "s-wx", "wechat", "bot-wx", "wx-chat", 3)
	// telegram: bound + a session → deliverable.
	if err := db.SaveChannel(ctx, &store.ChannelRecord{
		ID: "c-tg", UserID: "u1", AgentID: "agent-1", Type: "telegram", AccountID: "bot-tg", Enabled: true, Data: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("save tg channel: %v", err)
	}
	saveTestSession(t, db, "agent-1", "s-tg", "telegram", "bot-tg", "tg-1", 1)

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
	if len(res.Deliverable) != 1 {
		t.Fatalf("got %d deliverable, want 1 (only bound telegram): %s", len(res.Deliverable), out)
	}
	if res.Deliverable[0].Channel != "telegram" || res.Deliverable[0].ChatID != "tg-1" {
		t.Fatalf("deliverable=%+v, want telegram/tg-1", res.Deliverable[0])
	}
	if strings.Contains(out, "wx-chat") {
		t.Fatalf("unbound wechat chat_id leaked into result: %s", out)
	}
}

// TestListChannelsSkipsDisabled: a bound-but-disabled channel has no
// registered adapter, so its chats must be excluded too.
func TestListChannelsSkipsDisabled(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	if err := db.SaveChannel(ctx, &store.ChannelRecord{
		ID: "c-wx", UserID: "u1", AgentID: "agent-1", Type: "wechat", AccountID: "bot-wx", Enabled: false, Data: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("save channel: %v", err)
	}
	saveTestSession(t, db, "agent-1", "s1", "wechat", "bot-wx", "wx-chat", 2)

	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterListChannelsTool(r, db, "agent-1")
	out, err := r.Execute(ctx, "list_channels", "{}")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "No deliverable targets") {
		t.Fatalf("disabled-only agent should report no deliverable targets: %s", out)
	}
}

func TestResolveChannelTarget(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	if err := db.SaveChannel(ctx, &store.ChannelRecord{
		ID: "c-wx", UserID: "u1", AgentID: "agent-1", Type: "wechat", AccountID: "bot-wx", Enabled: true, Data: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("save channel: %v", err)
	}
	saveTestSession(t, db, "agent-1", "s1", "wechat", "bot-wx", "wx-chat-A", 1)

	// Valid + accountId auto-resolved to the bound bot (the bug fix: a
	// model that omits accountId must still get a fire-able job).
	acc, err := resolveChannelTarget(ctx, db, "agent-1", "wechat", "", "wx-chat-A")
	if err != nil || acc != "bot-wx" {
		t.Fatalf("expected bot-wx auto-resolved, got %q err=%v", acc, err)
	}
	// Explicit matching accountId resolves too.
	if acc, err := resolveChannelTarget(ctx, db, "agent-1", "wechat", "bot-wx", "wx-chat-A"); err != nil || acc != "bot-wx" {
		t.Fatalf("explicit accountId failed: %q %v", acc, err)
	}
	// Reject: foreign chatId.
	if _, err := resolveChannelTarget(ctx, db, "agent-1", "wechat", "", "foreign"); err == nil {
		t.Fatal("expected reject foreign chatId")
	}
	// Reject: wrong channel.
	if _, err := resolveChannelTarget(ctx, db, "agent-1", "telegram", "", "wx-chat-A"); err == nil {
		t.Fatal("expected reject wrong channel")
	}
	// Reject: chat exists only under an unbound account.
	saveTestSession(t, db, "agent-1", "s2", "wechat", "bot-old", "chat-X", 1)
	if _, err := resolveChannelTarget(ctx, db, "agent-1", "wechat", "", "chat-X"); err == nil {
		t.Fatal("expected reject chat with no bound adapter")
	}
	// Reject: chat exists only under another agent.
	saveTestSession(t, db, "agent-2", "s3", "wechat", "bot-wx", "shared", 1)
	if _, err := resolveChannelTarget(ctx, db, "agent-1", "wechat", "", "shared"); err == nil {
		t.Fatal("expected reject other-agent chat")
	}
}

// TestCreateCronJobCrossChannelTarget exercises the create_cron_job path
// end-to-end: while the current chat is telegram, scheduling a push to a
// whitelisted wechat chat must store the wechat triple on the job, and a
// non-whitelisted chat must be rejected before saving.
func TestCreateCronJobCrossChannelTarget(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	if err := db.SaveChannel(ctx, &store.ChannelRecord{
		ID: "c-wx", UserID: "u1", AgentID: "agent-1", Type: "wechat", AccountID: "bot-wx", Enabled: true, Data: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("save channel: %v", err)
	}
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
