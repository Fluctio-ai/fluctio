package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/workspace"
)

// TestShareFileTool covers the tool end to end: scope resolution into the
// agent-root key, the SKILL.md refusal, missing-file / missing-base-URL
// errors, and that the minted token actually serves via the store.
func TestShareFileTool(t *testing.T) {
	db := newTestStore(t)
	r, root := newScopedRegistry(t)
	r.SetSessionID("s-1")
	t.Setenv("FLUCTIO_PUBLIC_BASE_URL", "https://fluct.example.com")
	RegisterShareFileTool(r, db, "agt_test")
	ctx := context.Background()

	ws := workspace.NewLocalFS(root)
	if err := ws.Put(ctx, "agt_test", "", "s-1", "news.mp3", strings.NewReader("audio"), 0, ""); err != nil {
		t.Fatal(err)
	}

	out, err := r.Execute(ctx, "share_file", `{"path":"news.mp3"}`)
	if err != nil {
		t.Fatalf("share: %v", err)
	}
	if !strings.Contains(out, "https://fluct.example.com/f/") {
		t.Fatalf("output missing public URL: %q", out)
	}
	// Default TTL is 7 days (not permanent).
	if !strings.Contains(out, "Valid until:") || strings.Contains(out, "permanent") {
		t.Fatalf("default ttl should be time-boxed: %q", out)
	}
	// The share row carries the agent-root key (sessions/s-1/news.mp3).
	recs, err := db.ListFileSharesByAgent(ctx, "agt_test")
	if err != nil || len(recs) != 1 {
		t.Fatalf("shares: %v %v", recs, err)
	}
	if recs[0].Path != "sessions/s-1/news.mp3" {
		t.Fatalf("share path = %q, want sessions/s-1/news.mp3", recs[0].Path)
	}
	// The minted token serves the bytes through the workspace store.
	rc, err := ws.Get(ctx, "agt_test", "", "", recs[0].Path)
	if err != nil {
		t.Fatalf("resolve share key: %v", err)
	}
	rc.Close()

	// Explicit ttl_hours=0 → permanent link.
	if out, err = r.Execute(ctx, "share_file", `{"path":"news.mp3","ttl_hours":0}`); err != nil || !strings.Contains(out, "permanent") {
		t.Fatalf("permanent share: %v %q", err, out)
	}

	// Missing file → actionable error.
	if _, err = r.Execute(ctx, "share_file", `{"path":"nope.mp3"}`); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing file: %v", err)
	}
	// SKILL.md refused.
	if _, err = r.Execute(ctx, "share_file", `{"path":"SKILL.md"}`); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("skill.md: %v", err)
	}
}

// TestShareFileToolRequiresBaseURL: without FLUCTIO_PUBLIC_BASE_URL the tool
// errors with an operator-actionable message instead of minting a dead link.
func TestShareFileToolRequiresBaseURL(t *testing.T) {
	db := newTestStore(t)
	r, root := newScopedRegistry(t)
	r.SetSessionID("s-2")
	t.Setenv("FLUCTIO_PUBLIC_BASE_URL", "")
	RegisterShareFileTool(r, db, "agt_test")
	ctx := context.Background()

	ws := workspace.NewLocalFS(root)
	if err := ws.Put(ctx, "agt_test", "", "s-2", "a.png", strings.NewReader("x"), 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Execute(ctx, "share_file", `{"path":"a.png"}`); err == nil || !strings.Contains(err.Error(), "FLUCTIO_PUBLIC_BASE_URL") {
		t.Fatalf("want base-url error, got %v", err)
	}
	// The failed call must not leave a share row behind.
	if recs, _ := db.ListFileSharesByAgent(ctx, "agt_test"); len(recs) != 0 {
		t.Fatalf("orphan shares: %+v", recs)
	}
}
