package store

import (
	"context"
	"testing"
)

// TestDeleteLastUserMessages pins the count-based archive rollback: the
// caller (Session.RollbackLastUser) drops up to N trailing user rows from the
// working set — the unanswered pending turn plus, when a crash-heal parked
// one directly behind it, its turn-abort marker (also role=user) — and the
// archive must drop exactly the same rows so history-reload stays consistent.
func TestDeleteLastUserMessages(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	agent, key := "agent-A", "s-del-last-user"
	append := func(role, content string) {
		t.Helper()
		if err := db.AppendSessionMessage(ctx, agent, key, SessionMessage{Role: role, Content: content}); err != nil {
			t.Fatalf("append %s: %v", role, err)
		}
	}
	list := func() []SessionMessage {
		t.Helper()
		msgs, err := db.ListSessionMessages(ctx, agent, key)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		return msgs
	}

	// assistant tail → no-op, whatever the limit.
	append("user", "q1")
	append("assistant", "a1")
	if n, err := db.DeleteLastUserMessages(ctx, agent, key, 2); err != nil || n != 0 {
		t.Fatalf("assistant tail: deleted %d, err %v — want 0/no-op", n, err)
	}
	if len(list()) != 2 {
		t.Fatal("assistant tail: rows modified")
	}

	// user turn + abort marker (both user-role) → limit 2 removes both.
	append("user", "q2 (unanswered)")
	append("user", "[turn aborted]")
	if n, err := db.DeleteLastUserMessages(ctx, agent, key, 2); err != nil || n != 2 {
		t.Fatalf("marker+user tail: deleted %d, err %v — want 2", n, err)
	}
	got := list()
	if len(got) != 2 || got[0].Content != "q1" || got[1].Content != "a1" {
		t.Fatalf("remaining rows wrong: %+v", got)
	}

	// Limit larger than the trailing user run stops at the first non-user row.
	append("user", "q3 (unanswered)")
	if n, err := db.DeleteLastUserMessages(ctx, agent, key, 5); err != nil || n != 1 {
		t.Fatalf("oversized limit: deleted %d, err %v — want 1", n, err)
	}
	if got := list(); len(got) != 2 {
		t.Fatalf("rows after oversized delete: %d, want 2", len(got))
	}

	// Empty session → 0, no error.
	if n, err := db.DeleteLastUserMessages(ctx, agent, "s-nonexistent", 2); err != nil || n != 0 {
		t.Fatalf("empty session: deleted %d, err %v — want 0", n, err)
	}
}
