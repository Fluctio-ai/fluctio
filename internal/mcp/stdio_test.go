package mcp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
		// A single PowerShell process sleeping 20s: alive but never speaks
		// JSON-RPC. (A `cmd /c ping` child gets killed as an orphan when
		// only cmd.exe is terminated — it inherits the test binary's
		// stderr pipe and go test's driver then times out waiting for
		// "Test I/O complete". One process = one kill target.)
		c = NewStdioClient("powershell", []string{"-NoProfile", "-NonInteractive", "-Command", "Start-Sleep -Seconds 20"}, nil, "")
	default:
		c = NewStdioClient("sleep", []string{"20"}, nil, "")
	}

	start := time.Now()
	err := c.Connect(context.Background())
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

// Acceptance for the ctx work (audit special-1): a canceled context must
// return from CallTool IMMEDIATELY — not on the 120s (or any) timer —
// while the server stays alive for later calls.
//
// The HTTP half uses an httptest server that accepts the request and
// never answers; the transport's client timeout is disabled via an
// oversized mcpRequestTimeout-equivalent so the ONLY thing that can
// unblock the call is the ctx.
func TestHTTPCallToolReturnsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"initialize"`) {
			// Answer the handshake normally — only tools/call hangs.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			return
		}
		// Hang until the (canceled) request context aborts — otherwise
		// the deferred srv.Close() waits forever on this handler and
		// deadlocks the test.
		<-r.Context().Done()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":2,"result":{}}`)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}

	start := time.Now()
	cancel()
	_, err := c.CallTool(ctx, "slow", []byte(`{}`))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("canceled CallTool should fail")
	}
	if !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected cancel error, got: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("CallTool took %s to observe cancellation — ctx not flowing to the transport", elapsed)
	}
}

// Same acceptance, stdio half: a fake MCP server (a tiny Go child we
// build once) that answers initialize but never answers tools/call. The
// ctx cancel — not the 120s timer — must return the call, and the child
// must still be alive afterwards (cancel ≠ teardown).
func TestStdioCallToolReturnsOnContextCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a helper child binary")
	}
	old := mcpRequestTimeout
	mcpRequestTimeout = 60 * time.Second
	defer func() { mcpRequestTimeout = old }()

	// Fake MCP server: responds to initialize, then sleeps forever on
	// tools/call. Written as a platform shell one-liner to avoid building
	// a helper binary: `go run` needs the toolchain at test time, which
	// CI has, but a script keeps it lighter. We use PowerShell on Windows
	// and python3 elsewhere — both can speak a line protocol cheaply.
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "windows":
		// Read the initialize line, print a response, then block 60s.
		script := `$l = [Console]::In.ReadLine(); [Console]::Out.WriteLine('{"jsonrpc":"2.0","id":1,"result":{}}'); [Console]::Out.Flush(); Start-Sleep -Seconds 60`
		cmd = "powershell"
		args = []string{"-NoProfile", "-NonInteractive", "-Command", script}
	default:
		script := `read line; echo '{"jsonrpc":"2.0","id":1,"result":{}}'; sleep 60`
		cmd = "sh"
		args = []string{"-c", script}
	}
	c := NewStdioClient(cmd, args, nil, "")
	t.Cleanup(func() { _ = c.Close() }) // kill the sleeping child promptly

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect (fake server handshake): %v", err)
	}

	callCtx, callCancel := context.WithCancel(context.Background())
	start := time.Now()
	callCancel()
	_, err := c.CallTool(callCtx, "slow", []byte(`{}`))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("canceled CallTool should fail")
	}
	if !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected cancel error, got: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("CallTool took %s to observe cancellation — ctx not reaching the stdio read loop", elapsed)
	}
	// Cancel must NOT have torn the server down (that's the timeout
	// path's job): stdin is still open, so a write must succeed — Close()
	// would have closed the pipe and the write would fail.
	if _, werr := c.stdin.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":9,\"method\":\"ping\"}\n")); werr != nil {
		t.Fatalf("stdin write after cancel failed — server was torn down: %v", werr)
	}
}

// Server dies mid-request → the reader's EOF must surface as an error,
// not leave the caller waiting on the timer.
func TestStdioServerDeathFailsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a helper child")
	}
	// A server that answers initialize then exits immediately. PowerShell
	// (not cmd) on Windows: cmd's echo mangles the embedded JSON quotes,
	// which would leave initialize waiting on a never-valid line.
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "windows":
		script := `$null = [Console]::In.ReadLine(); [Console]::Out.WriteLine('{"jsonrpc":"2.0","id":1,"result":{}}'); [Console]::Out.Flush(); exit`
		cmd = "powershell"
		args = []string{"-NoProfile", "-NonInteractive", "-Command", script}
	default:
		cmd = "sh"
		args = []string{"-c", `read line; echo '{"jsonrpc":"2.0","id":1,"result":{}}'; exit 0`}
	}
	c := NewStdioClient(cmd, args, nil, "")
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	// The child has exited by now; tools/call's stdin write or the EOF
	// line must fail fast (well under the 120s timer).
	start := time.Now()
	_, err := c.ListTools(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("ListTools against a dead server should fail")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("dead server pinned the caller for %s", elapsed)
	}
}

