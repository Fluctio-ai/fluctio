package agent

import (
	"strings"
	"testing"
)

func TestModRuntimeContext(t *testing.T) {
	cb := &ContextBuilder{
		workspace:        "/tmp/ws",
		mcpServerSummary: "playwright (cwd: /tmp/ws/.playwright-mcp)",
		sandboxEnabled:   false,
	}
	p := &promptCtx{cb: cb, mode: "agent"}
	got := modRuntimeContext(p)

	mustContain := []string{
		"可见域",        // visible workspace
		"deliver_file", // 投递工具提示
		"playwright",   // MCP server 名
	}
	for _, s := range mustContain {
		if !strings.Contains(got, s) {
			t.Fatalf("modRuntimeContext missing %q:\n%s", s, got)
		}
	}
}
