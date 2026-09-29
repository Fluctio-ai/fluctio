package setup

import (
	"net/http"
	"testing"
)

// TestSessionExportRoute guards the raw-export route: unknown agent →
// 404 with the standard error envelope (the happy path is covered by
// store.ListSessionEventsSince tests; this pins routing + auth).
func TestSessionExportRoute(t *testing.T) {
	_, baseURL, token := bootRouteTestServer(t)

	code, out := routeDo(t, baseURL, token, "GET",
		"/api/chat/sessions/some-key/export?agentId=missing", nil)
	if code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", code)
	}
	if out["error"] != "agent not found" {
		t.Fatalf("body=%v", out)
	}
}
