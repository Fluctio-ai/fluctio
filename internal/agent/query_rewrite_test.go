package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// rewriteMockProvider stubs the Provider interface for rewrite tests: it
// records the prompt it was called with and replies with canned content.
type rewriteMockProvider struct {
	content   string
	err       error
	gotPrompt string
}

func (m *rewriteMockProvider) Chat(ctx context.Context, messages []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.Response, error) {
	m.gotPrompt = messages[0].Content
	if m.err != nil {
		return nil, m.err
	}
	return &provider.Response{Content: m.content}, nil
}

func (m *rewriteMockProvider) ChatStream(ctx context.Context, messages []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.StreamReader, error) {
	return nil, errors.New("not implemented")
}

func rewriteTestMessages() []provider.Message {
	return []provider.Message{
		{Role: "system", Content: "SOUL.md system prompt (must not leak into the rewrite context)"},
		{Role: "user", Content: "我们聊聊把部署从 Docker 迁到 Kubernetes 的方案"},
		{Role: "assistant", Content: "好,迁移方案分三步…"},
		{Role: "user", Content: "那个迁移现在什么状态了"},
	}
}

func TestRewriteQueryResolvesAndReturns(t *testing.T) {
	p := &rewriteMockProvider{content: `{"query": "Docker 迁移 Kubernetes 状态"}`}
	msgs := rewriteTestMessages()
	got := rewriteQuery(context.Background(), p, "m1", msgs, "那个迁移现在什么状态了")
	if got != "Docker 迁移 Kubernetes 状态" {
		t.Fatalf("rewriteQuery = %q, want the rewritten query", got)
	}
	// The prompt must carry both halves the rewrite depends on: the
	// resolution context and the raw message.
	if !strings.Contains(p.gotPrompt, "Docker") || !strings.Contains(p.gotPrompt, "Kubernetes") {
		t.Fatalf("prompt missing resolution context:\n%s", p.gotPrompt)
	}
	if !strings.Contains(p.gotPrompt, "那个迁移现在什么状态了") {
		t.Fatalf("prompt missing the raw latest message:\n%s", p.gotPrompt)
	}
	// System messages must not be fed into the prompt.
	if strings.Contains(p.gotPrompt, "SOUL.md") {
		t.Fatalf("system message leaked into rewrite context:\n%s", p.gotPrompt)
	}
}

func TestRewriteQueryFallsBack(t *testing.T) {
	// Every failure mode must yield the raw query unchanged: the rewrite
	// is a quality layer, never an availability gate.
	long := strings.Repeat("长", 200)
	cases := []struct {
		name    string
		content string
		err     error
	}{
		{"provider error", "", errors.New("boom")},
		{"empty body", "", nil},
		{"blank body", "   ", nil},
		{"prose instead of json", "I think the query is about migration", nil},
		// Pure-filler messages legitimately rewrite to "" — downstream
		// lanes still gate on their own floors, so fall back to raw.
		{"empty query", `{"query": ""}`, nil},
		{"oversized query", `{"query": "` + long + `"}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &rewriteMockProvider{content: tc.content, err: tc.err}
			if got := rewriteQuery(context.Background(), p, "m1", rewriteTestMessages(), "原话"); got != "原话" {
				t.Fatalf("rewriteQuery = %q, want fallback to the raw query", got)
			}
		})
	}
}

func TestRewriteQueryNilProvider(t *testing.T) {
	got := rewriteQuery(context.Background(), nil, "m1", rewriteTestMessages(), "原话")
	if got != "原话" {
		t.Fatalf("rewriteQuery with nil provider = %q, want the raw query", got)
	}
}

func TestClipRewriteContextBounds(t *testing.T) {
	// 8 non-system messages: only the most recent 6 appear.
	var msgs []provider.Message
	for i := 0; i < 8; i++ {
		msgs = append(msgs, provider.Message{Role: "user", Content: strings.Repeat("m", 5)})
	}
	msgs = append([]provider.Message{{Role: "system", Content: "sys"}}, msgs...)
	got := clipRewriteContext(msgs)
	if strings.Count(got, "user:") != queryRewriteCtxMsgs {
		t.Fatalf("context message count = %d, want %d:\n%s", strings.Count(got, "user:"), queryRewriteCtxMsgs, got)
	}
}

func TestClipBytesDoesNotSplitRune(t *testing.T) {
	if got := clipBytes("中文测试", 4); got != "中…" {
		t.Fatalf("clipBytes(中文测试, 4) = %q, want 中…", got)
	}
	if got := clipBytes("abc", 10); got != "abc" {
		t.Fatalf("clipBytes under limit = %q, want abc", got)
	}
}
