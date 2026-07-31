package store

import (
	"context"
	"encoding/json"
	"testing"
)

// TestMigrateVectorConfigSplit seeds "memory" setting rows (one carrying
// vector fields, one without) and checks migrateVectorConfigSplit:
//   - creates a "vectorization" row carrying only the vector fields,
//   - leaves non-vector fields (summaryModel, autoPersist) on memory,
//   - skips memory rows that have no vector config,
//   - is idempotent (a second run creates no duplicate),
//   - never mutates the source memory row.
func TestMigrateVectorConfigSplit(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	const agentID = "agt_vec"
	memData := `{"embedding":{"enabled":true,"apiBase":"https://x","model":"bge-m3"},"reranker":{"enabled":true,"model":"jina"},"kbEmbedding":true,"wikiEmbedding":true,"summaryModel":"gpt","autoPersist":{"enabled":true}}`
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO configs (id, kind, scope, scope_id, name, enabled, data, created_at, updated_at)
		 VALUES ('cfg_mem', 'setting', 'agent', ?, 'memory', 1, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		agentID, memData); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	// A memory row with no vector fields must be left alone.
	plainData := `{"summaryModel":"gpt","autoPersist":{"enabled":true}}`
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO configs (id, kind, scope, scope_id, name, enabled, data, created_at, updated_at)
		 VALUES ('cfg_plain', 'setting', 'agent', 'agt_plain', 'memory', 1, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		plainData); err != nil {
		t.Fatalf("seed plain: %v", err)
	}

	if err := db.migrateVectorConfigSplit(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// vectorization row created for the vectorized agent only.
	var vecData string
	if err := db.db.QueryRowContext(ctx,
		`SELECT data FROM configs WHERE kind='setting' AND name='vectorization' AND scope_id=?`,
		agentID).Scan(&vecData); err != nil {
		t.Fatalf("vectorization row missing: %v", err)
	}
	var vec map[string]any
	if err := json.Unmarshal([]byte(vecData), &vec); err != nil {
		t.Fatalf("unmarshal vectorization: %v", err)
	}
	if emb, ok := vec["embedding"].(map[string]any); !ok || emb["apiBase"] != "https://x" {
		t.Errorf("vectorization.embedding = %+v; want apiBase https://x", vec["embedding"])
	}
	if vec["kbEmbedding"] != true || vec["wikiEmbedding"] != true {
		t.Errorf("vectorization kb/wiki flags = %v/%v; want true/true", vec["kbEmbedding"], vec["wikiEmbedding"])
	}
	// Non-vector fields must NOT leak into the vectorization row.
	if _, ok := vec["summaryModel"]; ok {
		t.Errorf("summaryModel leaked into vectorization row")
	}
	if _, ok := vec["autoPersist"]; ok {
		t.Errorf("autoPersist leaked into vectorization row")
	}

	// The plain agent (no vector fields) must not get a vectorization row.
	var plainVecCount int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM configs WHERE kind='setting' AND name='vectorization' AND scope_id='agt_plain'`).Scan(&plainVecCount); err != nil {
		t.Fatalf("count plain vectorization: %v", err)
	}
	if plainVecCount != 0 {
		t.Errorf("plain agent got a vectorization row: %d", plainVecCount)
	}

	// The memory row is untouched (still carries summaryModel + autoPersist).
	var memAfter string
	if err := db.db.QueryRowContext(ctx,
		`SELECT data FROM configs WHERE id='cfg_mem'`).Scan(&memAfter); err != nil {
		t.Fatalf("read memory after: %v", err)
	}
	if memAfter != memData {
		t.Errorf("memory row mutated:\n got %s\n want %s", memAfter, memData)
	}

	// Idempotent: running again creates no duplicate.
	if err := db.migrateVectorConfigSplit(ctx); err != nil {
		t.Fatalf("migrate again: %v", err)
	}
	var vecCount int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM configs WHERE kind='setting' AND name='vectorization' AND scope_id=?`,
		agentID).Scan(&vecCount); err != nil {
		t.Fatalf("count vectorization: %v", err)
	}
	if vecCount != 1 {
		t.Errorf("vectorization rows = %d; want 1 (idempotent)", vecCount)
	}
}
