package store

import (
	"context"
	"testing"
	"time"
)

// TestIdleSessionsSkipHeartbeat guards the summary sweep's heartbeat
// exclusion: background cron sessions (channel=heartbeat) are task-run
// artifacts that repeat the same template daily — they must never enter
// the idle-summary candidate list, while real-channel sessions do.
// cutoff is caller-supplied, so the test passes a future cutoff instead
// of fabricating aged rows.
func TestIdleSessionsSkipHeartbeat(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	const agentID = "agt-idle"
	save := func(key, channel string) {
		rec := &SessionRecord{
			Channel:  channel,
			Messages: []SessionMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "yo"}},
		}
		if err := db.SaveSession(ctx, agentID, key, rec); err != nil {
			t.Fatalf("save %s: %v", key, err)
		}
	}
	save("web_s-real", "web")
	save("heartbeat_cron_x", "heartbeat")

	// cutoff in the future makes both rows "idle"; minMessages=1 lets the
	// 2-message rows qualify on the size gate.
	sessions, err := db.ListIdleSessions(ctx, agentID, time.Now().Add(time.Hour), 1)
	if err != nil {
		t.Fatalf("list idle: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("idle candidates = %d (%v); want only the web session", len(sessions), sessions)
	}
	if sessions[0].SessionKey != "web_s-real" {
		t.Fatalf("candidate = %s; want web_s-real", sessions[0].SessionKey)
	}
}
