package setup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/agent"
	"github.com/fluctio-ai/fluctio/internal/api"
	"github.com/fluctio-ai/fluctio/internal/auth"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// spyResolver satisfies api.UserResolver minimally and records the
// invalidation calls the privacy PUT handlers must make so a toggle
// takes effect without a daemon restart.
type spyResolver struct {
	invalidateID string
	reloads      int
}

func (s *spyResolver) UserSpaceFor(string) (*api.UserSpaceView, error) { return nil, nil }
func (s *spyResolver) LocalAgentManager() *agent.Manager               { return nil }
func (s *spyResolver) IsCloudMode() bool                               { return false }
func (s *spyResolver) InvalidateAgent(id string)                       { s.invalidateID = id }
func (s *spyResolver) ReloadAgents() error                             { s.reloads++; return nil }

func newPrivacyTestServer(t *testing.T) (*Server, *spyResolver) {
	t.Helper()
	dbs, err := store.NewDBStore("sqlite", "file::memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = dbs.Close() })
	if err := dbs.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := dbs.CreateUser(context.Background(), &store.UserRecord{ID: "u1", Username: "owner", Email: "o@x", Role: "super_admin"}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := dbs.SaveAgent(context.Background(), &store.AgentRecord{ID: "agt_p", Name: "p"}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	res := &spyResolver{}
	return &Server{dataStore: dbs, userResolver: res}, res
}

// The scrub gates are stamped at agent build time; a privacy toggle
// without invalidation silently waits for the next restart.
func TestUpdateAgentPrivacyInvalidatesAgent(t *testing.T) {
	s, res := newPrivacyTestServer(t)
	r := httptest.NewRequest(http.MethodPut, "/api/agents/agt_p/privacy",
		strings.NewReader(`{"privacy":{"piiScrubbing":{"enabled":true}}}`))
	r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", Role: "super_admin", AuthMethod: "session"}))
	r.SetPathValue("id", "agt_p")
	w := httptest.NewRecorder()
	s.handleUpdateAgentPrivacy(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if res.invalidateID != "agt_p" {
		t.Fatalf("agent not invalidated: %q", res.invalidateID)
	}
}

// The system row shadows every agent, so flipping it must rebuild all
// cached UserSpaces (via invalidateScope, the same entry point the
// other settings writers use), not just one.
func TestUpdateSystemPrivacyReloadsAllAgents(t *testing.T) {
	s, res := newPrivacyTestServer(t)
	r := httptest.NewRequest(http.MethodPut, "/api/privacy",
		strings.NewReader(`{"privacy":{"piiScrubbing":{"enabled":true}}}`))
	r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", Role: "super_admin", AuthMethod: "session"}))
	w := httptest.NewRecorder()
	s.handleUpdateSystemPrivacy(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if res.reloads != 1 {
		t.Fatalf("want 1 ReloadAgents call, got %d", res.reloads)
	}
}

// No resolver wired (misconfigured route / partial deployment): the
// write must still land and the handler must not panic.
func TestUpdateAgentPrivacyWithoutResolverStillSaves(t *testing.T) {
	s, _ := newPrivacyTestServer(t)
	s.userResolver = nil
	r := httptest.NewRequest(http.MethodPut, "/api/agents/agt_p/privacy",
		strings.NewReader(`{"privacy":{"piiScrubbing":{"enabled":true}}}`))
	r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", Role: "super_admin", AuthMethod: "session"}))
	r.SetPathValue("id", "agt_p")
	w := httptest.NewRecorder()
	s.handleUpdateAgentPrivacy(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
}
