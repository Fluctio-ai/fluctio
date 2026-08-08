package setup

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/users"
)

// TestBackupRoutes walks the full /api/backup surface against a real
// sqlite-backed setup server: read default config, enable, verify the
// write round-trips, trigger a manual snapshot, list it, delete it.
// Snapshots land under a temp FLUCTIO_HOME so the real ~/.fluctio is
// never touched.
func TestBackupRoutes(t *testing.T) {
	ctx := context.Background()
	// Redirect backup.Dir() writes away from the real ~/.fluctio/backups.
	t.Setenv("FLUCTIO_HOME", t.TempDir())

	s, _, adminUser, _ := newAuthTestServer(t, ctx)
	s.port = freeTCPPort(t)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(runCtx) }()
	baseURL := "http://127.0.0.1:" + strconv.Itoa(s.port)
	waitForSetupServer(t, baseURL, errCh)

	_, token, err := s.apikeys.Create(ctx, adminUser.ID, "backup-test", users.APIKeyTypeAdmin, nil)
	if err != nil {
		t.Fatalf("create apikey: %v", err)
	}

	do := func(method, path string, body io.Reader) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequest(method, baseURL+path, body)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := setupRouteTestHTTPClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// GET default config (zero-value BackupCfg).
	if code, _ := do("GET", "/api/backup", nil); code != http.StatusOK {
		t.Fatalf("GET /api/backup status=%d", code)
	}

	// PUT enable.
	if code, _ := do("PUT", "/api/backup", strings.NewReader(`{"backup":{"enabled":true,"cronTime":"03:00","maxKeep":5}}`)); code != http.StatusOK {
		t.Fatalf("PUT /api/backup status=%d", code)
	}

	// GET reflects the write.
	_, body := do("GET", "/api/backup", nil)
	bk, _ := body["backup"].(map[string]any)
	if bk == nil || bk["enabled"] != true {
		t.Fatalf("GET after PUT: backup=%v, want enabled=true", body["backup"])
	}

	// POST now creates a snapshot.
	code, body := do("POST", "/api/backup/now", nil)
	if code != http.StatusOK {
		t.Fatalf("POST /api/backup/now status=%d body=%v", code, body)
	}
	name, _ := body["name"].(string)
	if name == "" {
		t.Fatalf("POST now: no name in response %v", body)
	}

	// GET list shows exactly one.
	_, body = do("GET", "/api/backup/list", nil)
	items, _ := body["backups"].([]any)
	if len(items) != 1 {
		t.Fatalf("list len=%d, want 1 (body=%v)", len(items), body)
	}

	// DELETE removes it, leaving none.
	if code, _ := do("DELETE", "/api/backup?file="+name, nil); code != http.StatusOK {
		t.Fatalf("DELETE status=%d", code)
	}
	_, body = do("GET", "/api/backup/list", nil)
	items, _ = body["backups"].([]any)
	if len(items) != 0 {
		t.Fatalf("after delete list len=%d, want 0", len(items))
	}
}
