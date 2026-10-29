package store

import (
	"context"
	"testing"
)

// TestSessionKeyForChat guards the IM→session translation the mobile
// notification mirror depends on: outbound bus messages carry only the
// platform chat id, and both the suppression check ("is anyone viewing
// this session") and the deep link need the session key.
func TestSessionKeyForChat(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	if err := db.SaveSession(ctx, "a1", "sess-tg-1", &SessionRecord{
		Channel: "telegram", AccountID: "bot1", ChatID: "12345",
	}); err != nil {
		t.Fatalf("save session: %v", err)
	}

	got, err := db.SessionKeyForChat(ctx, "a1", "telegram", "bot1", "12345")
	if err != nil {
		t.Fatalf("SessionKeyForChat: %v", err)
	}
	if got != "sess-tg-1" {
		t.Fatalf("SessionKeyForChat = %q, want sess-tg-1", got)
	}

	// No row is the normal case for a chat that never got a session —
	// empty string without an error, so the caller just skips delivery.
	for _, tc := range []struct{ name, agent, channel, account, chat string }{
		{"unknown chat", "a1", "telegram", "bot1", "nope"},
		{"wrong account", "a1", "telegram", "bot2", "12345"},
		{"wrong agent", "a2", "telegram", "bot1", "12345"},
		{"wrong channel", "a1", "slack", "bot1", "12345"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.SessionKeyForChat(ctx, tc.agent, tc.channel, tc.account, tc.chat)
			if err != nil {
				t.Fatalf("SessionKeyForChat: %v", err)
			}
			if got != "" {
				t.Fatalf("SessionKeyForChat = %q, want empty", got)
			}
		})
	}
}
