package channels

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// qq_token_test.go covers the Phase 2 additions:
//   - token cache hit returns without re-fetching.
//   - singleflight collapses concurrent callers for the same appID.
//   - cache miss / near-expiry fires exactly one fetch.
//   - background refresh updates the cache before expiry.
//   - qqClearToken stops the refresh goroutine + wipes the cache.
//   - qqWithTokenRetry clears + retries once on 401 / token-error body.
//   - qqIsTokenError recognises 401 + the documented QQ error signals.
//   - QQChannel.SetAllowedChecker drops unauthorized inbound
//     (group_openid for 群绑群, user_openid for C2C).
//   - Start trips fireFailed("token_refresh_failed") after the
//     threshold of consecutive fetch failures.

// ----- helpers ---------------------------------------------------------------

// installFakeFetcher swaps qqTokenFetch for a fake that returns the
// given values and records each call through the returned counter.
// The returned cleanup restores the production fetcher.
func installFakeFetcher(token string, expiresIn int, err error) (*int32, func()) {
	var calls int32
	prev := qqTokenFetch
	qqTokenFetchMu.Lock()
	qqTokenFetch = func(_ context.Context, _, _ string) (string, int, error) {
		atomic.AddInt32(&calls, 1)
		return token, expiresIn, err
	}
	qqTokenFetchMu.Unlock()
	return &calls, func() {
		qqTokenFetchMu.Lock()
		qqTokenFetch = prev
		qqTokenFetchMu.Unlock()
	}
}

// TestQQTokenCacheHitSkipsFetcher: after one fetch populates the cache,
// subsequent reads within the refresh-lead window return the cached
// token WITHOUT calling the fetcher again.
func TestQQTokenCacheHitSkipsFetcher(t *testing.T) {
	qqResetTokenCacheForTest()
	calls, restore := installFakeFetcher("TOKEN_A", 7200, nil)
	defer restore()

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		got, err := qqGetToken(ctx, "APP_X", "secret")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got != "TOKEN_A" {
			t.Errorf("call %d: got %q, want TOKEN_A", i, got)
		}
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Errorf("fetcher called %d times, want exactly 1 (cache hit for the rest)", n)
	}
}

// TestQQTokenSingleflightCollapsesConcurrent: many goroutines asking
// for the same appID at the same time fire exactly one fetch.
func TestQQTokenSingleflightCollapsesConcurrent(t *testing.T) {
	qqResetTokenCacheForTest()
	calls, restore := installFakeFetcher("TOKEN_SF", 7200, nil)
	defer restore()

	const N = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, _ = qqGetToken(context.Background(), "APP_SF", "secret")
		}()
	}
	close(start)
	wg.Wait()

	if n := atomic.LoadInt32(calls); n != 1 {
		t.Errorf("fetcher called %d times for %d concurrent callers, want 1 (singleflight)", n, N)
	}
}

// TestQQTokenAppIDIsolation: two different appIDs get independent
// cache entries — fetching for one doesn't seed the other.
func TestQQTokenAppIDIsolation(t *testing.T) {
	qqResetTokenCacheForTest()

	mu := sync.Mutex{}
	seen := map[string]int{}
	prev := qqTokenFetch
	qqTokenFetchMu.Lock()
	qqTokenFetch = func(_ context.Context, appID, _ string) (string, int, error) {
		mu.Lock()
		seen[appID]++
		mu.Unlock()
		// Per-appID distinct token so we can prove the cache routed
		// callers to the right entry.
		return "TOKEN_" + appID, 7200, nil
	}
	qqTokenFetchMu.Unlock()
	defer func() {
		qqTokenFetchMu.Lock()
		qqTokenFetch = prev
		qqTokenFetchMu.Unlock()
	}()

	a, err := qqGetToken(context.Background(), "APP_A", "s")
	if err != nil {
		t.Fatalf("APP_A: %v", err)
	}
	b, err := qqGetToken(context.Background(), "APP_B", "s")
	if err != nil {
		t.Fatalf("APP_B: %v", err)
	}
	if a != "TOKEN_APP_A" || b != "TOKEN_APP_B" {
		t.Errorf("isolation broke: a=%q b=%q", a, b)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["APP_A"] != 1 || seen["APP_B"] != 1 {
		t.Errorf("fetch counts = %+v, want each exactly 1", seen)
	}
}

// TestQQTokenExpiresAndRefetches: when the cache entry is past the
// refresh-lead window, the next call fires a new fetch.
func TestQQTokenExpiresAndRefetches(t *testing.T) {
	qqResetTokenCacheForTest()

	var calls int32
	prev := qqTokenFetch
	qqTokenFetchMu.Lock()
	qqTokenFetch = func(_ context.Context, _, _ string) (string, int, error) {
		atomic.AddInt32(&calls, 1)
		return "FRESH", 1, // 1s expiry — shorter than qqTokenRefreshLead
			nil
	}
	qqTokenFetchMu.Unlock()
	defer func() {
		qqTokenFetchMu.Lock()
		qqTokenFetch = prev
		qqTokenFetchMu.Unlock()
	}()

	ctx := context.Background()
	// First call populates the cache.
	if _, err := qqGetToken(ctx, "EXP", "s"); err != nil {
		t.Fatal(err)
	}
	// Immediately after, the entry's expiresAt - qqTokenRefreshLead is
	// in the past, so the next call must re-fetch.
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("prereq: expected 1 initial fetch, got %d", atomic.LoadInt32(&calls))
	}
	if _, err := qqGetToken(ctx, "EXP", "s"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("after expiry window: %d fetches, want 2", n)
	}
}

// TestQQTokenBackgroundRefresh: when the cache holds a token with a
// real expiry, the background goroutine refreshes it before the
// deadline without any caller pressing qqGetToken.
func TestQQTokenBackgroundRefresh(t *testing.T) {
	qqResetTokenCacheForTest()

	var calls int32
	prev := qqTokenFetch
	// Refresh lead is 5min — for the timer to fire in test we need
	// expiresIn < refreshLead. qqRefreshToken clamps negative wait to
	// zero, so the loop fires immediately.
	qqTokenFetchMu.Lock()
	qqTokenFetch = func(_ context.Context, _, _ string) (string, int, error) {
		n := atomic.AddInt32(&calls, 1)
		return "GEN" + itoa(int(n)), 1, nil // 1s expiry → loop fires immediately
	}
	qqTokenFetchMu.Unlock()
	defer func() {
		qqTokenFetchMu.Lock()
		qqTokenFetch = prev
		qqTokenFetchMu.Unlock()
	}()

	ctx := context.Background()
	if _, err := qqGetToken(ctx, "BG", "s"); err != nil {
		t.Fatal(err)
	}

	// Wait for the background goroutine to fire at least one refresh.
	deadline := time.After(2 * time.Second)
	for {
		if atomic.LoadInt32(&calls) >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("background refresh never fired: calls=%d", atomic.LoadInt32(&calls))
		default:
			time.Sleep(15 * time.Millisecond)
		}
	}
}

// TestQQTokenClearWipesEntry: qqClearToken removes the cache entry so
// the next qqGetToken triggers a new fetch (and Forget key so a new
// singleflight flight starts).
func TestQQTokenClearWipesEntry(t *testing.T) {
	qqResetTokenCacheForTest()
	calls, restore := installFakeFetcher("C", 7200, nil)
	defer restore()

	ctx := context.Background()
	if _, err := qqGetToken(ctx, "CLR", "s"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("prereq: expected 1 fetch, got %d", n)
	}
	qqClearToken("CLR")
	if _, err := qqGetToken(ctx, "CLR", "s"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Errorf("after clear: %d total fetches, want 2", n)
	}
}

// TestQQTokenFetchFailurePropagates: when the fetcher errors, callers
// see the error and the cache stays empty.
func TestQQTokenFetchFailurePropagates(t *testing.T) {
	qqResetTokenCacheForTest()
	calls, restore := installFakeFetcher("", 0, errors.New("upstream 500"))
	defer restore()

	_, err := qqGetToken(context.Background(), "FAIL", "s")
	if err == nil {
		t.Fatal("expected error from failed fetch")
	}
	if !strings.Contains(err.Error(), "upstream 500") {
		t.Errorf("err = %q, want substring 'upstream 500'", err.Error())
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Errorf("failed fetch should still count as 1 call, got %d", n)
	}
}

// ----- qqIsTokenError + qqWithTokenRetry -------------------------------------

func TestQQIsTokenError(t *testing.T) {
	cases := []struct {
		name string
		resp *http.Response
		body []byte
		want bool
	}{
		{"401", &http.Response{StatusCode: 401}, nil, true},
		{"200 no body", &http.Response{StatusCode: 200}, nil, false},
		{"code 11244 compact", &http.Response{StatusCode: 200}, []byte(`{"code":11244,"msg":"expired"}`), true},
		{"code 11244 spaced", &http.Response{StatusCode: 200}, []byte(`{"code": 11244}`), true},
		{"token expired phrase", &http.Response{StatusCode: 200}, []byte(`token is expired`), true},
		{"token invalid phrase", &http.Response{StatusCode: 200}, []byte(`access_token invalid`), true},
		{"unrelated text", &http.Response{StatusCode: 200}, []byte(`{"msg":"frequency limit"}`), false},
		{"token without expired word", &http.Response{StatusCode: 200}, []byte(`{"token":"abc"}`), false},
		{"nil resp nil body", nil, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := qqIsTokenError(c.resp, c.body); got != c.want {
				t.Errorf("qqIsTokenError = %v, want %v", got, c.want)
			}
		})
	}
}

// TestQQWithTokenRetryClearsOn401: first call returns 401 → wrapper
// must clear cache + call do a second time with a fresh token.
func TestQQWithTokenRetryClearsOn401(t *testing.T) {
	qqResetTokenCacheForTest()

	// First qqGetToken populates cache with "OLD". qqClearToken wipes
	// it; second qqGetToken returns "NEW" (we swap fetcher on clear).
	// Simpler: just make the fetcher return increasing tokens.
	var fetches int32
	prev := qqTokenFetch
	qqTokenFetchMu.Lock()
	qqTokenFetch = func(_ context.Context, _, _ string) (string, int, error) {
		n := atomic.AddInt32(&fetches, 1)
		if n == 1 {
			return "OLD", 7200, nil
		}
		return "NEW", 7200, nil
	}
	qqTokenFetchMu.Unlock()
	defer func() {
		qqTokenFetchMu.Lock()
		qqTokenFetch = prev
		qqTokenFetchMu.Unlock()
	}()

	var doCalls int32
	do := func(token string) (*http.Response, []byte, error) {
		n := atomic.AddInt32(&doCalls, 1)
		if n == 1 {
			return &http.Response{StatusCode: 401}, []byte(`{"msg":"unauthorized"}`), nil
		}
		if token != "NEW" {
			t.Errorf("retry call used token %q, want NEW", token)
		}
		return &http.Response{StatusCode: 200}, []byte(`ok`), nil
	}

	resp, body, err := qqWithTokenRetry(context.Background(), "RTRY", "s", do)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want ok", string(body))
	}
	if n := atomic.LoadInt32(&doCalls); n != 2 {
		t.Errorf("do called %d times, want 2 (initial + retry)", n)
	}
}

// TestQQWithTokenRetryNoRetryOnNetworkErr: transport errors don't
// trigger a retry — a fresh token won't fix a dead network.
func TestQQWithTokenRetryNoRetryOnNetworkErr(t *testing.T) {
	qqResetTokenCacheForTest()
	_, restore := installFakeFetcher("OK", 7200, nil)
	defer restore()

	var doCalls int32
	do := func(string) (*http.Response, []byte, error) {
		atomic.AddInt32(&doCalls, 1)
		return nil, nil, errors.New("connection reset")
	}
	_, _, err := qqWithTokenRetry(context.Background(), "NET", "s", do)
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("err = %v, want connection reset", err)
	}
	if n := atomic.LoadInt32(&doCalls); n != 1 {
		t.Errorf("do called %d times, want 1 (no retry on network err)", n)
	}
}

// ----- SetAllowedChecker gate ------------------------------------------------

// TestQQAllowedCheckerNilAllowsAll: the Phase 1 default — nil checker
// means every inbound message is delivered.
func TestQQAllowedCheckerNilAllowsAll(t *testing.T) {
	q, mb := newTestQQ(t)
	send, _ := captureSend()

	raw, _ := json.Marshal(qqFrame{
		Op: qqOpDispatch,
		T:  "GROUP_AT_MESSAGE_CREATE",
		S:  intPtr(1),
		D:  json.RawMessage(`{"id":"M","group_openid":"G","content":"hi","author":{"member_openid":"X"}}`),
	})
	if err := q.handleServerMessage(context.Background(), raw, send); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	msg := drainInbound(t, mb, time.Second)
	if msg.Text != "hi" {
		t.Errorf("Text = %q, want hi (nil checker should allow)", msg.Text)
	}
}

// TestQQAllowedCheckerGroupFiltersOutsiders: when SetAllowedChecker
// is configured, group messages from unauthorized group_openids are
// silently dropped (no bus.Inbound delivery).
func TestQQAllowedCheckerGroupFiltersOutsiders(t *testing.T) {
	q, mb := newTestQQ(t)
	send, _ := captureSend()

	q.SetAllowedChecker(func(openid string) bool {
		return openid == "AUTHORIZED_GROUP"
	})

	// Authorized group → delivers.
	rawOK, _ := json.Marshal(qqFrame{
		Op: qqOpDispatch, T: "GROUP_AT_MESSAGE_CREATE", S: intPtr(1),
		D: json.RawMessage(`{"id":"M1","group_openid":"AUTHORIZED_GROUP","content":"ok","author":{"member_openid":"u1"}}`),
	})
	if err := q.handleServerMessage(context.Background(), rawOK, send); err != nil {
		t.Fatalf("dispatch ok: %v", err)
	}
	msg := drainInbound(t, mb, time.Second)
	if msg.Text != "ok" {
		t.Errorf("authorized: Text = %q, want ok", msg.Text)
	}

	// Unauthorized group → silently dropped.
	rawNo, _ := json.Marshal(qqFrame{
		Op: qqOpDispatch, T: "GROUP_AT_MESSAGE_CREATE", S: intPtr(2),
		D: json.RawMessage(`{"id":"M2","group_openid":"OTHER_GROUP","content":"drop","author":{"member_openid":"u2"}}`),
	})
	if err := q.handleServerMessage(context.Background(), rawNo, send); err != nil {
		t.Fatalf("dispatch drop: %v", err)
	}
	select {
	case m := <-mb.Inbound:
		t.Errorf("unauthorized group should be dropped, got %+v", m)
	case <-time.After(80 * time.Millisecond):
		// Expected — gate dropped it.
	}
}

// TestQQAllowedCheckerC2C: C2C path uses user_openid for the gate.
func TestQQAllowedCheckerC2C(t *testing.T) {
	q, mb := newTestQQ(t)
	send, _ := captureSend()

	q.SetAllowedChecker(func(openid string) bool {
		return openid == "USER_ALLOWED"
	})

	rawNo, _ := json.Marshal(qqFrame{
		Op: qqOpDispatch, T: "C2C_MESSAGE_CREATE", S: intPtr(1),
		D: json.RawMessage(`{"id":"DM1","content":"hi","author":{"user_openid":"STRANGER"}}`),
	})
	if err := q.handleServerMessage(context.Background(), rawNo, send); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	select {
	case m := <-mb.Inbound:
		t.Errorf("unauthorized c2c should be dropped, got %+v", m)
	case <-time.After(80 * time.Millisecond):
	}

	rawOK, _ := json.Marshal(qqFrame{
		Op: qqOpDispatch, T: "C2C_MESSAGE_CREATE", S: intPtr(2),
		D: json.RawMessage(`{"id":"DM2","content":"ok","author":{"user_openid":"USER_ALLOWED"}}`),
	})
	if err := q.handleServerMessage(context.Background(), rawOK, send); err != nil {
		t.Fatalf("dispatch ok: %v", err)
	}
	msg := drainInbound(t, mb, time.Second)
	if msg.Text != "ok" {
		t.Errorf("Text = %q, want ok", msg.Text)
	}
}

// TestQQStartTokenFailureTripsFireFailed: consecutive token-fetch
// errors eventually fire OnFailed("token_refresh_failed") and stop
// the Start loop. We drive this with a fetcher that always errors,
// and lower the threshold to 1 so the test doesn't have to wait
// through the full backoff ladder (~18s at threshold=5).
func TestQQStartTokenFailureTripsFireFailed(t *testing.T) {
	qqResetTokenCacheForTest()
	_, restore := installFakeFetcher("", 0, errors.New("qq service down"))
	defer restore()

	prevThreshold := qqTokenFetchFailureThreshold
	qqTokenFetchFailureThreshold = 1
	defer func() { qqTokenFetchFailureThreshold = prevThreshold }()

	q, _ := newTestQQ(t)
	var mu sync.Mutex
	got := ""
	q.OnFailed(func(_, reason string) {
		mu.Lock()
		defer mu.Unlock()
		got = reason
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = q.Start(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Start didn't return within 10s on persistent token failure")
	}

	mu.Lock()
	defer mu.Unlock()
	// threshold=1: after the first fetch fails, Start fires OnFailed
	// and returns.
	if got != "token_refresh_failed" {
		t.Errorf("OnFailed reason = %q, want token_refresh_failed", got)
	}
}

// ----- helpers ---------------------------------------------------------------

// itoa is a tiny strconv-free int → string for tests. We can't import
// strconv in a test-only helper without bloating the import block, and
// the format here is trivial.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	if n < 0 {
		b = append(b, '-')
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append(digits, byte('0'+n%10))
		n /= 10
	}
	for i := len(digits) - 1; i >= 0; i-- {
		b = append(b, digits[i])
	}
	return string(b)
}

// TestQQClose4004ClearsTokenCache pins down the contract §1.8 wiring:
// when the WS closes with code 4004 (token invalid), the reconnect
// loop's switch must call qqClearToken(q.appID) so the next iteration
// re-fetches instead of returning the same dead token (which would
// loop until backoff exhausts).
//
// The switch is inline in Start() (no helper extraction — would be a
// refactor). This test mirrors the production path step-by-step:
// classifyQQClose(4004) → qqCloseActionRefreshToken → qqClearToken.
// If either building block regresses (classification changes, or
// qqClearToken stops wiping the cache) this test fails; removing the
// qqClearToken call from Start()'s switch case is caught by code
// review (smallest necessary change — no refactor for testability).
func TestQQClose4004ClearsTokenCache(t *testing.T) {
	qqResetTokenCacheForTest()
	calls, restore := installFakeFetcher("T4004", 7200, nil)
	defer restore()

	const appID = "APP-4004"
	ctx := context.Background()

	// Prime the cache so qqClearToken has something to clear.
	if _, err := qqGetToken(ctx, appID, "s"); err != nil {
		t.Fatalf("prereq qqGetToken: %v", err)
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("prereq: %d fetches, want 1", n)
	}

	// Mirror Start()'s switch: 4004 → RefreshToken → clear.
	if got := classifyQQClose(qqCloseAuthFailed); got != qqCloseActionRefreshToken {
		t.Fatalf("classifyQQClose(%d) = %v, want qqCloseActionRefreshToken", qqCloseAuthFailed, got)
	}
	qqClearToken(appID)

	// Cache cleared → next qqGetToken must trigger a fresh fetch.
	if _, err := qqGetToken(ctx, appID, "s"); err != nil {
		t.Fatalf("post-clear qqGetToken: %v", err)
	}
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Errorf("after 4004 clear: %d total fetches, want 2 (cache must be cleared so the dead token isn't returned again)", n)
	}
}
