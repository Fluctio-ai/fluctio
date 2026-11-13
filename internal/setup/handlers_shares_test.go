package setup

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/workspace"
)

// bootShareServer starts a route-test server with a LocalFS workspace store
// and one agent holding two files (an mp3 and an html).
func bootShareServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	wsRoot := t.TempDir()
	s, baseURL, token := bootRouteTestServer(t, func(s *Server) {
		s.SetWorkspaceStore(workspace.NewLocalFS(wsRoot))
	})
	ctx := context.Background()
	if err := s.dataStore.SaveAgent(ctx, &store.AgentRecord{ID: "agt_share", Name: "share"}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	ws := workspace.NewLocalFS(wsRoot)
	for name, content := range map[string]string{
		"sessions/s-1/news.mp3":   "id3-audio-bytes",
		"sessions/s-1/page.html":  "<html><body>hi</body></html>",
		"sessions/s-1/SKILL.md":   "# do not share me",
	} {
		if err := ws.Put(ctx, "agt_share", "", "", name, strings.NewReader(content), 0, ""); err != nil {
			t.Fatalf("put %s: %v", name, err)
		}
	}
	return s, baseURL, token
}

func TestFileShareRoutes(t *testing.T) {
	s, baseURL, token := bootShareServer(t)

	// Unauthenticated mint → 401.
	req, _ := http.NewRequest("POST", baseURL+"/api/agents/agt_share/share/sessions/s-1/news.mp3", strings.NewReader(`{}`))
	if resp, err := setupRouteTestHTTPClient.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated mint: %d, want 401", resp.StatusCode)
		}
	}

	// Mint a single-use, 1h link.
	code, out := routeDo(t, baseURL, token, "POST",
		"/api/agents/agt_share/share/sessions/s-1/news.mp3",
		strings.NewReader(`{"ttlHours":1,"maxUses":1}`))
	if code != 200 || out["ok"] != true {
		t.Fatalf("mint: code=%d body=%v", code, out)
	}
	shareToken, _ := out["token"].(string)
	if shareToken == "" || !strings.Contains(out["url"].(string), "/f/"+shareToken) {
		t.Fatalf("mint returned bad token/url: %v", out)
	}

	// SKILL.md is refused at mint time.
	if code, _ = routeDo(t, baseURL, token, "POST",
		"/api/agents/agt_share/share/sessions/s-1/SKILL.md", strings.NewReader(`{}`)); code != 403 {
		t.Fatalf("SKILL.md mint: %d, want 403", code)
	}
	// Missing file and missing agent both 404.
	if code, _ = routeDo(t, baseURL, token, "POST",
		"/api/agents/agt_share/share/sessions/s-1/nope.mp3", strings.NewReader(`{}`)); code != 404 {
		t.Fatalf("missing file mint: %d, want 404", code)
	}
	if code, _ = routeDo(t, baseURL, token, "POST",
		"/api/agents/missing/share/x.mp3", strings.NewReader(`{}`)); code != 404 {
		t.Fatalf("missing agent mint: %d, want 404", code)
	}

	// Anonymous fetch #1 succeeds with the right content and headers.
	resp, err := setupRouteTestHTTPClient.Get(baseURL + "/f/" + shareToken)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "id3-audio-bytes" {
		t.Fatalf("fetch #1: %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "audio/mpeg") {
		t.Fatalf("content-type: %q", ct)
	}
	// Anonymous fetch #2 → 410 (maxUses=1 consumed).
	if resp, err = setupRouteTestHTTPClient.Get(baseURL + "/f/" + shareToken); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("fetch #2: %d, want 410", resp.StatusCode)
	}
	// Unknown token → 404 (not 410 — tokens can't be probed).
	if resp, err = setupRouteTestHTTPClient.Get(baseURL + "/f/deadbeef"); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown token: %d, want 404", resp.StatusCode)
	}

	// HTML share keeps the sandbox CSP; ?download=1 sets the disposition.
	code, out = routeDo(t, baseURL, token, "POST",
		"/api/agents/agt_share/share/sessions/s-1/page.html", strings.NewReader(`{}`))
	if code != 200 {
		t.Fatalf("html mint: %d", code)
	}
	htmlToken, _ := out["token"].(string)
	if resp, err = setupRouteTestHTTPClient.Get(baseURL + "/f/" + htmlToken + "?download=1"); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "page.html") {
		t.Fatalf("download disposition: %q", cd)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Fatalf("html CSP missing: %q", csp)
	}

	// Listing shows both shares; revoke kills the link.
	code, out = routeDo(t, baseURL, token, "GET", "/api/agents/agt_share/shares", nil)
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	if shares, _ := out["shares"].([]any); len(shares) != 2 {
		t.Fatalf("list shares: %v", out)
	}
	if code, _ = routeDo(t, baseURL, token, "DELETE", "/api/share/"+htmlToken, nil); code != 200 {
		t.Fatalf("revoke: %d", code)
	}
	if resp, err = setupRouteTestHTTPClient.Get(baseURL + "/f/" + htmlToken); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revoked fetch: %d, want 404", resp.StatusCode)
	}

	// An expired share (written directly via the store) → 410.
	past := time.Now().Add(-time.Minute).UTC()
	if err := s.dataStore.CreateFileShare(context.Background(), &store.FileShareRecord{
		Token: "sharetest-expired-route", AgentID: "agt_share", Path: "sessions/s-1/news.mp3", ExpiresAt: &past,
	}); err != nil {
		t.Fatal(err)
	}
	if resp, err = setupRouteTestHTTPClient.Get(baseURL + "/f/sharetest-expired-route"); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("expired fetch: %d, want 410", resp.StatusCode)
	}
}
