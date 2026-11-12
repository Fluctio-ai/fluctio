package agent

import (
	"context"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// newPrivacyTestAgent builds one agent through the production manager
// path. savePrivacyRow seeds the agent-scope privacy config row before
// the build. The provider is the package's inert test double
// (rewriteMockProvider); the manager tests never call Chat/ChatStream.
func newPrivacyTestAgent(t *testing.T, uid, agID string, savePrivacyRow bool) *Agent {
	t.Helper()
	st, err := store.NewDBStore("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = st.DB().Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if savePrivacyRow {
		if err := scope.SaveSetting(context.Background(), st, "", agID, "privacy",
			map[string]interface{}{
				"piiScrubbing": map[string]interface{}{"enabled": true, "entropy": true},
			}); err != nil {
			t.Fatalf("save privacy row: %v", err)
		}
	}

	rc := config.ResolvedAgent{ID: agID, UserID: uid, DisplayName: "pii-test", Home: t.TempDir(), Model: "test-model"}
	mgr, err := NewManager([]config.ResolvedAgent{rc}, &rewriteMockProvider{}, bus.New(), WithUserID(uid), WithDataStore(st))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ag := mgr.AgentByID(agID)
	if ag == nil {
		t.Fatal("agent not built")
	}
	return ag
}

// TestBuildAgentAppliesPrivacyScrubCfg guards the manager-side privacy
// stamp: the manager build path never loads cfg.Privacy, so without
// the stamp the UI toggle never reached the loop's scrub points (dead
// wiring — DB row said enabled, agent field stayed false).
func TestBuildAgentAppliesPrivacyScrubCfg(t *testing.T) {
	ag := newPrivacyTestAgent(t, "user-pii", "agt-privacy-test", true)
	if !ag.piiScrubEnabled || !ag.piiEntropyEnabled {
		t.Fatalf("privacy cfg not stamped: scrub=%v entropy=%v", ag.piiScrubEnabled, ag.piiEntropyEnabled)
	}
	// Enabled → bgProvider must actually wrap; this is what KB insights
	// and other background pipelines consume.
	if ag.bgProvider() == ag.provider {
		t.Fatal("bgProvider must wrap the raw provider when scrubbing is enabled")
	}
}

// TestBuildAgentPrivacyOffByDefault: missing privacy row → scrub stays
// off (missing row = off contract in privacy.ScopedProvider).
func TestBuildAgentPrivacyOffByDefault(t *testing.T) {
	ag := newPrivacyTestAgent(t, "user-pii-off", "agt-privacy-off", false)
	if ag.piiScrubEnabled {
		t.Fatal("missing privacy row must leave scrubbing off")
	}
	if ag.bgProvider() != ag.provider {
		t.Fatal("bgProvider must return the raw provider when scrubbing is off")
	}
}
