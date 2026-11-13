package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFileShareLifecycle(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	exp := time.Now().Add(time.Hour).UTC()
	rec := &FileShareRecord{
		Token:     "sharetest-tok-1",
		AgentID:   "agt_share",
		Path:      "sessions/s-1/news.mp3",
		CreatedBy: "u1",
		ExpiresAt: &exp,
		MaxUses:   2,
	}
	if err := db.CreateFileShare(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := db.GetFileShare(ctx, rec.Token)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Path != rec.Path || got.MaxUses != 2 || got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// Consume twice (max_uses=2); the third is rejected atomically.
	for i := 0; i < 2; i++ {
		c, err := db.ConsumeFileShare(ctx, rec.Token, time.Now().UTC())
		if err != nil {
			t.Fatalf("consume %d: %v", i, err)
		}
		if c.UseCount != i+1 {
			t.Fatalf("use_count = %d, want %d", c.UseCount, i+1)
		}
	}
	if _, err := db.ConsumeFileShare(ctx, rec.Token, time.Now().UTC()); !errors.Is(err, ErrFileShareExhausted) {
		t.Fatalf("expected ErrFileShareExhausted, got %v", err)
	}

	if _, err := db.ConsumeFileShare(ctx, "sharetest-nope", time.Now().UTC()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	list, err := db.ListFileSharesByAgent(ctx, "agt_share")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if err := db.DeleteFileShare(ctx, rec.Token); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetFileShare(ctx, rec.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestFileShareExpiryAndPermanent(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Expired share: consume rejects even with uses left, without burning
	// a use (the expiry guard lives inside the same UPDATE).
	past := time.Now().Add(-time.Minute).UTC()
	exp1 := &FileShareRecord{Token: "sharetest-exp", AgentID: "agt_share2", Path: "a.mp3", ExpiresAt: &past, MaxUses: 5}
	if err := db.CreateFileShare(ctx, exp1); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.ConsumeFileShare(ctx, exp1.Token, time.Now().UTC()); !errors.Is(err, ErrFileShareExhausted) {
		t.Fatalf("expected exhausted for expired, got %v", err)
	}
	if rec, _ := db.GetFileShare(ctx, exp1.Token); rec != nil && rec.UseCount != 0 {
		t.Fatalf("expired consume burned a use: use_count=%d", rec.UseCount)
	}

	// Permanent share (ExpiresAt nil): unlimited consumes, never pruned.
	perm := &FileShareRecord{Token: "sharetest-perm", AgentID: "agt_share2", Path: "b.png"}
	if err := db.CreateFileShare(ctx, perm); err != nil {
		t.Fatalf("create perm: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := db.ConsumeFileShare(ctx, perm.Token, time.Now().UTC()); err != nil {
			t.Fatalf("perm consume %d: %v", i, err)
		}
	}

	// Prune removes only the expired row.
	n, err := db.PruneExpiredFileShares(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want 1", n)
	}
	if _, err := db.GetFileShare(ctx, perm.Token); err != nil {
		t.Fatalf("permanent share pruned: %v", err)
	}
}
