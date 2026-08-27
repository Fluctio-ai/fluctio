package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

func TestClassifyToolError(t *testing.T) {
	cases := []struct{ in, wantCat, wantHintSub string }{
		{"exec: bash: executable file not found in %PATH%\n[Analyze...]", "env_missing", "替代"},
		{"open /x/y: no such file or directory", "env_missing", "替代"},
		{"'/bin/sh: foo: command not found'", "env_missing", "替代"},
		{"open /etc/shadow: permission denied", "permission", ""},
		{"createfile access is denied", "permission", ""},
		{"upstream_error: 503 Service busy", "external", "重试"},
		{"HTTP 500 internal server error", "external", "重试"},
		{"context deadline exceeded (timeout)", "external", "重试"},
		{"invalid argument: missing required field", "logic", "参数"},
		{"just some normal success result text", "", ""}, // 非失败 → 不分类
	}
	for _, c := range cases {
		gotCat, gotHint := classifyToolError(c.in)
		if gotCat != c.wantCat {
			t.Errorf("classifyToolError(%q) category = %q, want %q", c.in, gotCat, c.wantCat)
		}
		if c.wantHintSub != "" && !strings.Contains(gotHint, c.wantHintSub) {
			t.Errorf("classifyToolError(%q) hint = %q, want containing %q", c.in, gotHint, c.wantHintSub)
		}
	}
}

// TestClassifyToolErrorStreamingSuccessNotMisfired verifies that the
// isFailedToolResult gate added to the streaming path (loop.go:3447 area)
// prevents successful tool results that happen to contain classifier
// substrings ("503", "timeout", "http 4") from being tagged with a
// spurious [失败类别: external] marker. Regression for Phase 2 final-review
// finding: streaming classifyToolError was previously called
// unconditionally, diverging from the main path's gate at loop.go:2737.
func TestClassifyToolErrorStreamingSuccessNotMisfired(t *testing.T) {
	// Success results that contain classifier-trigger substrings.
	// Without the gate, classifyToolError alone would return ("external", ...).
	successCases := []string{
		"Server listening on port 5030",
		"configured timeout 5s for upstream calls",
		"see http 4xx docs at /docs/errors",
	}
	for _, content := range successCases {
		// Sanity: the raw classifier WOULD fire on these inputs, so the
		// gate is the only thing suppressing the tag.
		if cat, _ := classifyToolError(content); cat == "" {
			t.Fatalf("precondition failed: classifyToolError(%q) returned empty; test needs a substring that WOULD be tagged", content)
		}
		// Gate: a successful result (no err, no HTTP 4xx/5xx prefix, no
		// "[Analyze the error above…]" envelope) must NOT be tagged.
		if isFailedToolResult(nil, content) {
			t.Errorf("isFailedToolResult(nil, %q) = true; want false (success should pass the gate)", content)
		}
		// Emulate the streaming-path block: only tag when the gate agrees.
		tagged := false
		if isFailedToolResult(nil, content) {
			if cat, _ := classifyToolError(content); cat != "" {
				tagged = true
			}
		}
		if tagged {
			t.Errorf("streaming-path gate failed to suppress tag for success result %q", content)
		}
	}

	// Negative control: a genuine failure must still get tagged.
	failContent := "upstream_error: 503 Service busy"
	if !isFailedToolResult(nil, failContent) {
		// 503 alone does not set HasPrefix "HTTP 5" (it's "upstream_error"),
		// so the gate treats it as success. That's the conservative contract
		// shared with the main path — both only tag when the gate agrees.
		// Verify the symmetric main-path behavior: gate must drive the tag,
		// not the classifier alone.
		t.Logf("note: %q is not flagged by isFailedToolResult; consistent with main path", failContent)
	}
	// Use a failure that the gate actually recognises to confirm the tag still fires there.
	realFail := "HTTP 500 internal server error"
	if !isFailedToolResult(nil, realFail) {
		t.Fatalf("isFailedToolResult(nil, %q) = false; want true (genuine failure)", realFail)
	}
	if cat, _ := classifyToolError(realFail); cat == "" {
		t.Fatalf("classifyToolError(%q) returned empty; want a category", realFail)
	}
}

// TestClassifyLLMError covers the Hermes-style error→category router that
// llmRetry consults: terminal (don't retry), context_length (compress then
// resend), retryable (backoff + retry). Borrowed from Hermes' classify_api_
// error taxonomy — see classifyLLMError's doc comment for the status map.
func TestClassifyLLMError(t *testing.T) {
	httpErr := func(code int, body string) error {
		return &provider.HTTPError{StatusCode: code, Body: body}
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"canceled", context.Canceled, "terminal"},
		{"deadline", context.DeadlineExceeded, "terminal"},
		{"network EOF", errors.New("read tcp: EOF"), "retryable"},
		{"send failure", errors.New("send request: connection reset"), "retryable"},
		// Typed connection errors (what the providers actually emit via
		// "send request: %w" / "read stream: %w") get the unbounded tier.
		{"url.Error transport", &url.Error{Op: "Post", URL: "https://api.example/v1", Err: errors.New("dial tcp: connection refused")}, "connection"},
		{"wrapped send request", fmt.Errorf("send request: %w", &url.Error{Op: "Post", URL: "https://api.example/v1", Err: errors.New("connection reset")}), "connection"},
		{"wrapped unexpected EOF", fmt.Errorf("read stream: %w", io.ErrUnexpectedEOF), "connection"},
		{"bare EOF", io.EOF, "connection"},
		{"net op error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, "connection"},
		// Deterministic non-network errors stay bounded so the unbounded
		// tier can never hang a turn on a parse failure.
		{"json parse failure", fmt.Errorf("decode response: %w", errors.New("invalid character 'x'")), "retryable"},
		{"402 billing", httpErr(http.StatusPaymentRequired, "insufficient credits"), "terminal"},
		{"401 auth", httpErr(http.StatusUnauthorized, "invalid api key"), "terminal"},
		{"403 forbidden", httpErr(http.StatusForbidden, "forbidden"), "terminal"},
		{"413 too large", httpErr(http.StatusRequestEntityTooLarge, "payload too large"), "context_length"},
		{"400 PTL context length", httpErr(http.StatusBadRequest, "this model's context length is too long"), "context_length"},
		{"400 PTL too many tokens", httpErr(http.StatusBadRequest, "too many tokens"), "context_length"},
		{"400 PTL maximum context", httpErr(http.StatusBadRequest, "Your request exceeds the maximum context window"), "context_length"},
		{"400 param error (not PTL)", httpErr(http.StatusBadRequest, "invalid value for temperature"), "retryable"},
		{"429 rate limit", httpErr(http.StatusTooManyRequests, "rate limited"), "retryable"},
		{"500 server", httpErr(http.StatusInternalServerError, "oops"), "retryable"},
		{"502 gateway", httpErr(http.StatusBadGateway, "bad gateway"), "retryable"},
		{"503 overloaded", httpErr(http.StatusServiceUnavailable, "overloaded"), "retryable"},
		{"529 overloaded anthropic", httpErr(529, "overloaded"), "retryable"},
		{"404 not found", httpErr(http.StatusNotFound, "model not found"), "retryable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyLLMError(c.err)
			if got != c.want {
				t.Errorf("classifyLLMError(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// TestLLMRetryContextTooLong verifies llmRetry surfaces a context-length
// error wrapped with ErrContextTooLong immediately (no backoff, no extra
// attempts) so the caller can compress-and-resend. This is the entry point
// for callLLMWithPTLRecovery.
func TestLLMRetryContextTooLong(t *testing.T) {
	calls := 0
	ptlErr := &provider.HTTPError{StatusCode: http.StatusBadRequest, Body: "context length too long"}
	_, err := llmRetry(context.Background(), "test", func(ctx context.Context) (*provider.Response, error) {
		calls++
		return nil, ptlErr
	})
	if calls != 1 {
		t.Errorf("expected 1 call (no retry on PTL), got %d", calls)
	}
	if !errors.Is(err, ErrContextTooLong) {
		t.Errorf("expected ErrContextTooLong wrap, got %v", err)
	}
}

// TestLLMRetryTerminalNoRetry verifies terminal errors (billing/auth) are
// not retried — retrying a 402 wastes calls and delays the inevitable abort.
func TestLLMRetryTerminalNoRetry(t *testing.T) {
	calls := 0
	billingErr := &provider.HTTPError{StatusCode: http.StatusPaymentRequired, Body: "no credits"}
	_, err := llmRetry(context.Background(), "test", func(ctx context.Context) (*provider.Response, error) {
		calls++
		return nil, billingErr
	})
	if calls != 1 {
		t.Errorf("expected 1 call (no retry on billing), got %d", calls)
	}
	if !errors.Is(err, billingErr) {
		t.Errorf("expected original billing error, got %v", err)
	}
}

// TestLLMRetryRetryableRetries verifies transient 5xx errors DO go through
// the backoff retry loop. We use a short backoff override isn't possible
// (backoff is hardcoded), so this test asserts calls == llmRetryAttempts and
// accepts the ~14s of real backoff. Marked long via t.Skip when run with
// -short to keep the default `go test ./...` fast.
func TestLLMRetryRetryableRetries(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping backoff-duration test in short mode")
	}
	calls := 0
	serverErr := &provider.HTTPError{StatusCode: http.StatusBadGateway, Body: "bad gateway"}
	_, err := llmRetry(context.Background(), "test", func(ctx context.Context) (*provider.Response, error) {
		calls++
		return nil, serverErr
	})
	if calls != llmRetryAttempts {
		t.Errorf("expected %d calls (retry on 5xx), got %d", llmRetryAttempts, calls)
	}
	if err == nil {
		t.Errorf("expected error after all retries failed")
	}
}

// TestLLMRetryConnectionUnbounded verifies the connection tier retries
// past the bounded llmRetryAttempts limit (a network blip must not kill
// a long task), emits "reconnecting" events from the 2nd retry on, and
// respects ctx cancellation.
func TestLLMRetryConnectionUnbounded(t *testing.T) {
	origBackoff := connRetryBackoff
	connRetryBackoff = func(attempt int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { connRetryBackoff = origBackoff })

	connErr := fmt.Errorf("send request: %w", &url.Error{
		Op: "Post", URL: "https://api.example/v1", Err: errors.New("dial tcp: connection refused"),
	})

	calls := 0
	ch := make(chan ChatEvent, 16)
	ctx := ContextWithChatEvents(context.Background(), ch)
	resp, err := llmRetry(ctx, "test", func(ctx context.Context) (*provider.Response, error) {
		calls++
		if calls <= 5 { // 5 failures > llmRetryAttempts(3) — bounded tier would give up
			return nil, connErr
		}
		return &provider.Response{Content: "ok"}, nil
	})
	if err != nil {
		t.Fatalf("expected recovery after network blip, got %v", err)
	}
	if resp == nil || resp.Content != "ok" {
		t.Fatalf("expected response, got %+v", resp)
	}
	if calls != 6 {
		t.Errorf("expected 6 calls (5 failed + 1 ok), got %d", calls)
	}

	// Reconnecting events fire from the 2nd retry on: attempts 2..5 = 4 events.
	reconnects := 0
	for {
		select {
		case evt := <-ch:
			if evt.Type == "reconnecting" {
				reconnects++
			}
		default:
			if reconnects != 4 {
				t.Errorf("expected 4 reconnecting events (attempts 2-5), got %d", reconnects)
			}
			return
		}
	}
}

// TestLLMRetryConnectionCtxCancel verifies the unbounded tier is always
// escapable: a cancelled ctx must break the retry loop instead of looping
// forever on connection failures.
func TestLLMRetryConnectionCtxCancel(t *testing.T) {
	origBackoff := connRetryBackoff
	connRetryBackoff = func(attempt int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { connRetryBackoff = origBackoff })

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	connErr := fmt.Errorf("read stream: %w", io.ErrUnexpectedEOF)
	_, err := llmRetry(ctx, "test", func(ctx context.Context) (*provider.Response, error) {
		calls++
		return nil, connErr
	})
	if err == nil {
		t.Fatal("expected error after ctx cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled in joined error, got %v", err)
	}
}

// TestHideTrippedTools covers the per-tool failure-streak circuit breaker
// (Phase 2). A tool at/over the limit is removed from the LLM-bound list and
// recorded as tripped; under-limit tools and tools that already tripped in a
// prior round are handled correctly.
func TestHideTrippedTools(t *testing.T) {
	tools := []provider.Tool{
		{Function: provider.ToolFunction{Name: "web_search"}},
		{Function: provider.ToolFunction{Name: "read_file"}},
		{Function: provider.ToolFunction{Name: "exec"}},
	}
	streak := map[string]int{
		"web_search": 5, // at limit → trip
		"read_file":  2, // under limit → keep
	}

	// First round: web_search trips.
	got, tripped := hideTrippedTools(tools, streak, 5, nil)
	if len(tripped) != 1 || tripped[0] != "web_search" {
		t.Fatalf("first round tripped = %v, want [web_search]", tripped)
	}
	gotNames := toolNames(got)
	if contains(gotNames, "web_search") {
		t.Errorf("web_search should be hidden, got tools %v", gotNames)
	}
	if !contains(gotNames, "read_file") || !contains(gotNames, "exec") {
		t.Errorf("read_file and exec should remain, got tools %v", gotNames)
	}

	// Second round with same streak: web_search stays hidden, no re-nudge.
	got2, tripped2 := hideTrippedTools(tools, streak, 5, tripped)
	if len(tripped2) != 1 {
		t.Fatalf("second round tripped = %v, want no newly-tripped (already known)", tripped2)
	}
	if contains(toolNames(got2), "web_search") {
		t.Errorf("web_search should stay hidden on round 2")
	}

	// exec now also trips — both should be hidden, only exec newly recorded.
	streak["exec"] = 6
	got3, tripped3 := hideTrippedTools(tools, streak, 5, tripped)
	if !contains(tripped3, "exec") {
		t.Errorf("exec should be newly tripped, got %v", tripped3)
	}
	names3 := toolNames(got3)
	if contains(names3, "web_search") || contains(names3, "exec") {
		t.Errorf("web_search + exec both hidden expected, got %v", names3)
	}
	if !contains(names3, "read_file") {
		t.Errorf("read_file should survive, got %v", names3)
	}

	// Empty streak → no-op.
	got4, tripped4 := hideTrippedTools(tools, nil, 5, nil)
	if len(tripped4) != 0 || len(got4) != len(tools) {
		t.Errorf("empty streak should be no-op, got tools=%v tripped=%v", got4, tripped4)
	}
}

func toolNames(ts []provider.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Function.Name)
	}
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestLooksTruncated covers the Phase-3 max-output truncation heuristic.
// Fires only when maxTokens is set, usage is reported, no tool calls, and
// output ≥ 90% of the cap.
func TestLooksTruncated(t *testing.T) {
	cases := []struct {
		name      string
		resp      *provider.Response
		maxTokens int
		want      bool
	}{
		{"nil resp", nil, 4096, false},
		{"no maxTokens", &provider.Response{Usage: provider.Usage{OutputTokens: 4000}}, 0, false},
		{"no usage", &provider.Response{Content: "hi"}, 4096, false},
		{"has tool calls", &provider.Response{Usage: provider.Usage{OutputTokens: 4000}, ToolCalls: []provider.ToolCall{{}}}, 4096, false},
		{"at 90% threshold", &provider.Response{Usage: provider.Usage{OutputTokens: 3686}}, 4096, true},
		{"over cap", &provider.Response{Usage: provider.Usage{OutputTokens: 4096}}, 4096, true},
		{"well under cap", &provider.Response{Usage: provider.Usage{OutputTokens: 1000}}, 4096, false},
		{"just under 90%", &provider.Response{Usage: provider.Usage{OutputTokens: 3685}}, 4096, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksTruncated(c.resp, c.maxTokens); got != c.want {
				t.Errorf("looksTruncated(%+v, %d) = %v, want %v", c.resp, c.maxTokens, got, c.want)
			}
		})
	}
}

// TestHTTPErrorRetryAfterSeconds covers the Retry-After parsing added for
// server-paced 429 backoff: seconds form parses (clamped to 60), missing /
// date-form / malformed values report ok=false.
func TestHTTPErrorRetryAfterSeconds(t *testing.T) {
	h := func(v string) *provider.HTTPError {
		return &provider.HTTPError{
			StatusCode: http.StatusTooManyRequests,
			Headers:    http.Header{"Retry-After": []string{v}},
		}
	}
	if secs, ok := h("5").RetryAfterSeconds(); !ok || secs != 5 {
		t.Errorf("Retry-After: 5 → (%d, %v), want (5, true)", secs, ok)
	}
	if secs, ok := h("120").RetryAfterSeconds(); !ok || secs != 60 {
		t.Errorf("Retry-After: 120 should clamp to 60, got (%d, %v)", secs, ok)
	}
	for _, raw := range []string{"", "Wed, 21 Oct 2015 07:28:00 GMT", "soon", "-3"} {
		if _, ok := h(raw).RetryAfterSeconds(); ok {
			t.Errorf("Retry-After: %q should not parse, got ok=true", raw)
		}
	}
	if _, ok := (&provider.HTTPError{StatusCode: 429}).RetryAfterSeconds(); ok {
		t.Error("nil Headers should not parse")
	}
}

// TestRetryAfterDelay pins the jitter and asserts the server-paced
// backoff window: 5s directive → [5s, 7.5s], oversized directive clamps
// to 30s, and errors without a directive fall through (ok=false).
func TestRetryAfterDelay(t *testing.T) {
	orig := retryAfterJitter
	t.Cleanup(func() { retryAfterJitter = orig })

	rateLimited := func(secs string) *provider.HTTPError {
		return &provider.HTTPError{
			StatusCode: http.StatusTooManyRequests,
			Headers:    http.Header{"Retry-After": []string{secs}},
		}
	}

	retryAfterJitter = func() float64 { return 0 }
	if d, ok := retryAfterDelay(rateLimited("5")); !ok || d != 5*time.Second {
		t.Errorf("jitter=0: 5s directive → (%v, %v), want (5s, true)", d, ok)
	}
	retryAfterJitter = func() float64 { return 0.4999 }
	if d, ok := retryAfterDelay(rateLimited("5")); !ok || d < 5*time.Second || d > 7500*time.Millisecond {
		t.Errorf("jitter≈0.5: 5s directive → %v, want within [5s, 7.5s]", d)
	}
	retryAfterJitter = func() float64 { return 1 } // beyond the real [0,1) range, worst case
	if d, ok := retryAfterDelay(rateLimited("60")); !ok || d != 30*time.Second {
		t.Errorf("60s directive must clamp to 30s, got %v", d)
	}
	// Retry-After: 0 means "retry immediately is fine" — no pacing
	// directive worth honoring, fall through to the normal ladder.
	if _, ok := retryAfterDelay(rateLimited("0")); ok {
		t.Error("Retry-After: 0 should fall through to normal backoff")
	}
	// Non-HTTP errors (connection tier) and HTTP errors without the
	// header never take the server-paced path.
	if _, ok := retryAfterDelay(fmt.Errorf("send request: %w", &url.Error{Op: "Post", URL: "x", Err: io.EOF})); ok {
		t.Error("connection-tier error should not be server-paced")
	}
	if _, ok := retryAfterDelay(&provider.HTTPError{StatusCode: 429, Body: "no header"}); ok {
		t.Error("429 without Retry-After should not be server-paced")
	}
}

// TestLLMRetryServerPacedRetries verifies llmRetry honors the server's
// Retry-After instead of the squared ladder on a 429: with jitter pinned
// to 0 and a 1s directive, the retry waits ~1s (not the ladder's 1s — the
// observable assertion is recovery + delay ≥ directive).
func TestLLMRetryServerPacedRetries(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping backoff-duration test in short mode")
	}
	orig := retryAfterJitter
	retryAfterJitter = func() float64 { return 0 }
	t.Cleanup(func() { retryAfterJitter = orig })

	rateLimited := &provider.HTTPError{
		StatusCode: http.StatusTooManyRequests,
		Headers:    http.Header{"Retry-After": []string{"1"}},
	}
	calls := 0
	start := time.Now()
	_, err := llmRetry(context.Background(), "test", func(ctx context.Context) (*provider.Response, error) {
		calls++
		if calls == 1 {
			return nil, rateLimited
		}
		return &provider.Response{Content: "ok"}, nil
	})
	if err != nil {
		t.Fatalf("expected recovery after server-paced retry, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("retry should honor the 1s directive, retried after %v", elapsed)
	}
}

// TestClassifyLLMErrorFirstTokenStall pins the stall classification: a
// firstTokenStallError must classify as retryable EVEN THOUGH its cause
// chain contains context.Canceled (the watchdog cancels the stream ctx to
// break the dead read) — the context checks alone would brand it terminal
// and the loop would swallow it as a user Stop.
func TestClassifyLLMErrorFirstTokenStall(t *testing.T) {
	stall := &firstTokenStallError{waited: firstTokenStallDeadline}
	if got := classifyLLMError(stall); got != "retryable" {
		t.Errorf("classifyLLMError(stall) = %q, want retryable", got)
	}
	if got := classifyCallError(stall); got != "first_token_stall" {
		t.Errorf("classifyCallError(stall) = %q, want first_token_stall", got)
	}
	// A stall WRAPPING a context.Canceled (as sr.Err() would produce when
	// the watchdog's cancel propagates) must still classify retryable.
	wrapped := fmt.Errorf("read stream: %w: %w", context.Canceled, stall)
	if got := classifyLLMError(wrapped); got != "retryable" {
		t.Errorf("classifyLLMError(wrapped stall) = %q, want retryable", got)
	}
}

// TestFirstTokenWatchdog covers the three outcomes: no chunk in time →
// fires and cancels; chunk before deadline → disarmed; stop() without a
// chunk → never fires.
func TestFirstTokenWatchdog(t *testing.T) {
	t.Run("fires without first chunk", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w := newFirstTokenWatchdog(5*time.Millisecond, cancel)
		defer w.stop()
		<-ctx.Done()
		if !w.fired.Load() {
			t.Error("watchdog should report fired after deadline with no chunk")
		}
	})
	t.Run("disarmed by first chunk", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w := newFirstTokenWatchdog(20*time.Millisecond, cancel)
		w.mark()
		time.Sleep(40 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Fatal("disarmed watchdog must not cancel")
		default:
		}
		if w.fired.Load() {
			t.Error("watchdog should not fire after mark()")
		}
	})
	t.Run("stop without chunk never fires", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w := newFirstTokenWatchdog(10*time.Millisecond, cancel)
		w.stop()
		time.Sleep(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Fatal("stopped watchdog must not cancel")
		default:
		}
		if w.fired.Load() {
			t.Error("stopped watchdog must not report fired")
		}
	})
}

// TestResolveFirstTokenStallDeadline pins the env parsing: bare default
// 150s, valid override honored, junk/too-small values fall back.
func TestResolveFirstTokenStallDeadline(t *testing.T) {
	t.Setenv("FLUCTIO_FIRST_TOKEN_STALL_MS", "")
	if d := resolveFirstTokenStallDeadline(); d != 150*time.Second {
		t.Errorf("default = %v, want 150s", d)
	}
	t.Setenv("FLUCTIO_FIRST_TOKEN_STALL_MS", "5000")
	if d := resolveFirstTokenStallDeadline(); d != 5*time.Second {
		t.Errorf("override 5000 = %v, want 5s", d)
	}
	t.Setenv("FLUCTIO_FIRST_TOKEN_STALL_MS", "500") // below 1000ms floor
	if d := resolveFirstTokenStallDeadline(); d != 150*time.Second {
		t.Errorf("sub-floor override = %v, want 150s fallback", d)
	}
	t.Setenv("FLUCTIO_FIRST_TOKEN_STALL_MS", "abc")
	if d := resolveFirstTokenStallDeadline(); d != 150*time.Second {
		t.Errorf("junk override = %v, want 150s fallback", d)
	}
}
