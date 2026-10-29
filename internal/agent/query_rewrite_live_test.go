package agent

// Live test for the query rewriter — env-gated (FLUCTIO_LIVE_QR=1) so it
// never runs in CI. Uses the REAL provider credentials from the real
// ~/.fluctio DB (read-only; the key stays in memory and is never
// printed) and the isolated seeded DB at tmp/qr-home for the FTS corpus.
//
//	$env:FLUCTIO_LIVE_QR="1"; go test ./internal/agent -run TestQueryRewriteLive -v
//
// Prints raw → rewritten per case and the FTS hit count raw vs
// rewritten. No secrets in output.
import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	// No modernc import needed here: the store package (imported below
	// for the FTS corpus) registers the sqlite driver.
	"github.com/fluctio-ai/fluctio/internal/agent/tools"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/store"
)

func liveProvider(t *testing.T) provider.Provider {
	t.Helper()
	db, err := sql.Open("sqlite", os.Getenv("USERPROFILE")+`\.fluctio\fluctio.db?mode=ro`)
	if err != nil {
		t.Skipf("open real db: %v", err)
	}
	defer db.Close()
	var data string
	if err := db.QueryRow(`SELECT data FROM configs WHERE kind='provider' AND scope='system' AND name='z.ai'`).Scan(&data); err != nil {
		t.Skipf("z.ai provider row: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		t.Fatalf("parse provider data: %v", err)
	}
	get := func(k string) string { s, _ := m[k].(string); return s }
	key, base := get("apiKey"), get("apiBase")
	apiType := get("apiType")
	if key == "" || base == "" {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		t.Fatalf("provider data missing apiKey/apiBase; available fields: %v", keys)
	}
	return provider.NewProvider(key, base, apiType)
}

func TestQueryRewriteLive(t *testing.T) {
	if os.Getenv("FLUCTIO_LIVE_QR") != "1" {
		t.Skip("live test; set FLUCTIO_LIVE_QR=1")
	}
	prov := liveProvider(t)
	model := "glm-5.3"

	cases := []struct {
		name string
		msgs []provider.Message
		raw  string
	}{
		{
			name: "pronoun+ellipsis",
			msgs: []provider.Message{
				{Role: "user", Content: "我们计划把线上服务从 Docker Compose 迁移到 Kubernetes,分三个阶段,先迁移无状态服务。"},
				{Role: "assistant", Content: "好的,三阶段迁移是稳妥的做法。"},
			},
			raw: "那个迁移现在到哪个阶段了?",
		},
		{
			name: "filler+reference",
			msgs: []provider.Message{
				{Role: "user", Content: "上次聊到我想画一张小毛驴飞天的图"},
				{Role: "assistant", Content: "飞天小毛驴,可以参考敦煌飞天的飘带造型。"},
			},
			raw: "帮我把上次说的那个图发我一下呗",
		},
		{
			name: "already-standalone",
			msgs: nil,
			raw:  "芃芃最近学习怎么样",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// FTS corpus: the isolated seeded DB (3 summaries: K8s migration /
	// weekly report / camping). Skip the hit-count comparison when it
	// isn't present.
	var ftsDB *store.DBStore
	if abs, err := filepath.Abs(`..\..\tmp\qr-home\fluctio.db`); err == nil {
		if _, serr := os.Stat(abs); serr == nil {
			ftsDB, err = store.NewDBStore("sqlite", abs)
			if err != nil {
				t.Logf("open seeded db: %v (skipping FTS comparison)", err)
			}
		}
	}
	const seedAgent = "agt_27df76a3677a60d59a1c"

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rewritten := rewriteQuery(ctx, prov, model, tc.msgs, tc.raw)
			t.Logf("raw       = %s", tc.raw)
			t.Logf("rewritten = %s", rewritten)
			if ftsDB != nil {
				ctxF, cancelF := context.WithTimeout(ctx, 10*time.Second)
				rawHits, rawErr := tools.SemanticMemRecall(ctxF, ftsDB, nil, nil, seedAgent, "", tc.raw, 3, nil, false)
				rwHits, rwErr := tools.SemanticMemRecall(ctxF, ftsDB, nil, nil, seedAgent, "", rewritten, 3, nil, false)
				cancelF()
				t.Logf("fts hits: raw=%d (err=%v) rewritten=%d (err=%v)",
					len(rawHits), rawErr, len(rwHits), rwErr)
				for _, h := range rwHits {
					t.Logf("  rewritten hit: topic=%s", h.Topic)
				}
			}
		})
	}
}
