package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fluctio-ai/fluctio/internal/httpclient"
)

// HTTPClient implements the MCP client for HTTP (Streamable HTTP) servers.
type HTTPClient struct {
	url     string
	headers map[string]string
	client  *http.Client
	mu      sync.Mutex
	nextID  int
	// sessionID is the Mcp-Session-Id negotiated during initialize.
	// Stateful Streamable HTTP servers (the go-sdk/TS-SDK default) assign
	// one on initialize and require it echoed on every later request — a
	// POST without it spins up a fresh, uninitialized session and the
	// call fails. Guarded by mu.
	sessionID string
}

// NewHTTPClient creates a new HTTP MCP client.
func NewHTTPClient(url string, headers map[string]string) *HTTPClient {
	return &HTTPClient{
		url:     url,
		headers: expandHeaders(headers),
		// Hard request ceiling: a wedged MCP server otherwise hangs the
		// calling agent's tool goroutine forever (the client previously
		// carried no timeout at all). 120s matches the exec tool's
		// default budget.
		client: httpclient.NewClient(120 * time.Second),
		nextID: 1,
	}
}

func expandHeaders(headers map[string]string) map[string]string {
	expanded := make(map[string]string, len(headers))
	for k, v := range headers {
		if strings.HasPrefix(v, "$") {
			expanded[k] = os.Getenv(v[1:])
		} else {
			expanded[k] = v
		}
	}
	return expanded
}

func (c *HTTPClient) sendRequest(method string, params interface{}) (*jsonRPCResponse, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	c.mu.Unlock()

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	// Streamable HTTP transport (MCP spec) requires POST requests to accept
	// both media types; servers built on the official SDKs reject the
	// handshake with "HTTP 400: Accept must contain both ..." otherwise.
	// Set before user headers so an explicit Accept in the server config
	// can still override.
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}
	// Echo the negotiated session on subsequent requests; see the
	// sessionID field comment.
	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	if sid != "" {
		httpReq.Header.Set("Mcp-Session-Id", sid)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	// The server assigns the session on initialize; remember it before
	// any early return so even a failing first ListTools keeps it.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}

	if resp.StatusCode != http.StatusOK {
		// Bounded read for the error body only — the happy paths below
		// must not drain (or block on) the stream before deciding how to
		// consume it.
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// Streamable HTTP servers may answer with either a plain JSON body or
	// an SSE stream (Content-Type: text/event-stream) whose "data:" lines
	// carry the JSON-RPC response. The SSE path scans line by line and
	// returns as soon as the response carrying our ID shows up, so a
	// server that keeps the stream open for notifications doesn't pin the
	// caller until the client timeout — which a ReadAll-first approach
	// would do, since it blocks until EOF.
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return readSSEResponse(resp.Body, id)
	}

	// Bounded read: a hostile/misbehaving server must not be able to
	// balloon the gateway's memory through an endless response.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	return &rpcResp, nil
}

// readSSEResponse extracts the JSON-RPC response with the given ID from an
// SSE-formatted body. Lines other than "data:" payloads (event:/comments/
// pings) and payloads that don't unmarshal to our response are skipped, so
// server-initiated notifications on the same stream don't confuse the match.
func readSSEResponse(body io.Reader, id int) (*jsonRPCResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 5*1024*1024)
	for scanner.Scan() {
		payload, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" {
			continue
		}
		var rpcResp jsonRPCResponse
		if err := json.Unmarshal([]byte(payload), &rpcResp); err != nil {
			continue
		}
		if rpcResp.ID != id {
			continue
		}
		if rpcResp.Error != nil {
			return nil, fmt.Errorf("RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
		}
		return &rpcResp, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read sse stream: %w", err)
	}
	return nil, fmt.Errorf("SSE stream ended without response for id %d", id)
}

// Connect initializes the connection with the MCP server.
func (c *HTTPClient) Connect() error {
	_, err := c.sendRequest("initialize", initializeParams{
		ProtocolVersion: "2024-11-05",
		ClientInfo:      clientInfo{Name: "fluctio", Version: "0.1.0"},
	})
	return err
}

// ListTools returns the list of tools available on the MCP server.
func (c *HTTPClient) ListTools() ([]ToolDef, error) {
	resp, err := c.sendRequest("tools/list", struct{}{})
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
func (c *HTTPClient) CallTool(name string, args json.RawMessage) (string, error) {
	resp, err := c.sendRequest("tools/call", toolCallParams{
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
	for _, c := range result.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

// Close is a no-op for HTTP clients.
func (c *HTTPClient) Close() error {
	return nil
}
