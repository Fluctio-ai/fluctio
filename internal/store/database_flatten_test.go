package store

import (
	"context"
	"errors"
	"testing"
)

// TestFlattenUserDataMigration verifies migrateFlattenUserData collapses
// per-user rows onto a single row per natural key, keeping the newest
// content per group. It seeds duplicate (agent, session_key) rows across
// two users, runs the full Migrate (which now includes the flatten step),
// and asserts only the newest row per group survives. Idempotency is
// checked by running Migrate a second time.
//
// Note: agent_files used to be covered here too, but task 1.2 dropped its
// user_id column (PK is now (agent_id, filename), so duplicates can't
// exist) and the agent_files branch of the flatten is therefore a
// permanent no-op. The sessions / session_messages cases below still
// exercise the same flattenDuplicateRows code path end-to-end.
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

// TestAgentFilesFlatSchema verifies task 1.2's flattened agent_files
// schema: PK is (agent_id, filename), SaveAgentFile upserts on conflict,
// and GetAgentFile / ListAgentFiles / DeleteAgentFile work without a
// user_id. Also asserts the user_id column is actually gone post-migrate.
func TestAgentFilesFlatSchema(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	const agentID = "agt_flat"
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO agents (id, user_id, name, config) VALUES (?, ?, 'flat', '{}')`,
		agentID, "u_owner"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// Save twice on the same filename — upsert, not duplicate.
	if err := db.SaveAgentFile(ctx, agentID, "SOUL.md", []byte("v1")); err != nil {
		t.Fatalf("SaveAgentFile v1: %v", err)
	}
	if err := db.SaveAgentFile(ctx, agentID, "SOUL.md", []byte("v2")); err != nil {
		t.Fatalf("SaveAgentFile v2: %v", err)
	}
	if err := db.SaveAgentFile(ctx, agentID, "IDENTITY.md", []byte("id")); err != nil {
		t.Fatalf("SaveAgentFile IDENTITY: %v", err)
	}

	got, err := db.GetAgentFile(ctx, agentID, "SOUL.md")
	if err != nil {
		t.Fatalf("GetAgentFile: %v", err)
	}
	if string(got) != "v2" {
		t.Errorf("SOUL.md = %q; want v2 (upsert overwrote v1)", string(got))
	}

	files, err := db.ListAgentFiles(ctx, agentID)
	if err != nil {
		t.Fatalf("ListAgentFiles: %v", err)
	}
	want := []string{"IDENTITY.md", "SOUL.md"}
	if len(files) != len(want) {
		t.Fatalf("ListAgentFiles = %v; want %v", files, want)
	}
	for i, w := range want {
		if files[i] != w {
			t.Errorf("ListAgentFiles[%d] = %q; want %q", i, files[i], w)
		}
	}

	if err := db.DeleteAgentFile(ctx, agentID, "SOUL.md"); err != nil {
		t.Fatalf("DeleteAgentFile: %v", err)
	}
	if _, err := db.GetAgentFile(ctx, agentID, "SOUL.md"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetAgentFile after delete err = %v; want ErrNotFound", err)
	}

	// user_id column must be gone after migrateAgentFilesDropUserID.
	hasUID, err := db.tableHasColumn(ctx, "agent_files", "user_id")
	if err != nil {
		t.Fatalf("check user_id column: %v", err)
	}
	if hasUID {
		t.Error("agent_files.user_id still exists; migrateAgentFilesDropUserID should have removed it")
	}
}
