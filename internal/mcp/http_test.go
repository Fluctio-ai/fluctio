package mcp

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Streamable HTTP transport (MCP spec) requires POST requests to accept
// both media types; SDK-backed servers (go-sdk, TS SDK) reject the
// handshake with "HTTP 400: Accept must contain both ..." otherwise.
func TestHTTPClientDualAcceptHeader(t *testing.T) {
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, nil)
	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	for _, want := range []string{"application/json", "text/event-stream"} {
		if !strings.Contains(gotAccept, want) {
			t.Fatalf("Accept = %q, missing %q", gotAccept, want)
		}
	}
}

// Stateful streamable servers answer with an SSE-formatted body and
// require the Mcp-Session-Id from initialize to be echoed on later
// requests — without it each POST lands on a fresh uninitialized session.
func TestHTTPClientStreamableSession(t *testing.T) {
	var sessionEchoed string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		isInit := strings.Contains(string(body), `"initialize"`)
		if isInit {
			w.Header().Set("Mcp-Session-Id", "sess-123")
		} else {
			sessionEchoed = r.Header.Get("Mcp-Session-Id")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message\n")
		if isInit {
			fmt.Fprint(w, `data: {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"t","version":"0"}}}`+"\n\n")
		} else {
			fmt.Fprint(w, `data: {"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"search","description":"d","inputSchema":{"type":"object"}}]}}`+"\n\n")
		}
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, nil)
	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "search" {
		t.Fatalf("tools = %+v, want one 'search'", tools)
	}
	if sessionEchoed != "sess-123" {
		t.Fatalf("Mcp-Session-Id echoed = %q, want %q", sessionEchoed, "sess-123")
	}
}

// A stateful server may keep the POST response stream open after writing
// the response event. The client must return as soon as the matching
// response arrives, not block until the 120s timeout.
func TestHTTPClientSSEReturnsBeforeStreamClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message\n")
		fmt.Fprint(w, `data: {"jsonrpc":"2.0","id":1,"result":{}}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, nil)
	done := make(chan error, 1)
	go func() { done <- c.Connect() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client blocked on a stream the server never closes")
	}
}
