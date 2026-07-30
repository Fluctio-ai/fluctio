package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

func TestCreateCronJobPersistsMessageAccountID(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Bound telegram channel + a session so resolveChannelTarget accepts
	// the push target (new model: no "default to current chat" — a push
	// target must be explicit and whitelisted).
	ctx := context.Background()
	if err := db.SaveChannel(ctx, &store.ChannelRecord{
		ID: "c-tg", UserID: "user-1", AgentID: "agent-1", Type: "telegram", AccountID: "dclaw_official_bot", Enabled: true, Data: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("save channel: %v", err)
	}
	sess := &store.SessionRecord{Channel: "telegram", AccountID: "dclaw_official_bot", ChatID: "8169894742", UpdatedAt: time.Now(), Messages: []store.SessionMessage{{Role: "user", Content: "hi", Timestamp: time.Now()}}}
	if err := db.SaveSession(ctx, "agent-1", "s1", sess); err != nil {
		t.Fatalf("save session: %v", err)
	}

	r := NewRegistry(t.TempDir(), t.TempDir())
	r.SetOwnerUserID("user-1")
	r.SetOwnerUserID("user-1")
	r.SetMessageContext("telegram", "dclaw_official_bot", "8169894742")
	RegisterCronTools(r, db, "user-1", "agent-1")

	args, err := json.Marshal(createCronJobArgs{
		Name:      "telegram reminder",
		Type:      "once",
		Schedule:  time.Now().Add(time.Hour).Format(time.RFC3339),
		Message:   "提醒我",
		Channel:   "telegram",
		AccountID: "dclaw_official_bot",
		ChatID:    "8169894742",
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	if _, err := r.Execute(context.Background(), "create_cron_job", string(args)); err != nil {
		t.Fatalf("create cron job: %v", err)
	}

	jobs, err := db.ListCronJobsByAgent(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("list cron jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d cron jobs, want 1", len(jobs))
	}
	if got := jobs[0].AccountID; got != "dclaw_official_bot" {
		t.Fatalf("AccountID = %q, want dclaw_official_bot", got)
	}
	if got := jobs[0].Channel; got != "telegram" {
		t.Fatalf("Channel = %q, want telegram", got)
	}
	if got := jobs[0].ChatID; got != "8169894742" {
		t.Fatalf("ChatID = %q, want 8169894742", got)
	}
}
