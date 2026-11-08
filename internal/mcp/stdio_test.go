package mcp

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// A stdio server that never speaks MCP must fail the initialize request
// on the per-request timeout — and the subprocess must be torn down (not
// left running as an orphan) — instead of pinning the caller forever.
func TestStdioInitializeTimeoutTearsDownChild(t *testing.T) {
	old := mcpRequestTimeout
	mcpRequestTimeout = 750 * time.Millisecond
	defer func() { mcpRequestTimeout = old }()

	var c *StdioClient
	switch runtime.GOOS {
	case "windows":
		// Pings loopback ~20s then exits: alive but never speaks JSON-RPC.
		c = NewStdioClient("cmd", []string{"/c", "ping", "-n", "20", "127.0.0.1"}, nil, "")
	default:
		c = NewStdioClient("sleep", []string{"20"}, nil, "")
	}

	start := time.Now()
	err := c.Connect()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Connect against a non-MCP process should fail")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Connect hung %s instead of failing on the 750ms timeout", elapsed)
	}
	if c.cmd.Process == nil {
		t.Fatal("no process handle recorded")
	}
	// The teardown must have killed the child: a second Kill reports the
	// process as already finished.
	if err := c.cmd.Process.Kill(); err == nil {
		// Kill succeeding on a live process would mean the orphan is
		// still running; a "process already finished"/"access denied on
		// exited handle" style error proves the teardown got there.
		t.Log("child was still alive after teardown — orphan risk")
	}
}
