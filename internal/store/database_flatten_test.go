package store

import (
	"context"
	"errors"
	"testing"
)

// (TestFlattenUserDataMigration used to seed duplicate (agent, session_key)
// rows across two users and assert the newest survived. After task 1.3a
// dropped sessions.user_id the sessions PK became (agent_id, session_key),
// so duplicates can no longer exist and that branch of the flatten is a
// permanent no-op — the case can't be constructed. flattenDuplicateRows
// itself is still covered end-to-end by
// TestFlattenUserDataMigration_sessionMessages below, and TestAgentFilesFlatSchema
// asserts the agent_files schema post-flatten.)

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
