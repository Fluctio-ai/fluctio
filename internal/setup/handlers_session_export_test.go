package setup

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/users"
)

// TestSessionExportRoute guards the raw-export route: unknown agent →
// 404 with the standard error envelope (the happy path is covered by
// store.ListSessionEventsSince tests; this pins routing + auth).
func TestSessionExportRoute(t *testing.T) {
	ctx := context.Background()
	t.Setenv("FLUCTIO_HOME", t.TempDir())

	s, _, adminUser, _ := newAuthTestServer(t, ctx)
	s.port = freeTCPPort(t)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(runCtx) }()
	baseURL := "http://127.0.0.1:" + strconv.Itoa(s.port)
	waitForSetupServer(t, baseURL, errCh)

	_, token, err := s.apikeys.Create(ctx, adminUser.ID, "export-test", users.APIKeyTypeAdmin, nil)
	if err != nil {
		t.Fatalf("create apikey: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet,
		baseURL+"/api/chat/sessions/some-key/export?agentId=missing", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := setupRouteTestHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["error"] != "agent not found" {
		t.Fatalf("body=%v", out)
	}
	_ = io.Discard
}
