package store

import (
	"context"
	"testing"
)

// TestFlattenUserDataMigration verifies migrateFlattenUserData collapses
// per-user rows onto a single row per natural key, keeping the newest
// content per group. It seeds duplicate (agent, filename) / (agent,
// session_key) rows across two users, runs the full Migrate (which now
// includes the flatten step), and asserts only the newest row per group
// survives. Idempotency is checked by running Migrate a second time.
func TestFlattenUserDataMigration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	const agentID = "agt_demo"
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO agents (id, user_id, name, config) VALUES (?, ?, 'demo', '{}')`,
		agentID, "u_owner"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// agent_files: USER.md exists under both users — the chatter row is
	// newer so it must win. IDENTITY.md / MEMORY.md are singletons under
	// the owner and must survive untouched (proves we don't over-delete).
	files := []struct {
		userID, filename, content, updatedAt string
	}{
		{"u_owner", "IDENTITY.md", "owner-identity", "2026-07-01 10:00:00"},
		{"u_owner", "USER.md", "owner-user", "2026-07-01 10:00:00"},
		{"u_chatter", "USER.md", "chatter-user-v2", "2026-07-02 10:00:00"}, // dup of USER.md, newest → wins
		{"u_owner", "MEMORY.md", "owner-memory", "2026-07-01 10:00:00"},
	}
	for _, f := range files {
		if _, err := db.db.ExecContext(ctx,
			`INSERT INTO agent_files (agent_id, user_id, filename, content, updated_at)
			 VALUES (?, ?, ?, ?, ?)`,
			agentID, f.userID, f.filename, f.content, f.updatedAt); err != nil {
			t.Fatalf("seed agent_file %+v: %v", f, err)
		}
	}

	// sessions: two rows share (agent, session_key='sess_X') across users
	// (defensive collapse), plus a unique sess_Y that must be left alone.
	sess := []struct {
		userID, sessionKey, updatedAt string
	}{
		{"u_owner", "sess_X", "2026-07-01 10:00:00"},
		{"u_chatter", "sess_X", "2026-07-02 10:00:00"}, // dup, newer → wins
		{"u_owner", "sess_Y", "2026-07-01 10:00:00"},   // unique → kept
	}
	for _, s := range sess {
		if _, err := db.db.ExecContext(ctx,
			`INSERT INTO sessions (user_id, agent_id, session_key, channel, account_id, chat_id, updated_at)
			 VALUES (?, ?, ?, '', '', '', ?)`,
			s.userID, agentID, s.sessionKey, s.updatedAt); err != nil {
			t.Fatalf("seed session %+v: %v", s, err)
		}
	}

	// Run the migration — flatten fires as part of Migrate.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// agent_files: 3 distinct filenames remain; USER.md keeps the v2 content.
	rows, err := db.db.QueryContext(ctx,
		`SELECT filename, content FROM agent_files WHERE agent_id = ? ORDER BY filename`, agentID)
	if err != nil {
		t.Fatalf("query agent_files: %v", err)
	}
	type afRow struct{ filename, content string }
	var got []afRow
	for rows.Next() {
		var r afRow
		if err := rows.Scan(&r.filename, &r.content); err != nil {
			t.Fatalf("scan agent_files: %v", err)
		}
		got = append(got, r)
	}
	rows.Close()
	wantAF := []afRow{
		{"IDENTITY.md", "owner-identity"},
		{"MEMORY.md", "owner-memory"},
		{"USER.md", "chatter-user-v2"},
	}
	if len(got) != len(wantAF) {
		t.Fatalf("agent_files rows = %+v; want %+v", got, wantAF)
	}
	for i, w := range wantAF {
		if got[i] != w {
			t.Errorf("agent_files[%d] = %+v; want %+v", i, got[i], w)
		}
	}

	// sessions: sess_X collapsed to 1 row, sess_Y kept → 2 rows total.
	var sessCount int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE agent_id = ?`, agentID).Scan(&sessCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessCount != 2 {
		t.Errorf("sessions count = %d; want 2 (sess_X collapsed, sess_Y kept)", sessCount)
	}
	// sess_X winner is the chatter row (newer).
	var sessXUser string
	if err := db.db.QueryRowContext(ctx,
		`SELECT user_id FROM sessions WHERE agent_id = ? AND session_key = 'sess_X'`, agentID).Scan(&sessXUser); err != nil {
		t.Fatalf("scan sess_X: %v", err)
	}
	if sessXUser != "u_chatter" {
		t.Errorf("sess_X winner user_id = %q; want u_chatter (newest)", sessXUser)
	}

	// Idempotency: a second Migrate must not delete anything further.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate (2nd): %v", err)
	}
	var afCount2 int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_files WHERE agent_id = ?`, agentID).Scan(&afCount2); err != nil {
		t.Fatalf("count agent_files after 2nd migrate: %v", err)
	}
	if afCount2 != 3 {
		t.Errorf("agent_files not idempotent: count after 2nd migrate = %d; want 3", afCount2)
	}
	var sessCount2 int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE agent_id = ?`, agentID).Scan(&sessCount2); err != nil {
		t.Fatalf("count sessions after 2nd migrate: %v", err)
	}
	if sessCount2 != 2 {
		t.Errorf("sessions not idempotent: count after 2nd migrate = %d; want 2", sessCount2)
	}
}

// TestFlattenUserDataMigration_sessionMessages covers the (agent,
// session_key, seq) collapse on session_messages — the seq dimension
// means two users contributing the same seq to the same session is the
// only way dups form here, and only the newer physical row should remain.
func TestFlattenUserDataMigration_sessionMessages(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	const agentID = "agt_demo"
	const sessKey = "sess_M"
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO agents (id, user_id, name, config) VALUES (?, ?, 'demo', '{}')`,
		agentID, "u_owner"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	// Two rows at the same (agent, session_key, seq=0) under different
	// users — the chatter row inserted second wins on rowid tiebreak.
	msgs := []struct {
		userID, role, content string
	}{
		{"u_owner", "user", "owner-msg"},
		{"u_chatter", "user", "chatter-msg"},
	}
	for _, m := range msgs {
		if _, err := db.db.ExecContext(ctx,
			`INSERT INTO session_messages (user_id, agent_id, session_key, seq, role, content)
			 VALUES (?, ?, ?, 0, ?, ?)`,
			m.userID, agentID, sessKey, m.role, m.content); err != nil {
			t.Fatalf("seed session_message %+v: %v", m, err)
		}
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var n int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_messages WHERE agent_id = ? AND session_key = ?`,
		agentID, sessKey).Scan(&n); err != nil {
		t.Fatalf("count session_messages: %v", err)
	}
	if n != 1 {
		t.Errorf("session_messages rows = %d; want 1 (collapsed)", n)
	}
	// Winner is the second-inserted (chatter) row — higher rowid.
	var content string
	if err := db.db.QueryRowContext(ctx,
		`SELECT content FROM session_messages WHERE agent_id = ? AND session_key = ?`,
		agentID, sessKey).Scan(&content); err != nil {
		t.Fatalf("scan session_messages: %v", err)
	}
	if content != "chatter-msg" {
		t.Errorf("session_messages winner content = %q; want chatter-msg (newest rowid)", content)
	}
}
