package setup

import (
	"context"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// TestListConfigsByScopeUserDoesNotLeakSystem reproduces a single-user
// flatten wiring bug: listConfigsByScope mapped both (user, userID) and
// (system, "") to ListConfigs(ctx, kind, "") — i.e. agentID="" — so a
// user-scope list returned system-global rows. The frontend agent Models
// page merges the user + system lists, which made every system provider
// render twice in the table.
func TestListConfigsByScopeUserDoesNotLeakSystem(t *testing.T) {
	ctx := context.Background()
	s, _, _, _ := newAuthTestServer(t, ctx)

	// Seed a system-global provider (UserID="" AgentID="" -> scope_id="").
	if err := s.dataStore.SaveConfig(ctx, &store.ConfigRecord{
		Kind:    store.KindProvider,
		Name:    "longcat",
		Enabled: true,
		Data:    map[string]any{"apiBase": "https://example/v1"},
	}); err != nil {
		t.Fatalf("SaveConfig system provider: %v", err)
	}

	// A user-scope list must not fall through to system-global rows.
	got, err := s.listConfigsByScope(ctx, store.KindProvider, scope.User, "user-xyz")
	if err != nil {
		t.Fatalf("listConfigsByScope user: %v", err)
	}
	for _, r := range got {
		if r.Name == "longcat" {
			t.Fatalf("user-scope list leaked system provider %q (rows=%v); user scope must not return system-global rows", r.Name, got)
		}
	}
}
