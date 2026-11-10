package setup

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// TestDiaryShelvedSettleRoute covers the manual settlement endpoint's
// HTTP contract: status whitelist, settle-once semantics (repeat is a
// 200 no-op), the ledger dropping the row immediately, and unknown
// agents 404ing like the rest of the diary surface.
func TestDiaryShelvedSettleRoute(t *testing.T) {
	s, baseURL, token := bootRouteTestServer(t)
	ctx := context.Background()
	if err := s.dataStore.SaveAgent(ctx, &store.AgentRecord{ID: "agt_sh", Name: "shelved"}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	dbs := s.dataStore.(*store.DBStore)
	if err := dbs.UpsertShelvedCandidates(ctx, []store.ShelvedCandidate{{
		AgentID: "agt_sh", Kind: "deferred", Topic: "整理周报模板",
		SessionKey: "sess-1", Seq: 5, Since: "2026-11-09",
		LastSeenAt: time.Now(), Source: "summary",
	}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	open, err := dbs.ListOpenShelvedCandidates(ctx, "agt_sh", 20)
	if err != nil || len(open) != 1 {
		t.Fatalf("open: err=%v rows=%+v", err, open)
	}
	path := "/api/agents/agt_sh/diary/shelved/" + strconv.FormatInt(open[0].ID, 10)

	// Off-whitelist status → 400.
	if code, _ := routeDo(t, baseURL, token, "PATCH", path, strings.NewReader(`{"status":"nope"}`)); code != 400 {
		t.Fatalf("bad status: code=%d", code)
	}
	// Settle → 200 settled:true; /diary/shelved no longer lists it.
	code, body := routeDo(t, baseURL, token, "PATCH", path, strings.NewReader(`{"status":"dropped"}`))
	if code != 200 || body["settled"] != true {
		t.Fatalf("settle: code=%d body=%v", code, body)
	}
	if open, _ = dbs.ListOpenShelvedCandidates(ctx, "agt_sh", 20); len(open) != 0 {
		t.Fatalf("settled row must leave the open list: %+v", open)
	}
	// Repeat → 200 settled:false (idempotent, no error).
	if _, body = routeDo(t, baseURL, token, "PATCH", path, strings.NewReader(`{"status":"dropped"}`)); body["settled"] != false {
		t.Fatalf("repeat settle must be a no-op: %v", body)
	}
	// Unknown agent → 404.
	if code, _ = routeDo(t, baseURL, token, "PATCH", "/api/agents/agt_none/diary/shelved/1", strings.NewReader(`{"status":"dropped"}`)); code != 404 {
		t.Fatalf("unknown agent: code=%d", code)
	}
}
