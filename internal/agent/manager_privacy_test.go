package agent

import (
	"context"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// nopProvider satisfies provider.Provider without network.
type nopProvider struct{}

func (nopProvider) Chat(ctx context.Context, messages []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.Response, error) {
	return nil, nil
}

func (nopProvider) ChatStream(ctx context.Context, messages []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.StreamReader, error) {
	return nil, nil
}

// TestBuildAgentAppliesPrivacyScrubCfg guards the manager-side privacy
// stamp: the production path (NewAgentWithSkillsCfg) never loads
// fullCfg.Privacy, so before the stamp existed the UI toggle never
// reached the loop's scrub points (dead wiring — DB row said enabled,
// agent field stayed false).
func TestBuildAgentAppliesPrivacyScrubCfg(t *testing.T) {
	st, err := store.NewDBStore("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.DB().Close() })

	const uid = "user-pii"
	const agID = "agt-privacy-test"
	if err := scope.SaveSetting(context.Background(), st, "", agID, "privacy",
		map[string]interface{}{
			"piiScrubbing": map[string]interface{}{"enabled": true, "entropy": true},
		}); err != nil {
		t.Fatalf("save privacy row: %v", err)
	}

	rc := config.ResolvedAgent{ID: agID, UserID: uid, DisplayName: "pii-test", Home: t.TempDir(), Model: "test-model"}
	mgr, err := NewManager([]config.ResolvedAgent{rc}, nopProvider{}, bus.New(), WithUserID(uid), WithDataStore(st))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ag := mgr.AgentByID(agID)
	if ag == nil {
		t.Fatal("agent not built")
	}
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
	st, err := store.NewDBStore("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.DB().Close() })

	const uid = "user-pii-off"
	const agID = "agt-privacy-off"
	rc := config.ResolvedAgent{ID: agID, UserID: uid, DisplayName: "pii-off", Home: t.TempDir(), Model: "test-model"}
	mgr, err := NewManager([]config.ResolvedAgent{rc}, nopProvider{}, bus.New(), WithUserID(uid), WithDataStore(st))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ag := mgr.AgentByID(agID)
	if ag == nil {
		t.Fatal("agent not built")
	}
	if ag.piiScrubEnabled {
		t.Fatal("missing privacy row must leave scrubbing off")
	}
	if ag.bgProvider() != ag.provider {
		t.Fatal("bgProvider must return the raw provider when scrubbing is off")
	}
}
