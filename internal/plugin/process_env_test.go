package plugin

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Audit special-3 guard: plugin subprocesses must not inherit host
// credentials — Process.Start must install the same scrubbed whitelist
// the MCP stdio transport uses (mcp.ScrubbedEnv), instead of Go's default
// full os.Environ() inheritance. Before the fix cmd.Env was never set,
// so every plugin child saw the gateway's DSN, provider keys, tokens.
func TestProcessStartScrubsChildEnv(t *testing.T) {
	t.Setenv("PLUGIN_TEST_SECRET_TOKEN", "leaky-value")
	t.Setenv("FLUCTIO_STORAGE_DSN", "secret-dsn")

	var command string
	switch runtime.GOOS {
	case "windows":
		// strings.Fields splits the command line, so no arg may contain
		// spaces; this token sequence is a valid single sleep command.
		command = "powershell -NoProfile -NonInteractive -Command Start-Sleep -Seconds 15"
	default:
		command = "sleep 15"
	}
	p := NewProcess(&Manifest{ID: "envtest", Command: command})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(2 * time.Second)

	if p.cmd.Env == nil {
		t.Fatal("cmd.Env is nil — child inherits the full parent environment")
	}
	for _, kv := range p.cmd.Env {
		if strings.HasPrefix(kv, "PLUGIN_TEST_SECRET_TOKEN=") {
			t.Errorf("secret token leaked into child env: %s", kv)
		}
		if strings.HasPrefix(kv, "FLUCTIO_STORAGE_DSN=") {
			t.Errorf("storage DSN leaked into child env: %s", kv)
		}
	}
	// PATH must survive the scrub or command resolution inside the child
	// (node spawning subprocesses etc.) breaks.
	foundPath := false
	for _, kv := range p.cmd.Env {
		if strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "Path=") {
			foundPath = true
		}
	}
	if !foundPath {
		t.Error("PATH missing from child env — the scrub is too aggressive")
	}
}
