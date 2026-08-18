package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// TestCardsPushStampAndDigest covers the once-a-day stamp round-trip and
// the digest body: count, first-three teaser, no link without a public
// base URL. The full push cycle needs a live channel manager — e2e
// territory (M6), not unit-testable here.
func TestCardsPushStampAndDigest(t *testing.T) {
	dbs, err := store.NewDBStore("sqlite", "file::memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer dbs.Close()
	if err := dbs.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	today := time.Now().In(diaryCST).Format("2006-01-02")

	if cardsPushedToday(ctx, dbs, "a1", today) {
		t.Fatalf("nothing pushed yet but flagged pushed")
	}
	if err := stampCardsPush(ctx, dbs, "a1", today, 5, "telegram"); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if !cardsPushedToday(ctx, dbs, "a1", today) {
		t.Fatalf("stamp not readable back")
	}
	if cardsPushedToday(ctx, dbs, "a2", today) {
		t.Fatalf("stamp leaked across agents")
	}
	if cardsPushedToday(ctx, dbs, "a1", "2020-01-01") {
		t.Fatalf("stamp leaked across dates")
	}

	due := []kb.KBCard{
		{Question: "Q1？"}, {Question: "Q2？"}, {Question: "Q3？"}, {Question: "Q4？"},
	}
	msg := formatCardsDigest("agt_x", due)
	if !strings.Contains(msg, "4 张待复习") {
		t.Fatalf("digest missing count: %q", msg)
	}
	if !strings.Contains(msg, "Q1？") || !strings.Contains(msg, "Q3？") {
		t.Fatalf("digest missing teaser questions: %q", msg)
	}
	if strings.Contains(msg, "Q4？") {
		t.Fatalf("digest should stop at three teasers: %q", msg)
	}
	if strings.Contains(msg, "/knowledge/cards") {
		t.Fatalf("no public base URL configured — link should be omitted: %q", msg)
	}
}
