package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// QQ access-token lifecycle (contract §3.2 + §6.7).
//
// This file owns:
//   - The OAuth2 client_credentials fetch (POST bots.qq.com/app/
//     getAppAccessToken with {appId, clientSecret}).
//   - A process-wide, per-appID in-memory cache (sync.Map).
//   - singleflight.Group so concurrent callers sharing an appID only
//     fire one HTTP request (contract §3.2: "同 appId 并发过期只发一次").
//   - A background refresh goroutine per cached entry that re-fetches
//     `qqTokenRefreshLead` before expiry (contract §6.7: WS open →
//     immediately start refresh so long connections never see a stale
//     token). On failure, retries every `qqTokenRetryInterval`.
//   - qqWithTokenRetry: wraps a REST call so a 401 / token-error body
//     clears the cache and replays once with a fresh token (contract
//     §3.2: "401 / token 字样错误 → clearTokenCache(appId) 后重试一次").
//
// Phase 1 of the QQ adapter re-fetched the token on every reconnect
// loop. Phase 2 swaps that out for this module; the public entry point
// QQChannel.getAccessToken delegates to qqGetToken.

// ---------------------------------------------------------------------------
// Tunables (contract §3.2)
// ---------------------------------------------------------------------------

const (
	// qqTokenRefreshLead is how long before expiry the background refresh
	// fires. Contract §3.2 says 5min (openclaw adds ±30s jitter; we omit
	// the jitter — singleflight + 5s retry covers the same race window).
	qqTokenRefreshLead = 5 * time.Minute

	// qqTokenRetryInterval is the delay between refresh retries on fetch
	// failure (contract §3.2: 5s).
	qqTokenRetryInterval = 5 * time.Second

	// qqTokenDefaultExpiresIn is the assumed lifetime when the API
	// response omits expires_in (contract §3.2: default 7200s = 2h).
	qqTokenDefaultExpiresIn = 7200
)

// qqTokenFetchFailureThreshold is the number of consecutive token-
// fetch failures in Start that trip fireFailed("token_refresh_failed").
// Keeps the adapter from looping forever on a mis-typed secret. A var
// (not a const) so tests with the production backoff ladder can lower
// it (the ladder sums to ~18s before the threshold trips, which is
// awkward to exercise in `go test`).
var qqTokenFetchFailureThreshold = 5

// ---------------------------------------------------------------------------
// Fetcher (swappable for tests)
// ---------------------------------------------------------------------------

// qqTokenFetcher performs the actual OAuth2 POST. The default
// implementation (qqDefaultTokenFetch) hits bots.qq.com; tests swap
// qqTokenFetch to stub the network.
type qqTokenFetcher func(ctx context.Context, appID, secret string) (token string, expiresIn int, err error)

// qqTokenFetch is the active fetcher. Tests swap this; production code
// reads it through qqTokenFetchMu so the race detector stays happy.
var (
	qqTokenFetch    qqTokenFetcher = qqDefaultTokenFetch
	qqTokenFetchMu  sync.Mutex
)

// ---------------------------------------------------------------------------
// Cache + singleflight (package-scoped, keyed by appID)
// ---------------------------------------------------------------------------

// qqTokenEntry is one cached token plus the handle that stops its
// background refresh goroutine.
type qqTokenEntry struct {
	token         string
	expiresAt     time.Time
	refreshCancel context.CancelFunc // cancels the refresh goroutine
}

var (
	// qqTokenCache maps appID -> *qqTokenEntry. Shared across all
	// QQChannel instances so multiple channels on the same appID (and
	// reconnect cycles of one channel) share the token + refresh loop.
	qqTokenCache sync.Map

	// qqTokenSF deduplicates concurrent fetches for the same appID.
	qqTokenSF singleflight.Group
)

// qqGetToken returns a live access token for appID. Cache hit returns
// immediately; otherwise singleflight fires one fetch and every
// concurrent waiter shares the result. On success a background refresh
// goroutine is running that will re-fetch before expiry.
func qqGetToken(ctx context.Context, appID, secret string) (string, error) {
	now := time.Now()
	if v, ok := qqTokenCache.Load(appID); ok {
		if e, ok := v.(*qqTokenEntry); ok && now.Before(e.expiresAt.Add(-qqTokenRefreshLead)) {
			return e.token, nil
		}
	}

	// Cache miss or near-expiry — single-flight the refresh so N
	// concurrent callers fire exactly one HTTP request.
	_, err, _ := qqTokenSF.Do(appID, func() (any, error) {
		// Re-check inside the winner: a concurrent refresh may have
		// populated the cache while we were waiting on the flight.
		if v, ok := qqTokenCache.Load(appID); ok {
			if e, ok := v.(*qqTokenEntry); ok && now.Before(e.expiresAt.Add(-qqTokenRefreshLead)) {
				return e, nil
			}
		}
		return qqRefreshToken(ctx, appID, secret)
	})
	if err != nil {
		return "", err
	}

	// Read the result. The singleflight winner stored it; waiters see
	// the same entry.
	if v, ok := qqTokenCache.Load(appID); ok {
		if e, ok := v.(*qqTokenEntry); ok {
			return e.token, nil
		}
	}
	return "", fmt.Errorf("qq token: cache empty after refresh for %q", appID)
}

// qqRefreshToken performs one fetch and replaces the cache entry. On
// success it stops the previous refresh goroutine and starts a new one
// (contract §6.7). On failure the cache is left untouched so callers
// can still read a stale-but-unexpired token.
func qqRefreshToken(ctx context.Context, appID, secret string) (*qqTokenEntry, error) {
	qqTokenFetchMu.Lock()
	fetch := qqTokenFetch
	qqTokenFetchMu.Unlock()

	token, expiresIn, err := fetch(ctx, appID, secret)
	if err != nil {
		return nil, err
	}
	if expiresIn <= 0 {
		expiresIn = qqTokenDefaultExpiresIn
	}
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)

	// Supersede the previous entry + cancel its refresh goroutine.
	if v, ok := qqTokenCache.LoadAndDelete(appID); ok {
		if e, ok := v.(*qqTokenEntry); ok && e.refreshCancel != nil {
			e.refreshCancel()
		}
	}

	refreshCtx, refreshCancel := context.WithCancel(context.Background())
	entry := &qqTokenEntry{
		token:         token,
		expiresAt:     expiresAt,
		refreshCancel: refreshCancel,
	}
	qqTokenCache.Store(appID, entry)

	slog.Debug("qq token refreshed",
		"appId", appID,
		"expiresIn", expiresIn,
		"expiresAt", expiresAt.Format(time.RFC3339))

	// Contract §6.7: refresh loop must run for as long as the cache
	// holds a live token, so a long-lived WS connection never sees a
	// 401 on an idle token (heartbeat / typing / etc.).
	go qqTokenRefreshLoop(refreshCtx, appID, secret, expiresAt)
	return entry, nil
}

// qqTokenRefreshLoop blocks until one refresh is due (expiresAt minus
// qqTokenRefreshLead) then fires one refresh via singleflight. On
// failure, retries every qqTokenRetryInterval. Exits when ctx is
// cancelled (superseded by a newer entry, or shutdown).
//
// One goroutine per cached entry. When qqRefreshToken replaces an
// entry it cancels the previous entry's context, which terminates the
// previous loop.
func qqTokenRefreshLoop(ctx context.Context, appID, secret string, expiresAt time.Time) {
	wait := time.Until(expiresAt.Add(-qqTokenRefreshLead))
	if wait < 0 {
		wait = 0
	}
	t := time.NewTimer(wait)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// If we've been superseded (another refresh won and
			// replaced the entry), exit quietly — the new entry's
			// loop takes over.
			v, ok := qqTokenCache.Load(appID)
			if !ok {
				return
			}
			e, ok := v.(*qqTokenEntry)
			if !ok || !e.expiresAt.Equal(expiresAt) {
				return
			}

			_, err, _ := qqTokenSF.Do(appID, func() (any, error) {
				return qqRefreshToken(ctx, appID, secret)
			})
			if err != nil {
				slog.Warn("qq token background refresh failed — retrying",
					"appId", appID,
					"retryIn", qqTokenRetryInterval,
					"error", err)
				t.Reset(qqTokenRetryInterval)
				continue
			}
			// Success — the new entry's goroutine takes over.
			return
		}
	}
}

// qqClearToken removes the cached token for appID and stops the
// background refresh goroutine. Called by qqWithTokenRetry on 401 and
// on adapter shutdown. The row in qqTokenSF is forgotten too so the
// next qqGetToken starts a fresh flight instead of sharing an in-
// flight one whose result we just invalidated.
func qqClearToken(appID string) {
	if v, ok := qqTokenCache.LoadAndDelete(appID); ok {
		if e, ok := v.(*qqTokenEntry); ok && e.refreshCancel != nil {
			e.refreshCancel()
		}
	}
	qqTokenSF.Forget(appID)
}

// ---------------------------------------------------------------------------
// 401 / token-error retry wrapper (contract §3.2)
// ---------------------------------------------------------------------------

// qqWithTokenRetry wraps a REST call so that a 401 or token-error body
// clears the cache and replays once with a fresh token. `do` receives
// the token and returns the HTTP response, body bytes, and any
// transport-level error. Network errors are not retried (a new token
// won't fix them); only token-rejection triggers the retry.
//
// Used by Phase 3 outbound calls (Send / SendMessage / SendTyping);
// exposed here so every QQ REST path shares the same semantics.
func qqWithTokenRetry(
	ctx context.Context,
	appID, secret string,
	do func(token string) (*http.Response, []byte, error),
) (*http.Response, []byte, error) {
	token, err := qqGetToken(ctx, appID, secret)
	if err != nil {
		return nil, nil, fmt.Errorf("qq token: %w", err)
	}
	resp, body, err := do(token)
	if err != nil {
		return resp, body, err
	}
	if !qqIsTokenError(resp, body) {
		return resp, body, nil
	}

	slog.Info("qq request rejected token — clearing cache and retrying once",
		"appId", appID, "status", respStatusCode(resp))
	qqClearToken(appID)

	token, err = qqGetToken(ctx, appID, secret)
	if err != nil {
		return resp, body, fmt.Errorf("qq token retry: %w", err)
	}
	return do(token)
}

// qqIsTokenError reports whether the response indicates the token was
// rejected. Contract §3.2: HTTP 401 OR body matches a token-error
// signal. Contract §3.7 lists 11244 (InputNotify token expired) as the
// explicit code we must handle.
func qqIsTokenError(resp *http.Response, body []byte) bool {
	if resp != nil && resp.StatusCode == http.StatusUnauthorized {
		return true
	}
	if len(body) == 0 {
		return false
	}
	s := string(body)
	// Contract §3.7 code 11244 (InputNotify token expired). Allow
	// both compact and pretty-printed JSON spacing.
	if strings.Contains(s, `"code":11244`) || strings.Contains(s, `"code": 11244`) {
		return true
	}
	lower := strings.ToLower(s)
	// Phrasing seen in QQ token-related errors. "expired" / "invalid"
	// co-occurring with "token" is a strong signal.
	if strings.Contains(lower, "token") &&
		(strings.Contains(lower, "expired") || strings.Contains(lower, "invalid")) {
		return true
	}
	return false
}

// respStatusCode safely extracts the status code from a possibly-nil
// response (the retry path may see a nil resp if the first call failed
// at the transport layer, though we don't retry that case).
func respStatusCode(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// ---------------------------------------------------------------------------
// Default fetcher (live HTTP)
// ---------------------------------------------------------------------------

// qqDefaultTokenFetch POSTs the OAuth2 client_credentials request to
// bots.qq.com/app/getAppAccessToken per contract §3.2. Returns the
// access_token + expires_in (seconds).
func qqDefaultTokenFetch(ctx context.Context, appID, secret string) (string, int, error) {
	body, _ := json.Marshal(map[string]string{
		"appId":        appID,
		"clientSecret": secret,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qqTokenURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", qqUserAgent)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("qq token http: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("qq token read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("qq token HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed qqAccessTokenResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", 0, fmt.Errorf("qq token parse: %w", err)
	}
	if parsed.AccessToken == "" {
		return "", 0, fmt.Errorf("qq token empty in response")
	}
	return parsed.AccessToken, int(parsed.ExpiresIn), nil
}

// ---------------------------------------------------------------------------
// Test helpers (only safe for single-threaded test setup)
// ---------------------------------------------------------------------------

// qqResetTokenCacheForTest clears all token cache state and restores
// the default fetcher. Test-only — production code never needs to
// reset the cache globally.
func qqResetTokenCacheForTest() {
	qqTokenCache.Range(func(k, _ any) bool {
		qqClearToken(k.(string))
		return true
	})
	qqTokenFetchMu.Lock()
	qqTokenFetch = qqDefaultTokenFetch
	qqTokenFetchMu.Unlock()
}
