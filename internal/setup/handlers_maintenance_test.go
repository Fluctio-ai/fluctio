package setup

import (
	"net/http"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/maintenance"
)

// TestMaintenanceRoutes walks the /api/maintenance surface against a
// real sqlite-backed setup server: status reflects the wired
// coordinator (with db stats), vacuum accepts and runs to done on a
// small db, and a second concurrent trigger gets 409.
func TestMaintenanceRoutes(t *testing.T) {
	_, baseURL, token := bootRouteTestServer(t, func(s *Server) {
		s.SetMaintenance(maintenance.New(s.dataStore, nil))
	})

	// Status before any run: idle + db stats present.
	code, out := routeDo(t, baseURL, token, "GET", "/api/maintenance/status", nil)
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
	if code, out = routeDo(t, baseURL, token, "POST", "/api/maintenance/vacuum", nil); code != http.StatusAccepted {
		t.Fatalf("vacuum code=%d out=%v", code, out)
	}
	if code, _ = routeDo(t, baseURL, token, "POST", "/api/maintenance/vacuum", nil); code != http.StatusConflict {
		t.Fatalf("concurrent vacuum code=%d, want 409", code)
	}

	// Poll until done (small temp db: seconds at most).
	deadline := time.Now().Add(30 * time.Second)
	stage := ""
	for time.Now().Before(deadline) {
		_, out := routeDo(t, baseURL, token, "GET", "/api/maintenance/status", nil)
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
	_, baseURL, token := bootRouteTestServer(t)

	code, out := routeDo(t, baseURL, token, "GET", "/api/maintenance/status", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d, want 503", code)
	}
	if out["error"] != "maintenance not wired" {
		t.Fatalf("body=%v", out)
	}
}
