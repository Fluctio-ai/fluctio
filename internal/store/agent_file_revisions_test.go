package store

import (
	"context"
	"testing"
)

// Revision history: content-changing saves land a revision row with the
// caller's writer/reason; identical rewrites record nothing; CAS
// forwards its attribution; prune keeps the newest N per file.
func TestAgentFileRevisions(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer db.Close()

	// First write (no prior row) records revision 1 with attribution.
	if err := db.SaveAgentFile(ctx, "agt_rev", "MEMORY.md", []byte("v1"),
		WithFileWriter("system", "seed")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Unattributed write defaults to writer="system".
	if err := db.SaveAgentFile(ctx, "agt_rev", "MEMORY.md", []byte("v2")); err != nil {
		t.Fatalf("save v2: %v", err)
	}
	// Same-bytes rewrite records nothing.
	if err := db.SaveAgentFile(ctx, "agt_rev", "MEMORY.md", []byte("v2"),
		WithFileWriter("auto-persist", "noop")); err != nil {
		t.Fatalf("save noop: %v", err)
	}
	// CAS swap carries attribution through.
	if _, err := db.CompareAndSwapAgentFile(ctx, "agt_rev", "MEMORY.md", []byte("v2"), []byte("v3"),
		WithFileWriter("tidy", "pruned 2 stale entries")); err != nil {
		t.Fatalf("cas: %v", err)
	}
	// Another file's history is independent.
	if err := db.SaveAgentFile(ctx, "agt_rev", "USER.md", []byte("u1"),
		WithFileWriter("auto-persist", "+1 note")); err != nil {
		t.Fatalf("user save: %v", err)
	}

	revs, err := db.ListAgentFileRevisions(ctx, "agt_rev", "MEMORY.md", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(revs) != 3 {
		t.Fatalf("MEMORY.md revisions = %d, want 3 (noop skipped)", len(revs))
	}
	// Newest first.
	if revs[0].Writer != "tidy" || revs[0].Reason != "pruned 2 stale entries" {
		t.Errorf("newest revision writer/reason = %q/%q, want tidy/pruned 2 stale entries",
			revs[0].Writer, revs[0].Reason)
	}
	if revs[2].Writer != "system" {
		t.Errorf("default writer = %q, want system", revs[2].Writer)
	}
	if revs[0].Size != 2 {
		t.Errorf("size = %d, want len(\"v3\")=2", revs[0].Size)
	}
	// Failed CAS (stale) records nothing.
	if _, err := db.CompareAndSwapAgentFile(ctx, "agt_rev", "MEMORY.md", []byte("stale"), []byte("v4")); err != nil {
		t.Fatalf("stale cas: %v", err)
	}
	revs, _ = db.ListAgentFileRevisions(ctx, "agt_rev", "MEMORY.md", 0)
	if len(revs) != 3 {
		t.Fatalf("revisions after stale CAS = %d, want 3", len(revs))
	}

	// Detail view: content + previous content for diffing.
	detail, err := db.GetAgentFileRevision(ctx, "agt_rev", "MEMORY.md", revs[0].ID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if detail.Content != "v3" || detail.PrevContent != "v2" {
		t.Errorf("detail content/prev = %q/%q, want v3/v2", detail.Content, detail.PrevContent)
	}
	// Oldest revision has no previous.
	detail, err = db.GetAgentFileRevision(ctx, "agt_rev", "MEMORY.md", revs[2].ID)
	if err != nil {
		t.Fatalf("detail oldest: %v", err)
	}
	if detail.PrevContent != "" {
		t.Errorf("oldest prev content = %q, want empty", detail.PrevContent)
	}
	// Unknown revision id → ErrNotFound.
	if _, err := db.GetAgentFileRevision(ctx, "agt_rev", "MEMORY.md", 999999); err != ErrNotFound {
		t.Errorf("missing revision err = %v, want ErrNotFound", err)
	}

	// Prune keeps the newest N per (agent, filename) — both files keep 2.
	if _, err := db.PruneAgentFileRevisions(ctx, 2); err != nil {
		t.Fatalf("prune: %v", err)
	}
	memRevs, _ := db.ListAgentFileRevisions(ctx, "agt_rev", "MEMORY.md", 0)
	userRevs, _ := db.ListAgentFileRevisions(ctx, "agt_rev", "USER.md", 0)
	if len(memRevs) != 2 || len(userRevs) != 1 {
		t.Fatalf("after prune: MEMORY=%d USER=%d, want 2/1", len(memRevs), len(userRevs))
	}

	// keep<=0 disables recording; existing rows are frozen, not deleted.
	db.SetAgentFileRevisionKeep(0)
	if err := db.SaveAgentFile(ctx, "agt_rev", "MEMORY.md", []byte("v4")); err != nil {
		t.Fatalf("save with history off: %v", err)
	}
	if got, _ := db.GetAgentFile(ctx, "agt_rev", "MEMORY.md"); string(got) != "v4" {
		t.Fatalf("content with history off: %q", got)
	}
	revs, _ = db.ListAgentFileRevisions(ctx, "agt_rev", "MEMORY.md", 0)
	if len(revs) != 2 {
		t.Fatalf("revisions with history off = %d, want 2 (frozen)", len(revs))
	}
	if n, err := db.PruneAgentFileRevisions(ctx, 0); err != nil || n != 0 {
		t.Errorf("prune with keep=0 = (%d, %v), want (0, nil)", n, err)
	}
}

// DeleteAgent must take the revision history with the agent.
func TestDeleteAgentCascadesFileRevisions(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer db.Close()

	if err := db.SaveAgentFile(ctx, "agt_rev_del", "MEMORY.md", []byte("v1")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.DeleteAgent(ctx, "agt_rev_del"); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	revs, err := db.ListAgentFileRevisions(ctx, "agt_rev_del", "MEMORY.md", 0)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(revs) != 0 {
		t.Fatalf("revisions survived agent delete: %d", len(revs))
	}
}
