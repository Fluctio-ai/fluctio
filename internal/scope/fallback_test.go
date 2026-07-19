package scope

import (
	"context"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// Verifies the agent → system precedence post single-user flatten: the
// user / per-(user, agent) layers are retired (single owner identity),
// so an agent-scope override wins over the system default and clearing
// it falls back to system. Pins the contract so a future refactor
// surfaces a regression instead of silently flipping precedence.
func TestSettingPrecedence_AgentBeatsSystem(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	agentID := "agent-x"

	// system: default model
	if err := SaveSetting(ctx, db, "", "", "agents.defaults",
		map[string]interface{}{"model": "deepseek/deepseek-v4-pro"}); err != nil {
		t.Fatalf("save system: %v", err)
	}

	// System default wins on its own.
	var got config.AgentDefaults
	if err := SettingInto(ctx, db, "agents.defaults", "", agentID, &got); err != nil {
		t.Fatalf("setting into: %v", err)
	}
	if got.Model != "deepseek/deepseek-v4-pro" {
		t.Fatalf("system default: want deepseek/deepseek-v4-pro, got %q", got.Model)
	}

	// agent-scope override on top
	if err := SaveSetting(ctx, db, "", agentID, "agents.defaults",
		map[string]interface{}{"model": "anthropic/claude-opus-4-7"}); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	got = config.AgentDefaults{}
	if err := SettingInto(ctx, db, "agents.defaults", "", agentID, &got); err != nil {
		t.Fatalf("setting into: %v", err)
	}
	if got.Model != "anthropic/claude-opus-4-7" {
		t.Fatalf("agent should beat system: want anthropic/claude-opus-4-7, got %q", got.Model)
	}

	// Verify the raw agent-scope row reads what we wrote, independent of
	// the merge — the loadUserSpace overlay path reads this directly.
	rec, err := db.GetConfigByName(ctx, store.KindSetting, agentID, "agents.defaults")
	if err != nil {
		t.Fatalf("get agent-scope row: %v", err)
	}
	if rec == nil {
		t.Fatal("agent-scope row missing after save")
	}
	if v, _ := rec.Data["model"].(string); v != "anthropic/claude-opus-4-7" {
		t.Fatalf("agent-scope row model: want anthropic/claude-opus-4-7, got %q", v)
	}

	// Delete agent-scope (empty data) → falls back to system.
	if err := SaveSetting(ctx, db, "", agentID, "agents.defaults", nil); err != nil {
		t.Fatalf("delete agent-scope: %v", err)
	}
	got = config.AgentDefaults{}
	if err := SettingInto(ctx, db, "agents.defaults", "", agentID, &got); err != nil {
		t.Fatalf("setting into after delete: %v", err)
	}
	if got.Model != "deepseek/deepseek-v4-pro" {
		t.Fatalf("clear agent should fall back to system: want deepseek/deepseek-v4-pro, got %q", got.Model)
	}
}
