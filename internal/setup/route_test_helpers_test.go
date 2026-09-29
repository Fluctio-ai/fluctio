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

// bootRouteTestServer starts a real sqlite-backed setup server on a
// free port and returns it with the base URL and a throwaway
// admin-scoped API token. The shared tail of the route tests:
// FLUCTIO_HOME redirect, newAuthTestServer, optional configure hook
// (runs before the server does, for wiring like SetMaintenance), Run in
// a goroutine, readiness wait, one admin key per test.
func bootRouteTestServer(t *testing.T, configure ...func(*Server)) (*Server, string, string) {
	t.Helper()
	ctx := context.Background()
	t.Setenv("FLUCTIO_HOME", t.TempDir())
	s, _, adminUser, _ := newAuthTestServer(t, ctx)
	for _, f := range configure {
		f(s)
	}
	s.port = freeTCPPort(t)
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(runCtx) }()
	baseURL := "http://127.0.0.1:" + strconv.Itoa(s.port)
	waitForSetupServer(t, baseURL, errCh)
	_, token, err := s.apikeys.Create(ctx, adminUser.ID, "route-test-"+t.Name(), users.APIKeyTypeAdmin, nil)
	if err != nil {
		t.Fatalf("create apikey: %v", err)
	}
	return s, baseURL, token
}

// routeDo issues one authorized JSON request and decodes the response
// body into a map. Returns the status code and the decoded body.
func routeDo(t *testing.T, baseURL, token, method, path string, body io.Reader) (int, map[string]any) {
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
