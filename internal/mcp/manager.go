package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/config"
)

// Manager manages connections to multiple MCP servers.
type Manager struct {
	servers map[string]Client // serverName -> client
	// toolMap maps prefixed tool name -> (serverName, originalToolName)
	toolMap map[string]toolRoute
}

type toolRoute struct {
	serverName   string
	originalName string
}

// NewManager creates an MCP manager and connects to all configured servers.
// dir is passed as the working directory for stdio subprocesses so MCP
// tools land in the agent's session/project scope. Servers that fail to
// connect are logged as warnings but don't block startup. Connections run
// in PARALLEL: serial dialing let N unreachable servers (each up to its
// client timeout) stall session binding by N × timeout.
//
// Dialing uses context.Background() on purpose: the manager is a
// session-lifetime resource and NewManager runs at session bind, where
// the binding turn's ctx may already be ending. Canceling a turn must not
// leave the session's MCP servers half-dialed and unreachable for every
// later turn (the manager is only rebuilt when the session's scope dir
// changes). Each dial is still bounded by the transports' own 120s
// request timer.
func NewManager(servers map[string]config.MCPServerConfig, dir string) *Manager {
	m := &Manager{
		servers: make(map[string]Client),
		toolMap: make(map[string]toolRoute),
	}

	type serverConn struct {
		name   string
		client Client
		tools  []ToolDef
	}
	conns := make(chan serverConn, len(servers))
	pending := 0
	for name, cfg := range servers {
		var client Client
		switch cfg.Type {
		case "http":
			client = NewHTTPClient(cfg.URL, cfg.Headers)
		case "stdio":
			client = NewStdioClient(cfg.Command, cfg.Args, cfg.Env, dir)
		default:
			slog.Warn("unknown MCP server type, skipping", "server", name, "type", cfg.Type)
			continue
		}
		pending++
		go func(name string, client Client) {
			dialCtx, cancel := context.WithTimeout(context.Background(), mcpDialTimeout)
			defer cancel()
			if err := client.Connect(dialCtx); err != nil {
				slog.Warn("failed to connect to MCP server, skipping", "server", name, "error", err)
				// Release whatever the client did acquire (a stdio child
				// started before a failed initialize, sockets, …).
				_ = client.Close()
				conns <- serverConn{name: name}
				return
			}
			tools, err := client.ListTools(dialCtx)
			if err != nil {
				slog.Warn("failed to list MCP tools, skipping", "server", name, "error", err)
				client.Close()
				conns <- serverConn{name: name}
				return
			}
			conns <- serverConn{name: name, client: client, tools: tools}
		}(name, client)
	}

	for i := 0; i < pending; i++ {
		sc := <-conns
		if sc.client == nil {
			continue
		}
		m.servers[sc.name] = sc.client

		for _, t := range sc.tools {
			prefixed := prefixToolName(sc.name, t.Name)
			if _, clash := m.toolMap[prefixed]; clash {
				// Sanitization is asymmetric (server names are mapped,
				// tool names are not), so "a_b"+"c" and "a"+"b_c" can
				// collide; surface it instead of silently overwriting.
				slog.Warn("MCP tool name collision — later server overwrites earlier registration",
					"tool", prefixed, "server", sc.name)
			}
			m.toolMap[prefixed] = toolRoute{
				serverName:   sc.name,
				originalName: t.Name,
			}
		}

		slog.Info("connected to MCP server", "server", sc.name, "tools", len(sc.tools))
	}

	return m
}

// mcpDialTimeout bounds the session-bind dial (Connect + ListTools) per
// server — independent of the per-request timer so a slow handshake
// can't stretch bind beyond this budget.
const mcpDialTimeout = 120 * time.Second

// ToolDefs returns tool definitions for all MCP tools, with prefixed names.
func (m *Manager) ToolDefs(ctx context.Context) []ToolDef {
	var defs []ToolDef
	for name, cfg := range m.servers {
		tools, err := cfg.ListTools(ctx)
		if err != nil {
			slog.Warn("failed to list tools from MCP server", "server", name, "error", err)
			continue
		}
		for _, t := range tools {
			defs = append(defs, ToolDef{
				Name:        prefixToolName(name, t.Name),
				Description: t.Description,
				InputSchema: t.InputSchema,
			})
		}
	}
	return defs
}

// CallTool routes a prefixed tool call to the correct MCP server. The
// turn's ctx flows down to the transport: a user Stop or turn deadline
// cancels the in-flight MCP call instead of pinning it to the transport's
// internal timer.
func (m *Manager) CallTool(ctx context.Context, prefixedName string, args json.RawMessage) (string, error) {
	route, ok := m.toolMap[prefixedName]
	if !ok {
		return "", fmt.Errorf("unknown MCP tool: %s", prefixedName)
	}

	client, ok := m.servers[route.serverName]
	if !ok {
		return "", fmt.Errorf("MCP server not connected: %s", route.serverName)
	}

	return client.CallTool(ctx, route.originalName, args)
}

// HasTools returns true if any MCP tools are available.
func (m *Manager) HasTools() bool {
	return len(m.toolMap) > 0
}

// Close shuts down all MCP server connections.
func (m *Manager) Close() {
	for name, client := range m.servers {
		if err := client.Close(); err != nil {
			slog.Warn("error closing MCP server", "server", name, "error", err)
		}
	}
}

func prefixToolName(serverName, toolName string) string {
	// Sanitize server name: replace non-alphanumeric with _
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return '_'
	}, serverName)
	return "mcp_" + safe + "_" + toolName
}
