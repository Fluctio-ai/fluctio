package recallcheck

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"strconv"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// mockEmb embeds every text as one axis unit vector, so a fixture's cosine
// against any query is decided entirely by its stored vector: same axis →
// 1.0 (passes every threshold), different axis → 0 (dies at the gate). Two
// axes let one fixture set serve both hit tests (axis 0) and unanswerable
// traps (axis 1, orthogonal to everything).
type mockEmb struct {
	axis int
}

func vecAxis(axis int) []float32 {
	v := make([]float32, 1024)
	v[axis] = 1
	return v
}
func blob(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

func (m mockEmb) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = vecAxis(m.axis)
	}
	return out, nil
}
func (mockEmb) Model() string   { return "mock" }
func (mockEmb) Dim() int        { return 1024 }
func (mockEmb) Available() bool { return true }

func setup(t *testing.T) (context.Context, *store.DBStore, *kb.KBStore) {
	t.Helper()
	ctx := context.Background()
	db, err := store.NewDBStore("sqlite", "file:recallcheck_test?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ks := kb.NewKBStore(db.DB(), db.Dialect())
	ks.SetRetriever(mockEmb{axis: 0}, nil)
	return ctx, db, ks
}

func seedFixtures(t *testing.T, ctx context.Context, db *store.DBStore) {
	t.Helper()
	raw := db.DB()
	// Aligned wiki page on axis 0 (survives gates). All fixtures live on
	// axis 0 so an axis-1 embedder is orthogonal to everything — that is
	// what makes unanswerable traps stay silent.
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO wiki_pages (id, agent_id, page_type, slug, title, body, summary, source_ids, tags) VALUES ('entity:foo', 'a1', 'entity', 'foo', 'Foo Topic', 'body', 'summary', '[]', '[]')`); err != nil {
		t.Fatalf("insert page: %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO wiki_page_embeddings (page_id, agent_id, embedding, dim, model) VALUES ('entity:foo', 'a1', ?, 1024, 'mock')`,
		blob(vecAxis(0))); err != nil {
		t.Fatalf("insert page emb: %v", err)
	}
	// Aligned conversation summary for the [MEM] lane.
	id, err := db.InsertConversationSummary(ctx, store.ConversationSummary{
		AgentID: "a1", SessionKey: "s1", Topic: "Milk Foam Notes",
		Summary: "milk foam steaming notes", Keywords: []string{"milk"},
		SeqStart: 1, SeqEnd: 2, Kind: "durable",
	})
	if err != nil {
		t.Fatalf("insert summary: %v", err)
	}
	if err := db.InsertConversationSummaryVector(ctx, id, vecAxis(0)); err != nil {
		t.Fatalf("insert summary vec: %v", err)
	}
}

func TestSeedGeneratesCasesAcrossLanes(t *testing.T) {
	ctx, db, _ := setup(t)
	defer db.Close()
	seedFixtures(t, ctx, db)

	cases, err := Seed(ctx, db, "a1", 2, 1)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	lanes := map[string]int{}
	traps := 0
	for _, c := range cases {
		if c.Schema != GoldenCaseSchema {
			t.Errorf("case %s missing schema tag", c.ID)
		}
		if c.Unanswerable {
			traps++
		} else {
			lanes[c.Lane]++
		}
	}
	if lanes["wiki"] == 0 || lanes["mem_inject"] == 0 {
		t.Errorf("seeded lanes missing wiki/mem: %+v", lanes)
	}
	if traps != 1 {
		t.Errorf("unanswerable traps = %d, want 1", traps)
	}
}

func TestRunComputesLaneMetrics(t *testing.T) {
	ctx, db, ks := setup(t)
	defer db.Close()
	seedFixtures(t, ctx, db)

	cases := []GoldenCase{
		{Schema: GoldenCaseSchema, ID: "w1", Query: "Foo Topic", Lane: "wiki", ExpectIDs: []string{"entity:foo"}},
		{Schema: GoldenCaseSchema, ID: "m1", Query: "milk foam", Lane: "mem_inject", ExpectIDs: []string{"" /* filled below */}},
	}
	// Resolve the mem expectation from the seeded row.
	var sumID int64
	if err := db.DB().QueryRowContext(ctx, `SELECT id FROM conversation_summaries WHERE agent_id='a1' LIMIT 1`).Scan(&sumID); err != nil {
		t.Fatalf("lookup summary: %v", err)
	}
	cases[1].ExpectIDs = []string{strconv.FormatInt(sumID, 10)}

	rep := Run(ctx, Deps{DB: db, KB: ks, AgentID: "a1", Embedder: mockEmb{axis: 0}}, cases)
	if rep.Degraded {
		t.Fatal("report degraded with an embedder wired")
	}
	ws := rep.ByLane["wiki"]
	if ws == nil || ws.Cases != 1 || ws.Hit1 != 1 || ws.MRR != 1.0 {
		t.Errorf("wiki stat wrong: %+v", ws)
	}
	ms := rep.ByLane["mem_inject"]
	if ms == nil || ms.Cases != 1 || ms.Hit1 != 1 {
		t.Errorf("mem_inject stat wrong: %+v", ms)
	}
	for _, r := range rep.Cases {
		if r.Case.ID == "w1" && r.Rank != 1 {
			t.Errorf("w1 rank = %d, want 1 (hits %v)", r.Rank, r.Hits)
		}
	}

	// Unanswerable traps run under an orthogonal-axis embedder: every
	// fixture is axis 0, so an axis-1 query matches nothing and every lane
	// must stay silent. The KB lanes embed through the store's own
	// retriever (production parity), so rewire it for this run; only the
	// mem lane reads Deps.Embedder. (With the hit-test embedder any query
	// is axis 0 and the dumb fixture matches regardless of query text —
	// mocks have no semantics; production embeddings do.)
	ks.SetRetriever(mockEmb{axis: 1}, nil)
	trap := []GoldenCase{
		{Schema: GoldenCaseSchema, ID: "u1", Query: "zz-no-such-thing-aaaa", Lane: "any", Unanswerable: true},
	}
	rep2 := Run(ctx, Deps{DB: db, KB: ks, AgentID: "a1", Embedder: mockEmb{axis: 1}}, trap)
	us := rep2.ByLane["unanswerable"]
	if us == nil || us.Cases != 1 || us.Violations != 0 {
		t.Errorf("unanswerable stat wrong (traps must stay silent): %+v", us)
	}
}

func TestRunMissCountedWhenLaneKilled(t *testing.T) {
	ctx, db, ks := setup(t)
	defer db.Close()
	seedFixtures(t, ctx, db)

	// An impossible expectation on an otherwise-healthy lane counts as a
	// miss — the shape a real lane-killing regression produces (case runs,
	// lane returns nothing or wrong items, rank stays 0).
	cases := []GoldenCase{
		{Schema: GoldenCaseSchema, ID: "w1", Query: "Foo Topic", Lane: "wiki", ExpectIDs: []string{"entity:nonexistent"}},
	}
	rep := Run(ctx, Deps{DB: db, KB: ks, AgentID: "a1", Embedder: mockEmb{axis: 0}}, cases)
	ws := rep.ByLane["wiki"]
	if ws.Hit1 != 0 || ws.Hit5 != 0 || ws.MRR != 0 {
		t.Errorf("miss not counted: %+v", ws)
	}
}

// SaveGolden/LoadGolden roundtrip through a temp file.
func TestGoldenRoundTrip(t *testing.T) {
	cases := []GoldenCase{
		{ID: "a", Query: "q1", Lane: "wiki", ExpectIDs: []string{"entity:x"}},
		{ID: "b", Query: "q2", Lane: "any", Unanswerable: true},
	}
	path := t.TempDir() + "/golden.jsonl"
	if err := SaveGolden(path, cases); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadGolden(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 || got[0].ExpectIDs[0] != "entity:x" || !got[1].Unanswerable {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	// Wrong schema rejected.
	bad := t.TempDir() + "/bad.jsonl"
	if err := os.WriteFile(bad, []byte(`{"schema":"fluctio.golden.v0","id":"x","query":"q","lane":"wiki"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	if _, err := LoadGolden(bad); err == nil {
		t.Error("stale schema accepted")
	}
}
