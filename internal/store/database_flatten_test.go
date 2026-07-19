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

// (TestFlattenUserDataMigration_sessionMessages used to seed two rows at
// the same (agent, session_key, seq) under different users and assert the
// newest survived. After task 1.3b dropped session_messages.user_id the
// PK became (agent_id, session_key, seq), so duplicates can no longer
// exist and that branch of the flatten is a permanent no-op. With sessions
// + session_messages both flattened, the only remaining dup-collapse case
// is configs — covered by migrateFlattenUserData's configs branch when the
// column exists, exercised on legacy-install upgrade paths.)

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
		`INSERT INTO agents (id, name, config) VALUES (?, 'flat', '{}')`,
		agentID); err != nil {
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
