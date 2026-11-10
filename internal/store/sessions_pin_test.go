package store

import (
	"context"
	"testing"
	"time"
)

// TestSessionPinnedOrder covers the sidebar keep-at-top flag end-to-end:
// SetSessionPinned flips the row, ListSessions orders pinned first
// (recency within the pinned group), SaveSession's per-turn upsert must
// not clobber the flag, and unpinning restores pure recency order.
func TestSessionPinnedOrder(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	const agentID = "agt-pin"
	save := func(key string) {
		rec := &SessionRecord{Messages: []SessionMessage{{Role: "user", Content: "hi " + key}}}
		if err := db.SaveSession(ctx, agentID, key, rec); err != nil {
			t.Fatalf("save %s: %v", key, err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	// Inserted oldest → newest: a, b, c. Plain recency order is c, b, a.
	save("s-a")
	save("s-b")
	save("s-c")

	// Pin the OLDEST session — it must float to the front.
	if err := db.SetSessionPinned(ctx, agentID, "s-a", true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	metas, err := db.ListSessions(ctx, agentID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(metas) != 3 {
		t.Fatalf("list len = %d; want 3", len(metas))
	}
	if metas[0].Key != "s-a" || !metas[0].Pinned {
		t.Fatalf("first = (%s, pinned=%v); want (s-a, true)", metas[0].Key, metas[0].Pinned)
	}
	if metas[1].Key != "s-c" || metas[2].Key != "s-b" {
		t.Fatalf("unpinned tail = [%s, %s]; want [s-c, s-b] (recency)", metas[1].Key, metas[2].Key)
	}
	for _, m := range metas[1:] {
		if m.Pinned {
			t.Errorf("%s unexpectedly pinned", m.Key)
		}
	}

	// A per-turn SaveSession on the pinned row must keep the flag (the
	// upsert's UPDATE clause doesn't touch pinned).
	if err := db.SaveSession(ctx, agentID, "s-a", &SessionRecord{
		Messages: []SessionMessage{{Role: "user", Content: "another turn"}},
	}); err != nil {
		t.Fatalf("re-save pinned: %v", err)
	}
	metas, err = db.ListSessions(ctx, agentID)
	if err != nil {
		t.Fatalf("list after re-save: %v", err)
	}
	if !metas[0].Pinned || metas[0].Key != "s-a" {
		t.Fatalf("pinned flag lost after re-save: first = (%s, pinned=%v)", metas[0].Key, metas[0].Pinned)
	}

	// Unpin restores pure recency order.
	if err := db.SetSessionPinned(ctx, agentID, "s-a", false); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	metas, err = db.ListSessions(ctx, agentID)
	if err != nil {
		t.Fatalf("list after unpin: %v", err)
	}
	// s-a was re-saved last, so it's now the most recently updated.
	if metas[0].Key != "s-a" || metas[1].Key != "s-c" || metas[2].Key != "s-b" {
		t.Fatalf("order after unpin = [%s %s %s]; want [s-a s-c s-b]",
			metas[0].Key, metas[1].Key, metas[2].Key)
	}
	for _, m := range metas {
		if m.Pinned {
			t.Errorf("%s still pinned after unpin", m.Key)
		}
	}
}

// TestProjectPinnedRoundTrip covers the projects keep-at-top flag: the
// pin survives a name-edit read-modify-write and drives the pinned-first
// listing order.
func TestProjectPinnedRoundTrip(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	const agentID = "agt-pinproj"
	projA := &ProjectRecord{AgentID: agentID, ID: "proj_a", Name: "A"}
	if err := db.SaveProject(ctx, projA); err != nil {
		t.Fatalf("save A: %v", err)
	}
	time.Sleep(15 * time.Millisecond)
	projB := &ProjectRecord{AgentID: agentID, ID: "proj_b", Name: "B"}
	if err := db.SaveProject(ctx, projB); err != nil {
		t.Fatalf("save B: %v", err)
	}

	// Plain recency: B first.
	projects, err := db.ListProjects(ctx, agentID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(projects) != 2 || projects[0].ID != "proj_b" {
		t.Fatalf("baseline order = [%v]; want proj_b first", projectIDs(projects))
	}

	// Pin the older project via read-modify-write (the PATCH handler's
	// shape), then rename it in a second write — both must preserve the
	// flag, and the pinned row must lead the listing.
	existing, err := db.GetProject(ctx, agentID, "proj_a")
	if err != nil {
		t.Fatalf("get A: %v", err)
	}
	existing.Pinned = true
	if err := db.SaveProject(ctx, existing); err != nil {
		t.Fatalf("pin A: %v", err)
	}
	existing, err = db.GetProject(ctx, agentID, "proj_a")
	if err != nil {
		t.Fatalf("re-get A: %v", err)
	}
	if !existing.Pinned {
		t.Fatal("pinned flag lost after read-back")
	}
	existing.Name = "A renamed"
	if err := db.SaveProject(ctx, existing); err != nil {
		t.Fatalf("rename A: %v", err)
	}

	projects, err = db.ListProjects(ctx, agentID)
	if err != nil {
		t.Fatalf("list after pin: %v", err)
	}
	if projects[0].ID != "proj_a" || !projects[0].Pinned {
		t.Fatalf("first = (%s, pinned=%v); want (proj_a, true)", projects[0].ID, projects[0].Pinned)
	}
	if projects[0].Name != "A renamed" {
		t.Fatalf("name = %q; want %q", projects[0].Name, "A renamed")
	}
	if projects[1].ID != "proj_b" || projects[1].Pinned {
		t.Fatalf("second = (%s, pinned=%v); want (proj_b, false)", projects[1].ID, projects[1].Pinned)
	}

	// Unpin. SaveProject bumps updated_at on every write, so the just-
	// unpinned A now leads the unpinned group (same bump a rename gets —
	// pre-existing semantics). Assert flags, not order.
	existing, err = db.GetProject(ctx, agentID, "proj_a")
	if err != nil {
		t.Fatalf("get A before unpin: %v", err)
	}
	existing.Pinned = false
	if err := db.SaveProject(ctx, existing); err != nil {
		t.Fatalf("unpin A: %v", err)
	}
	projects, err = db.ListProjects(ctx, agentID)
	if err != nil {
		t.Fatalf("list after unpin: %v", err)
	}
	for _, p := range projects {
		if p.Pinned {
			t.Errorf("%s still pinned after unpin", p.ID)
		}
	}
}

func projectIDs(ps []ProjectRecord) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}
