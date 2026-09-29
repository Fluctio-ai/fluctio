package setup

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/maintenance"
	"github.com/fluctio-ai/fluctio/internal/users"
)

// TestMaintenanceRoutes walks the /api/maintenance surface against a
// real sqlite-backed setup server: status reflects the wired
// coordinator (with db stats), vacuum accepts and runs to done on a
// small db, and a second concurrent trigger gets 409.
func TestMaintenanceRoutes(t *testing.T) {
	ctx := context.Background()
	t.Setenv("FLUCTIO_HOME", t.TempDir()) // backup snapshots land here

	s, _, adminUser, _ := newAuthTestServer(t, ctx)
	s.port = freeTCPPort(t)
	st := s.dataStore
	s.SetMaintenance(maintenance.New(st, nil))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(runCtx) }()
	baseURL := "http://127.0.0.1:" + strconv.Itoa(s.port)
	waitForSetupServer(t, baseURL, errCh)

	_, token, err := s.apikeys.Create(ctx, adminUser.ID, "maint-test", users.APIKeyTypeAdmin, nil)
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

	// Status before any run: idle + db stats present.
	code, out := do("GET", "/api/maintenance/status", nil)
	if code != http.StatusOK {
		t.Fatalf("status code=%d out=%v", code, out)
	}
	m, _ := out["maintenance"].(map[string]any)
	if m == nil || m["stage"] != "idle" {
		t.Fatalf("want idle maintenance, got %v", m)
	}
	db, _ := out["db"].(map[string]any)
	if db == nil || db["dialect"] != "sqlite" {
		t.Fatalf("want sqlite db stats, got %v", db)
	}

	// Kick off a run, then immediately re-trigger: 409 while in flight.
	if code, out = do("POST", "/api/maintenance/vacuum", nil); code != http.StatusAccepted {
		t.Fatalf("vacuum code=%d out=%v", code, out)
	}
	if code, _ = do("POST", "/api/maintenance/vacuum", nil); code != http.StatusConflict {
		t.Fatalf("concurrent vacuum code=%d, want 409", code)
	}

	// Poll until done (small temp db: seconds at most).
	deadline := time.Now().Add(30 * time.Second)
	stage := ""
	for time.Now().Before(deadline) {
		_, out := do("GET", "/api/maintenance/status", nil)
		if m, _ = out["maintenance"].(map[string]any); m != nil {
			stage, _ = m["stage"].(string)
			if stage == "done" || stage == "failed" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if stage != "done" {
		t.Fatalf("stage=%s maint=%v", stage, m)
	}
	if name, _ := m["backupName"].(string); name == "" {
		t.Fatal("backupName empty after done")
	}
}

// TestMaintenanceUnwired: no coordinator wired → 503, not a panic.
func TestMaintenanceUnwired(t *testing.T) {
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

	_, token, err := s.apikeys.Create(ctx, adminUser.ID, "maint-unwired", users.APIKeyTypeAdmin, nil)
	if err != nil {
		t.Fatalf("create apikey: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/api/maintenance/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := setupRouteTestHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("code=%d, want 503", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if !strings.Contains(out["error"].(string), "not wired") {
		t.Fatalf("body=%v", out)
	}
}
