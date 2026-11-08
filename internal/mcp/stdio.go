package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Per-request bound: a hung MCP server must not pin the agent turn
// forever. On timeout the subprocess is torn down so the reader
// goroutine unblocks and every later request fails fast.
var mcpRequestTimeout = 120 * time.Second

// Large tool responses (screenshot base64, big exports) exceed the old
// 1MB scanner cap; 16MB matches the HTTP path's tolerance.
const mcpScanBufferMax = 16 * 1024 * 1024

// StdioClient implements the MCP client for stdio-based servers.
type StdioClient struct {
	command string
	args    []string
	env     map[string]string
	dir     string // working dir for the subprocess (per-session scope)
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	mu      sync.Mutex
	nextID  int
	// lines is the persistent stdout feed: ONE reader goroutine (started
	// at Connect) pushes every stdout line here, and each sendRequest
	// consumes from the shared channel. Before the ctx work, every
	// request spawned its own reader on the same scanner — after a
	// context-cancel return that skipped teardown, a second reader would
	// race the stale one on scanner reads. One reader for the process
	// lifetime removes the race: stale responses (for canceled requests)
	// are drained by whichever request reads them and dropped on the ID
	// mismatch check, and a cancel now leaves the server running for
	// later turns instead of killing it.
	lines chan stdioLine
}

type stdioLine struct {
	text string
	err  error
}

// NewStdioClient creates a new stdio MCP client. dir is the subprocess's
// working directory — set to the agent's session/project scope dir so
// MCP tools (Playwright screenshots, file-backed servers) land in the
// workspace, not the gateway's launch dir. Empty inherits the parent's cwd.
func NewStdioClient(command string, args []string, env map[string]string, dir string) *StdioClient {
	return &StdioClient{
		command: command,
		args:    args,
		env:     env,
		dir:     dir,
		nextID:  1,
		lines:   make(chan stdioLine, 16),
	}
}

// ScrubbedEnv returns the minimal child environment for a low-trust
// subprocess (MCP stdio servers, plugin bridges): only the system
// variables the OS and language runtimes need (binary lookup, temp dirs,
// Windows loader paths, locale), covering both Windows and POSIX.
// Everything else the host inherited is dropped — third-party child
// processes must not receive host credentials through the environment.
// Per-child config travels explicitly: MCP servers get their per-server
// env on top, plugins their initialize params.
func ScrubbedEnv() []string {
	keep := map[string]bool{
		"PATH":        true, // binary lookup (POSIX and Windows launchers)
		"PATHEXT":     true, // Windows: executable extensions for resolution
		"HOME":        true,
		"TEMP":        true,
		"TMP":         true,
		"TMPDIR":      true,
		"SystemRoot":  true, // Windows: required by the loader / winsock
		"COMSPEC":     true, // Windows: cmd.exe location for shell-outs
		"USERPROFILE": true,
		"APPDATA":     true,
		"LANG":        true,
	}
	env := make([]string, 0, 16)
	for _, kv := range os.Environ() {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if keep[k] || strings.HasPrefix(k, "LC_") {
			env = append(env, kv)
		}
	}
	return env
}

// scrubbedEnv is the internal alias kept so call sites read as before.
func scrubbedEnv() []string { return ScrubbedEnv() }

// Connect starts the subprocess and initializes the MCP session. The ctx
// bounds the initialize handshake: a canceled context (turn aborted
// mid-bind) fails Connect, and the caller's error path tears the child
// down.
func (c *StdioClient) Connect(ctx context.Context) error {
	c.cmd = exec.Command(c.command, c.args...)
	// cwd into the session/project scope dir so MCP tools write into the
	// agent's workspace (sessions/<sid>/ or projects/<pid>/), not the
	// gateway's launch directory.
	if c.dir != "" {
		c.cmd.Dir = c.dir
	}

	// Pass only the minimal environment an MCP subprocess needs to run.
	// Inheriting the full os.Environ() would hand every credential the host
	// process holds (API keys, tokens, service URLs) straight to a low-trust
	// MCP server, bypassing env redaction. The per-server env below is the
	// only extra data the child receives.
	c.cmd.Env = scrubbedEnv()
	for k, v := range c.env {
		if strings.HasPrefix(v, "$") {
			v = os.Getenv(v[1:])
		}
		c.cmd.Env = append(c.cmd.Env, k+"="+v)
	}

	var err error
	c.stdin, err = c.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	c.cmd.Stderr = os.Stderr
	c.scanner = bufio.NewScanner(stdout)
	c.scanner.Buffer(make([]byte, 0, 64*1024), mcpScanBufferMax)

	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("start process: %w", err)
	}

	// One reader goroutine for the process's whole lifetime — see the
	// lines field comment for why this must not be per-request. It closes
	// the channel when the stream ends so callers learn about EOF
	// immediately instead of waiting out the request timer (a clean EOF
	// carries no scanner error — the closed channel IS the signal).
	go func() {
		for c.scanner.Scan() {
			c.lines <- stdioLine{text: c.scanner.Text()}
		}
		c.lines <- stdioLine{err: c.scanner.Err()}
		close(c.lines)
	}()

	// Send initialize. On failure the subprocess is already alive — tear
	// it down here or every scope rebuild leaks an orphan child.
	_, initErr := c.sendRequest(ctx, "initialize", initializeParams{
		ProtocolVersion: "2024-11-05",
		ClientInfo:      clientInfo{Name: "fluctio", Version: "0.1.0"},
	})
	if initErr != nil {
		_ = c.stdin.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
			_, _ = c.cmd.Process.Wait()
		}
		return fmt.Errorf("initialize: %w", initErr)
	}
	return nil
}

func (c *StdioClient) sendRequest(ctx context.Context, method string, params interface{}) (*jsonRPCResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID
	c.nextID++

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	data = append(data, '\n')
	if _, err := c.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("write to stdin: %w", err)
	}

	// The scanner has no deadline support, so the (single, persistent)
	// reader goroutine feeds the lines channel and the caller selects
	// over it, the per-request timer, and the caller's context. The timer
	// is the hung-server floor that tears the subprocess down; the ctx is
	// the caller-controlled bound — a canceled request returns
	// immediately WITHOUT tearing the server down (it stays usable for
	// later turns), and its late response is dropped harmlessly on the ID
	// mismatch check by whichever request drains the channel next.
	timer := time.NewTimer(mcpRequestTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("MCP request %q canceled: %w", method, ctx.Err())
		case <-timer.C:
			_ = c.Close()
			return nil, fmt.Errorf("MCP request %q timed out after %s — server torn down", method, mcpRequestTimeout)
		case lo, ok := <-c.lines:
			if !ok {
				// Stream ended (server exited / pipe closed): fail now,
				// don't pin the caller until the timer.
				return nil, fmt.Errorf("read stdout: server stream closed during %q", method)
			}
			if lo.err != nil {
				return nil, fmt.Errorf("read stdout: %w", lo.err)
			}
			line := lo.text
			if len(line) == 0 {
				continue
			}

			var resp jsonRPCResponse
			if err := json.Unmarshal([]byte(line), &resp); err != nil {
				continue // skip non-JSON lines (e.g. stderr leaking)
			}

			if resp.ID == id {
				if resp.Error != nil {
					return nil, fmt.Errorf("RPC error %d: %s", resp.Error.Code, resp.Error.Message)
				}
				return &resp, nil
			}
		}
	}
}

// ListTools returns the list of tools available on the MCP server.
func (c *StdioClient) ListTools(ctx context.Context) ([]ToolDef, error) {
	resp, err := c.sendRequest(ctx, "tools/list", struct{}{})
	if err != nil {
		return nil, err
	}

	var result toolsListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("parse tools list: %w", err)
	}

	return result.Tools, nil
}

// CallTool calls a tool on the MCP server.
func (c *StdioClient) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	resp, err := c.sendRequest(ctx, "tools/call", toolCallParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return "", err
	}

	var result toolCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return "", fmt.Errorf("parse tool result: %w", err)
	}

	var texts []string
	for _, ct := range result.Content {
		if ct.Type == "text" {
			texts = append(texts, ct.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

// Close stops the subprocess.
func (c *StdioClient) Close() error {
	if c.stdin != nil {
		c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		c.cmd.Process.Kill()
		c.cmd.Wait()
	}
	return nil
}
