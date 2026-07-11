package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAgentMMRLambdaDefaultAndRoundTrip(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// No row yet → default.
	got, err := db.GetAgentMMRLambda(ctx, "agent-1")
	if err != nil {
		t.Fatalf("get default: %v", err)
	}
	if got != DefaultMMRLambda {
		t.Errorf("default lambda = %v, want %v", got, DefaultMMRLambda)
	}

	// Set then get.
	if err := db.SetAgentMMRLambda(ctx, "agent-1", 0.35); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err = db.GetAgentMMRLambda(ctx, "agent-1")
	if err != nil {
		t.Fatalf("get after set: %v", err)
	}
	if got != 0.35 {
		t.Errorf("lambda after set = %v, want 0.35", got)
	}

	// Upsert (same agent, new value) replaces the row.
	if err := db.SetAgentMMRLambda(ctx, "agent-1", 0.8); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ = db.GetAgentMMRLambda(ctx, "agent-1")
	if got != 0.8 {
		t.Errorf("lambda after upsert = %v, want 0.8", got)
	}

	// A different agent is unaffected — still default.
	got, _ = db.GetAgentMMRLambda(ctx, "agent-2")
	if got != DefaultMMRLambda {
		t.Errorf("agent-2 lambda = %v, want default", got)
	}
}

func TestInsertRecallEvent(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	ev := RecallEvent{
		RecallID:   "recall-abc",
		AgentID:    "agent-1",
		UserID:     "user-1",
		SessionKey: "sess-1",
		Lambda:     0.6,
		Explored:   true,
		SummaryIDs: []int64{10, 20, 30},
	}
	if err := db.InsertRecallEvent(ctx, ev); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Verify the row landed with the right payload (incl. user_id/session_key + JSON IDs).
	var (
		recallID   string
		userID     string
		sessionKey string
		lambda     float64
		explored   int
		idsJSON    string
	)
	err := db.db.QueryRowContext(ctx,
		`SELECT recall_id, user_id, session_key, lambda, explored, summary_ids FROM memory_recall_events WHERE recall_id = ?`,
		ev.RecallID).Scan(&recallID, &userID, &sessionKey, &lambda, &explored, &idsJSON)
	if err != nil {
		t.Fatalf("query back: %v", err)
	}
	if recallID != "recall-abc" || userID != "user-1" || sessionKey != "sess-1" || lambda != 0.6 || explored != 1 {
		t.Errorf("row = %q %q %q %v %v, want recall-abc user-1 sess-1 0.6 1", recallID, userID, sessionKey, lambda, explored)
	}
	if idsJSON != "[10,20,30]" {
		t.Errorf("summary_ids json = %q, want [10,20,30]", idsJSON)
	}
}

func TestTryUpgradeLambdaPromotesStrongExploration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Seed explored recalls: lambda=0.7 gets strong feedback (22 up / 3
	// down → 88%), lambda=0.5 gets weak (3 up / 22 down → 12%). Current
	// default lambda is 0.6 with no feedback.
	mustEvent := func(id string, lambda float64, up bool) {
		t.Helper()
		if err := db.InsertRecallEvent(ctx, RecallEvent{
			RecallID: id, AgentID: "a1", Lambda: lambda, Explored: true,
			SummaryIDs: []int64{1},
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.InsertRecallFeedback(ctx, id, up); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 25; i++ {
		mustEvent(fmt.Sprintf("r7-%d", i), 0.7, i >= 3) // 22 up
	}
	for i := 0; i < 25; i++ {
		mustEvent(fmt.Sprintf("r5-%d", i), 0.5, i < 3) // 3 up
	}

	upgraded, newLambda, err := db.TryUpgradeLambda(ctx, "a1")
	if err != nil {
		t.Fatalf("try upgrade: %v", err)
	}
	if !upgraded || newLambda != 0.7 {
		t.Errorf("upgrade = %v %v, want true 0.7", upgraded, newLambda)
	}
	got, _ := db.GetAgentMMRLambda(ctx, "a1")
	if got != 0.7 {
		t.Errorf("persisted lambda = %v, want 0.7", got)
	}
}

func TestTryUpgradeLambdaNoUpgradeBelowThreshold(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Only 5 samples at lambda=0.7 — below minSamples (20), no upgrade.
	for i := 0; i < 5; i++ {
		_ = db.InsertRecallEvent(ctx, RecallEvent{
			RecallID: fmt.Sprintf("x-%d", i), AgentID: "a1", Lambda: 0.7,
			Explored: true, SummaryIDs: []int64{1},
		})
		_ = db.InsertRecallFeedback(ctx, fmt.Sprintf("x-%d", i), true)
	}

	upgraded, newLambda, err := db.TryUpgradeLambda(ctx, "a1")
	if err != nil {
		t.Fatalf("try upgrade: %v", err)
	}
	if upgraded || newLambda != DefaultMMRLambda {
		t.Errorf("upgrade = %v %v, want false 0.6 (insufficient samples)", upgraded, newLambda)
	}
}

// TestIncrementConversationSummaryAccessMaintainsTimeSum guards the
// placeholder-ordering in IncrementConversationSummaryAccess: access_count
// AND access_time_sum must both grow. A sqlite ? misalignment would leave
// access_count at 0 (the UPDATE matches no rows), failing this test.
func TestIncrementConversationSummaryAccessMaintainsTimeSum(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	res, err := db.db.ExecContext(ctx, `INSERT INTO conversation_summaries
		(user_id, agent_id, session_key, chatter_user_id, summary, keywords, seq_start, seq_end)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"u1", "a1", "s1", "c1", "sum", "[]", 0, 10)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := res.LastInsertId()

	var ac0 int
	var ats0 int64
	if err := db.db.QueryRowContext(ctx, `SELECT access_count, access_time_sum FROM conversation_summaries WHERE id = ?`, id).Scan(&ac0, &ats0); err != nil {
		t.Fatalf("query before: %v", err)
	}
	if ac0 != 0 || ats0 != 0 {
		t.Fatalf("defaults: access_count=%d access_time_sum=%d, want 0/0", ac0, ats0)
	}

	before := time.Now().Unix()
	if err := db.IncrementConversationSummaryAccess(ctx, []int64{id}); err != nil {
		t.Fatalf("increment 1: %v", err)
	}
	var ac1 int
	var ats1 int64
	db.db.QueryRowContext(ctx, `SELECT access_count, access_time_sum FROM conversation_summaries WHERE id = ?`, id).Scan(&ac1, &ats1)
	if ac1 != 1 {
		t.Errorf("after 1st: access_count=%d, want 1", ac1)
	}
	if ats1 < before {
		t.Errorf("after 1st: access_time_sum=%d < call time %d", ats1, before)
	}

	// second increment: access_count must grow; access_time_sum must not shrink.
	if err := db.IncrementConversationSummaryAccess(ctx, []int64{id}); err != nil {
		t.Fatalf("increment 2: %v", err)
	}
	var ac2 int
	var ats2 int64
	db.db.QueryRowContext(ctx, `SELECT access_count, access_time_sum FROM conversation_summaries WHERE id = ?`, id).Scan(&ac2, &ats2)
	if ac2 != 2 {
		t.Errorf("after 2nd: access_count=%d, want 2", ac2)
	}
	if ats2 < ats1 {
		t.Errorf("after 2nd: access_time_sum=%d < %d (should not shrink)", ats2, ats1)
	}
}

func TestReRankSummariesPrefersNewerMeanRecallTime(t *testing.T) {
	// Two summaries with equal overlap/importance/creation; one has a
	// newer mean recall time → must rank first under batch min-max recency.
	base := time.Unix(1000, 0)
	older := ConversationSummary{ID: 1, Summary: "alpha beta", Keywords: []string{"alpha"}, CreatedAt: base, Importance: 3, AccessCount: 1, AccessTimeSum: 1000}
	newer := ConversationSummary{ID: 2, Summary: "alpha beta", Keywords: []string{"alpha"}, CreatedAt: base, Importance: 3, AccessCount: 1, AccessTimeSum: 5000}

	out := reRankSummaries([]ConversationSummary{older, newer}, "alpha", 2)
	if len(out) != 2 {
		t.Fatalf("got %d results, want 2", len(out))
	}
	if out[0].ID != 2 {
		t.Errorf("newer mean-recall-time (id=2) should rank first, got id=%d", out[0].ID)
	}
}

// TestVec0SelectProbe isolates whether a non-KNN SELECT on the vec0
// virtual table (GetConversationSummaryEmbeddings) hangs — it has never
// been exercised by a test before the implicit-feedback sweep.
func TestVec0SelectProbe(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	v := make([]float32, 1024)
	v[0] = 1
	if err := db.InsertConversationSummaryVector(ctx, 1, v); err != nil {
		t.Fatalf("insert: %v", err)
	}
	m, err := db.GetConversationSummaryEmbeddings(ctx, []int64{1})
	if err != nil {
		t.Fatalf("get embeddings: %v", err)
	}
	if len(m) != 1 {
		t.Errorf("got %d embeddings, want 1", len(m))
	}
}

// TestSweepStepsProbe exercises each step the sweep performs, with t.Log
// before/after each, to localize a hang.
func TestSweepStepsProbe(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	v := make([]float32, 1024)
	v[0] = 1
	if err := db.InsertConversationSummaryVector(ctx, 100, v); err != nil {
		t.Fatal(err)
	}
	t.Log("vec inserted")

	if err := db.InsertRecallEvent(ctx, RecallEvent{
		RecallID: "p1", AgentID: "a1", UserID: "u1", SessionKey: "s1",
		Lambda: 0.6, Explored: true, SummaryIDs: []int64{100},
	}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * time.Minute)
	if _, err := db.db.ExecContext(ctx, `UPDATE memory_recall_events SET created_at = ? WHERE recall_id = ?`, old, "p1"); err != nil {
		t.Fatal(err)
	}
	t.Log("event inserted + backdated")

	for i := 0; i < 3; i++ {
		if _, err := db.db.ExecContext(ctx,
			`INSERT INTO session_messages (user_id, agent_id, session_key, seq, role, content, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"u1", "a1", "s1", i, "user", fmt.Sprintf("alpha %d", i), old.Add(time.Duration(i+1)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("messages inserted")

	cutoff := time.Now().Add(-5 * time.Minute)
	rows, err := db.db.QueryContext(ctx,
		`SELECT recall_id FROM memory_recall_events
		 WHERE explored = 1 AND session_key != '' AND user_id != '' AND created_at < ?
		   AND NOT EXISTS (SELECT 1 FROM memory_recall_feedback f WHERE f.recall_id = memory_recall_events.recall_id)
		 ORDER BY created_at LIMIT ?`, cutoff, 50)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		count++
	}
	rows.Close()
	t.Logf("events queried: %d", count)

	msgs, err := db.ListSessionMessagesAfterTime(ctx, "u1", "a1", "s1", old, 3)
	t.Logf("messages listed: %d err=%v", len(msgs), err)

	embs, err := db.GetConversationSummaryEmbeddings(ctx, []int64{100})
	t.Logf("embeddings: %d err=%v", len(embs), err)
}

// sweepMockEmbedder maps "alpha" content → dim0 unit, else → dim1023
// (orthogonal to both test summaries, cosine ≈ 0).
type sweepMockEmbedder struct{}

func (sweepMockEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 1024)
		if strings.Contains(strings.ToLower(t), "alpha") {
			v[0] = 1
		} else {
			v[1023] = 1
		}
		out[i] = v
	}
	return out, nil
}
func (sweepMockEmbedder) Model() string   { return "mock" }
func (sweepMockEmbedder) Dim() int        { return 1024 }
func (sweepMockEmbedder) Available() bool { return true }

func TestSweepImplicitFeedbackRecordsUpDown(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	dim0 := make([]float32, 1024)
	dim0[0] = 1
	dim1 := make([]float32, 1024)
	dim1[1] = 1
	if err := db.InsertConversationSummaryVector(ctx, 100, dim0); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertConversationSummaryVector(ctx, 200, dim1); err != nil {
		t.Fatal(err)
	}

	mk := func(recallID string, summaryID int64, msgs []string) {
		ev := RecallEvent{
			RecallID: recallID, AgentID: "a1", UserID: "u1", SessionKey: "s-" + recallID,
			Lambda: 0.6, Explored: true, SummaryIDs: []int64{summaryID},
		}
		if err := db.InsertRecallEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-30 * time.Minute)
		if _, err := db.db.ExecContext(ctx, `UPDATE memory_recall_events SET created_at = ? WHERE recall_id = ?`, old, recallID); err != nil {
			t.Fatal(err)
		}
		for i, c := range msgs {
			if _, err := db.db.ExecContext(ctx,
				`INSERT INTO session_messages (user_id, agent_id, session_key, seq, role, content, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				"u1", "a1", "s-"+recallID, i, "user", c, old.Add(time.Duration(i+1)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("rc-up", 100, []string{"alpha details", "more alpha", "alpha again"})
	mk("rc-down", 200, []string{"beta off topic", "gamma other", "delta unrelated"})

	cfg := DefaultImplicitFeedbackConfig
	cfg.WindowMessages = 3
	cfg.MaxAgeMinutes = 5
	n, err := db.SweepImplicitFeedback(ctx, sweepMockEmbedder{}, cfg)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Errorf("processed = %d, want 2", n)
	}
	var upVal int
	db.db.QueryRowContext(ctx, `SELECT up FROM memory_recall_feedback WHERE recall_id = ?`, "rc-up").Scan(&upVal)
	if upVal != 1 {
		t.Errorf("rc-up feedback up=%d, want 1", upVal)
	}
	db.db.QueryRowContext(ctx, `SELECT up FROM memory_recall_feedback WHERE recall_id = ?`, "rc-down").Scan(&upVal)
	if upVal != 0 {
		t.Errorf("rc-down feedback up=%d, want 0", upVal)
	}
}
