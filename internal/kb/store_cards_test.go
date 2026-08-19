package kb

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// setupCardsTestDB opens an in-memory sqlite with just the kb_cards family
// of tables — enough surface for the card store tests (no vector tables:
// the embedder is nil here, so only the always-on keyword dedup leg runs).
func setupCardsTestDB(t *testing.T) *KBStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE kb_cards (
			id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, question TEXT NOT NULL,
			answer TEXT NOT NULL DEFAULT '', source_type TEXT NOT NULL DEFAULT 'manual',
			source_ref TEXT NOT NULL DEFAULT '', source_excerpt TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active', interval_index INTEGER NOT NULL DEFAULT 0,
			due_at TEXT NOT NULL DEFAULT '', last_reviewed_at TEXT NOT NULL DEFAULT '',
			review_count INTEGER NOT NULL DEFAULT 0, lapse_count INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE kb_card_reviews (
			id INTEGER PRIMARY KEY AUTOINCREMENT, card_id TEXT NOT NULL, agent_id TEXT NOT NULL,
			grade TEXT NOT NULL, prev_interval_index INTEGER NOT NULL DEFAULT 0,
			new_interval_index INTEGER NOT NULL DEFAULT 0, new_due_at TEXT NOT NULL DEFAULT '',
			reviewed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE kb_card_embeddings (
			card_id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, embedding BLOB,
			dim INTEGER NOT NULL DEFAULT 0, model TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	return NewKBStore(db, "sqlite")
}

// TestCardReviewLadder walks one card through the full Ebbinghaus ladder:
// each "remembered" advances the interval index, "fuzzy" holds it, and
// "forgot" resets to 0 with a lapse — then enough "remembered" grades
// master the card and clear due_at.
func TestCardReviewLadder(t *testing.T) {
	store := setupCardsTestDB(t)
	ctx := context.Background()
	const agent = "agt_cards"

	id, err := store.SaveCard(ctx, agent, "什么是前缀缓存？", "复用请求前缀的 KV 缓存以降低 TTFT", "manual", "", "")
	if err != nil {
		t.Fatalf("SaveCard: %v", err)
	}
	card, _ := store.GetCard(ctx, agent, id)
	if card.DueAt == nil {
		t.Fatalf("new card should be scheduled for tomorrow")
	}
	if card.Status != "active" || card.IntervalIndex != 0 {
		t.Fatalf("new card state: %+v", card)
	}

	// remembered ×2 → index 2 (4-day interval)
	for i := 1; i <= 2; i++ {
		card, err = store.ReviewCard(ctx, agent, id, "remembered")
		if err != nil {
			t.Fatalf("remembered #%d: %v", i, err)
		}
		if card.IntervalIndex != i {
			t.Fatalf("after %d remembered: interval_index=%d want %d", i, card.IntervalIndex, i)
		}
		if card.DueAt == nil {
			t.Fatalf("after %d remembered: due_at should be set", i)
		}
	}

	// fuzzy → index unchanged, due re-pushed
	card, err = store.ReviewCard(ctx, agent, id, "fuzzy")
	if err != nil {
		t.Fatalf("fuzzy: %v", err)
	}
	if card.IntervalIndex != 2 || card.DueAt == nil {
		t.Fatalf("fuzzy state: %+v", card)
	}

	// forgot → reset to 0, lapse recorded
	card, err = store.ReviewCard(ctx, agent, id, "forgot")
	if err != nil {
		t.Fatalf("forgot: %v", err)
	}
	if card.IntervalIndex != 0 || card.LapseCount != 1 || card.Status != "active" {
		t.Fatalf("forgot state: %+v", card)
	}

	// run the ladder to mastery: 6 remembered grades from index 0
	for i := 0; i < len(CardIntervals); i++ {
		card, err = store.ReviewCard(ctx, agent, id, "remembered")
		if err != nil {
			t.Fatalf("ladder step %d: %v", i, err)
		}
	}
	if card.Status != "mastered" || card.DueAt != nil {
		t.Fatalf("mastered state: %+v", card)
	}
	if card.ReviewCount != 10 { // 2 remembered + fuzzy + forgot + 6 ladder
		t.Fatalf("review_count=%d want 10", card.ReviewCount)
	}

	// timeline recorded every grade
	reviews, err := store.ListCardReviews(ctx, agent, id)
	if err != nil || len(reviews) != 10 {
		t.Fatalf("ListCardReviews: %v len=%d want 10", err, len(reviews))
	}

	// mastered + forgot reactivates at step 0
	card, err = store.ReviewCard(ctx, agent, id, "forgot")
	if err != nil {
		t.Fatalf("mastered forgot: %v", err)
	}
	if card.Status != "active" || card.IntervalIndex != 0 {
		t.Fatalf("reactivated state: %+v", card)
	}
}

// TestCardListFiltersAndStats checks the library filters and the dashboard
// stats: due window, source filter, search, and per-status counts.
func TestCardListFiltersAndStats(t *testing.T) {
	store := setupCardsTestDB(t)
	ctx := context.Background()
	const agent = "agt_cards2"

	// due: yesterday → due today; today+3d → not yet due
	dueID, _ := store.SaveCard(ctx, agent, "到期卡", "a", "diary", "2026-08-17", "")
	store.db.Exec(`UPDATE kb_cards SET due_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339), dueID)
	futureID, _ := store.SaveCard(ctx, agent, "未到期卡", "b", "wiki", "p1", "")
	store.db.Exec(`UPDATE kb_cards SET due_at = ? WHERE id = ?`,
		time.Now().UTC().Add(72*time.Hour).Format(time.RFC3339), futureID)
	searchID, _ := store.SaveCard(ctx, agent, "前缀缓存是什么", "KV 复用", "manual", "", "")

	due, err := store.ListCards(ctx, agent, "due", "", "", 50, 0)
	if err != nil {
		t.Fatalf("ListCards due: %v", err)
	}
	if len(due) != 1 || due[0].ID != dueID {
		t.Fatalf("due filter: got %d cards, want 1 (%s)", len(due), dueID)
	}

	wiki, _ := store.ListCards(ctx, agent, "all", "wiki", "", 50, 0)
	if len(wiki) != 1 || wiki[0].ID != futureID {
		t.Fatalf("source filter: got %d wiki cards", len(wiki))
	}

	hit, _ := store.ListCards(ctx, agent, "all", "", "前缀缓存", 50, 0)
	if len(hit) != 1 || hit[0].ID != searchID {
		t.Fatalf("search: got %d hits", len(hit))
	}

	stats, err := store.CardStats(ctx, agent)
	if err != nil {
		t.Fatalf("CardStats: %v", err)
	}
	if stats.DueToday != 1 || stats.Active != 3 {
		t.Fatalf("stats: %+v", stats)
	}

	// ListDueQueue: most-overdue first, capped — the review-session feed.
	// A third card 72h overdue must sort ahead of the 24h one even though
	// it was created later (ListCards' created_at DESC would cut the
	// wrong end of a capped list).
	older, _ := store.SaveCard(ctx, agent, "最久未复习", "a", "diary", "2026-08-17", "")
	store.db.Exec(`UPDATE kb_cards SET due_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-72*time.Hour).Format(time.RFC3339), older)
	queue, err := store.ListDueQueue(ctx, agent, 2)
	if err != nil {
		t.Fatalf("ListDueQueue: %v", err)
	}
	head := "none"
	if len(queue) > 0 {
		head = queue[0].ID
	}
	if len(queue) != 2 || queue[0].ID != older || queue[1].ID != dueID {
		t.Fatalf("due queue order/cap: got %d cards head=%q, want [%s %s]",
			len(queue), head, older, dueID)
	}

	// one review today → streak 1
	if _, err := store.ReviewCard(ctx, agent, dueID, "remembered"); err != nil {
		t.Fatalf("review: %v", err)
	}
	stats, _ = store.CardStats(ctx, agent)
	if stats.StreakDays != 1 {
		t.Fatalf("streak=%d want 1", stats.StreakDays)
	}
}

// TestCardArchiveAndDuplicate covers archive/restore lifecycle and the
// keyword leg of generation-time dedup (identical question blocks, a
// different one passes).
func TestCardArchiveAndDuplicate(t *testing.T) {
	store := setupCardsTestDB(t)
	ctx := context.Background()
	const agent = "agt_cards3"

	id, _ := store.SaveCard(ctx, agent, "Go 的 goroutine 是什么", "用户态轻量线程", "manual", "", "")
	if err := store.ArchiveCard(ctx, agent, id); err != nil {
		t.Fatalf("ArchiveCard: %v", err)
	}
	// archived cards drop out of the default rotation lists
	active, _ := store.ListCards(ctx, agent, "active", "", "", 50, 0)
	if len(active) != 0 {
		t.Fatalf("archived card still listed active")
	}
	if _, err := store.ReviewCard(ctx, agent, id, "remembered"); err == nil {
		t.Fatalf("reviewing an archived card should fail")
	}
	if err := store.RestoreCard(ctx, agent, id); err != nil {
		t.Fatalf("RestoreCard: %v", err)
	}

	if dup := store.CheckCardDuplicate(ctx, agent, "  go 的 GOROUTINE 是什么 "); dup == nil {
		t.Fatalf("normalized question duplicate should be caught")
	}
	if dup := store.CheckCardDuplicate(ctx, agent, "channel 是什么"); dup != nil {
		t.Fatalf("distinct question flagged duplicate")
	}

	if err := store.DeleteCard(ctx, agent, id); err != nil {
		t.Fatalf("DeleteCard: %v", err)
	}
	if _, err := store.GetCard(ctx, agent, id); err == nil {
		t.Fatalf("deleted card still readable")
	}
	if dup := store.CheckCardDuplicate(ctx, agent, "Go 的 goroutine 是什么"); dup != nil {
		t.Fatalf("duplicate hit after delete")
	}
}
