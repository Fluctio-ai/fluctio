package cardsgen

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/wiki"
)

// stubProvider fakes provider.Provider: Chat returns a canned JSON body.
type stubProvider struct{ resp string }

func (p *stubProvider) Chat(ctx context.Context, messages []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.Response, error) {
	return &provider.Response{Content: p.resp}, nil
}

func (p *stubProvider) ChatStream(ctx context.Context, messages []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.StreamReader, error) {
	return nil, errors.New("not implemented")
}

// openStore builds a fully-migrated in-memory DBStore — exercises the real
// migration chain (kb_cards + kb_card_gen_runs included).
func openStore(t *testing.T) *store.DBStore {
	t.Helper()
	dbs, err := store.NewDBStore("sqlite", "file::memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := dbs.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return dbs
}

const genDate = "2026-08-17"

func seedDay(t *testing.T, dbs *store.DBStore) {
	t.Helper()
	ctx := context.Background()
	day, _ := time.ParseInLocation("2006-01-02", genDate, cst)
	err := dbs.InsertDailyDiary(ctx, store.DailyDiary{
		AgentID: "agt_gen",
		Date:    genDate,
		Themes: []store.DiaryTheme{{
			Title:   "前缀缓存优化",
			Summary: "讨论了系统提示词固定+tools 排序使前缀缓存命中率达到 99%。",
			Points:  []string{"get_time 工具替代时间戳硬编码"},
		}},
	})
	if err != nil {
		t.Fatalf("seed diary: %v", err)
	}
	// Seed one wiki page updated mid-window (raw SQL: UpsertPage stamps
	// updated_at=now, which would fall outside yesterday's window).
	if _, err := dbs.DB().ExecContext(ctx,
		`INSERT INTO wiki_pages (id, agent_id, page_type, slug, title, body, summary, source_ids, tags, created_at, updated_at, revision)
		 VALUES ('entity:prefix-cache','agt_gen','entity','prefix-cache','前缀缓存','body','KV 缓存复用降低首字延迟','[]','[]',?,?,1)`,
		day.Add(2*time.Hour), day.Add(12*time.Hour)); err != nil {
		t.Fatalf("seed wiki page: %v", err)
	}
}

// TestRunGeneratesCards walks the happy path: diary + wiki material in the
// window → LLM candidates → cards saved with correct source mapping → run
// stamped. A second pass with the same output dedups to zero.
func TestRunGeneratesCards(t *testing.T) {
	dbs := openStore(t)
	defer dbs.Close()
	seedDay(t, dbs)
	ctx := context.Background()

	ks := kb.NewKBStore(dbs.DB(), dbs.Dialect())
	ws := wiki.NewWikiStore(dbs.DB(), dbs.Dialect())
	prov := &stubProvider{resp: `{"cards":[
		{"question":"如何让前缀缓存命中率达到 99%？","answer":"系统提示词固定 + tools 按稳定顺序排序，时间信息交给 get_time 工具。","source_index":0,"excerpt":"tools 排序"},
		{"question":"前缀缓存的作用是什么？","answer":"复用请求公共前缀的 KV 缓存，降低首字延迟。","source_index":1,"excerpt":"KV 缓存复用"}
	]}`}

	created, err := Run(ctx, dbs, ks, ws, "agt_gen", genDate, prov, "stub-model", 10)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if created != 2 {
		t.Fatalf("created=%d want 2", created)
	}
	cards, _ := ks.ListCards(ctx, "agt_gen", "all", "", "", 50, 0)
	if len(cards) != 2 {
		t.Fatalf("stored cards=%d want 2", len(cards))
	}
	for _, c := range cards {
		switch c.SourceType {
		case "diary":
			if c.SourceRef != genDate {
				t.Errorf("diary card ref=%q want %q", c.SourceRef, genDate)
			}
		case "wiki":
			if c.SourceRef != "entity:prefix-cache" {
				t.Errorf("wiki card ref=%q want entity:prefix-cache", c.SourceRef)
			}
		default:
			t.Errorf("unexpected source_type %q", c.SourceType)
		}
	}
	if !HasRunFor(ctx, dbs, "agt_gen", genDate) {
		t.Fatalf("run not stamped")
	}

	// Second pass with identical output: everything dedups away.
	created, err = Run(ctx, dbs, ks, ws, "agt_gen", genDate, prov, "stub-model", 10)
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if created != 0 {
		t.Fatalf("rerun created=%d want 0 (dedup)", created)
	}
}

// TestRunLimitAndEmptyMaterial checks the daily cap and the no-material
// short circuit (still stamps the run so the sweep stays idempotent).
func TestRunLimitAndEmptyMaterial(t *testing.T) {
	dbs := openStore(t)
	defer dbs.Close()
	ctx := context.Background()
	ks := kb.NewKBStore(dbs.DB(), dbs.Dialect())
	ws := wiki.NewWikiStore(dbs.DB(), dbs.Dialect())

	// No material for this date → 0 created, run stamped.
	prov := &stubProvider{resp: `{"cards":[]}`}
	created, err := Run(ctx, dbs, ks, ws, "agt_gen", "2030-01-01", prov, "m", 5)
	if err != nil || created != 0 {
		t.Fatalf("empty material: created=%d err=%v", created, err)
	}
	if !HasRunFor(ctx, dbs, "agt_gen", "2030-01-01") {
		t.Fatalf("empty-material run not stamped")
	}

	// Daily limit caps creation even when the LLM over-produces.
	seedDay(t, dbs)
	prov = &stubProvider{resp: `{"cards":[
		{"question":"Q1？","answer":"A1","source_index":0,"excerpt":""},
		{"question":"Q2？","answer":"A2","source_index":0,"excerpt":""},
		{"question":"Q3？","answer":"A3","source_index":0,"excerpt":""}
	]}`}
	created, err = Run(ctx, dbs, ks, ws, "agt_gen", genDate, prov, "m", 2)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if created != 2 {
		t.Fatalf("limit: created=%d want 2", created)
	}
}
