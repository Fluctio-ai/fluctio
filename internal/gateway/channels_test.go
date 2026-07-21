package gateway

import (
	"testing"

	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/channels"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// TestRegisterQQChannelsUseMarkdownWiring pins down the Phase 4 wiring
// contract: AccountConfig.UseMarkdown on the configs row must propagate
// to the registered *QQChannel.useMarkdown. Without the SetUseMarkdown
// call after NewQQChannel the dashboard toggle is silently ignored
// (qq_ws.go defaults useMarkdown to false). The setter itself is
// exercised in channels.TestQQSetUseMarkdown; this test covers the
// gateway-side call site end-to-end (ConfigRecord → Manager.Get).
func TestRegisterQQChannelsUseMarkdownWiring(t *testing.T) {
	// Stub the agent-config loader so the allowed-checker closure isn't
	// wired (no agent row → skip). Restore on exit.
	old := config.AgentFileConfigLoader
	config.AgentFileConfigLoader = func(_, _ string) (config.AgentFileConfig, bool) {
		return config.AgentFileConfig{}, false
	}
	t.Cleanup(func() { config.AgentFileConfigLoader = old })

	mb := bus.New()
	mgr := channels.NewManager(mb)

	chCfg := config.ChannelConfig{
		Enabled: true,
		Accounts: map[string]config.AccountConfig{
			"acct-md-on": {
				AppID:        "app-md-on",
				ClientSecret: "secret-md-on",
				UseMarkdown:  true,
			},
			"acct-md-off": {
				AppID:        "app-md-off",
				ClientSecret: "secret-md-off",
				UseMarkdown:  false,
			},
		},
	}

	// rec.AgentID empty on purpose — skips SetAllowedChecker. st=nil
	// skips OnFailed wiring; both are unrelated to the UseMarkdown path.
	if err := registerQQChannels(store.ConfigRecord{}, chCfg, mb, mgr, nil, false); err != nil {
		t.Fatalf("registerQQChannels: %v", err)
	}

	on := mgr.Get("qq", "acct-md-on")
	if on == nil {
		t.Fatalf("Manager.Get(qq, acct-md-on) returned nil")
	}
	qOn, ok := on.(*channels.QQChannel)
	if !ok {
		t.Fatalf("acct-md-on: want *channels.QQChannel, got %T", on)
	}
	if !qOn.UseMarkdown() {
		t.Errorf("acct-md-on: UseMarkdown() = false, want true (AccountConfig.UseMarkdown=true must propagate)")
	}

	off := mgr.Get("qq", "acct-md-off")
	if off == nil {
		t.Fatalf("Manager.Get(qq, acct-md-off) returned nil")
	}
	qOff, ok := off.(*channels.QQChannel)
	if !ok {
		t.Fatalf("acct-md-off: want *channels.QQChannel, got %T", off)
	}
	if qOff.UseMarkdown() {
		t.Errorf("acct-md-off: UseMarkdown() = true, want false (AccountConfig.UseMarkdown=false)")
	}
}
