// Package httpclient brands every outbound HTTP request this process
// makes with a Fluctio Agent User-Agent, so external APIs see
// "fluctio-agent/<version>" instead of Go's default
// "Go-http-client/1.1".
//
// Coverage strategy:
//   - Install() swaps http.DefaultTransport for a UA-injecting wrapper.
//     Clients created as &http.Client{} (nil Transport) resolve
//     DefaultTransport at request time, so they pick the wrapper up
//     automatically — that's the bulk of the process's outbound calls.
//   - Call sites that build their own *http.Transport (or clone it)
//     must wrap explicitly via Wrap().
package httpclient

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/fluctio-ai/fluctio/internal/buildinfo"
)

// userAgent is built once. buildinfo.Version/Commit are package vars the
// Makefile stamps at link time; reading them here at import fixes the UA
// for the process. The string is immutable after construction.
var userAgent atomic.Value

func init() {
	ua := "fluctio-agent/" + buildinfo.Version
	if buildinfo.Commit != "" && buildinfo.Commit != "unknown" {
		ua += " (" + buildinfo.Commit + ")"
	}
	userAgent.Store(ua)
}

// UserAgent returns the branded UA string, e.g.
// "fluctio-agent/v0.4.2 (a1b2c3d)".
func UserAgent() string {
	return userAgent.Load().(string)
}

// fetchUserAgent is the browser-shaped UA, built once like userAgent.
var fetchUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Fluctio/" + buildinfo.Version

// FetchUserAgent returns a browser-shaped UA for fetching web content
// (web_fetch, KB URL ingestion): sites and CDNs commonly block
// self-declared bot agents outright but ignore unknown trailing UA
// tokens, so the string looks like a stock Chrome request and appends
// "Fluctio/<version>" at the end for honest identification. API-type
// endpoints (LLM providers, channel bots, search APIs) should keep
// using UserAgent().
func FetchUserAgent() string {
	return fetchUserAgent
}

// uaTransport sets a User-Agent on every request unless the caller already
// set one (some SDKs / auth flows provide their own). Clones the request
// before mutating headers so the caller's reused request isn't raced.
type uaTransport struct {
	ua   string
	base http.RoundTripper
}

func (t uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if _, ok := req.Header["User-Agent"]; !ok {
		clone := req.Clone(req.Context())
		clone.Header.Set("User-Agent", t.ua)
		req = clone
	}
	return t.base.RoundTrip(req)
}

// wrapWith returns a RoundTripper that injects ua on top of base. nil
// base falls back to http.DefaultTransport's current value (captured at
// call time).
func wrapWith(base http.RoundTripper, ua string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return uaTransport{ua: ua, base: base}
}

// Wrap returns a RoundTripper that injects the branded User-Agent on top
// of base. nil base falls back to http.DefaultTransport's current value
// (captured at call time).
func Wrap(base http.RoundTripper) http.RoundTripper {
	return wrapWith(base, UserAgent())
}

// Install replaces http.DefaultTransport with a UA-injecting wrapper so
// every nil-Transport *http.Client in the process is branded. Call once,
// as early as possible in main (before any client is constructed).
func Install() {
	http.DefaultTransport = Wrap(http.DefaultTransport)
}

// NewClient returns an *http.Client whose transport is a UA-wrapped clone
// of http.DefaultTransport. Use for ad-hoc outbound calls that want a
// per-call timeout without losing the UA branding.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: Wrap(CloneDefault()),
	}
}

// NewStreamingClient is like NewClient but lets the caller set a
// ResponseHeaderTimeout on the underlying *http.Transport (streaming
// endpoints hang mid-request, so a per-call timeout is wrong; a header
// timeout catches a wedged connection without killing live streams).
//
// This is the safe way to get a branded client with transport tuning —
// callers must NOT type-assert http.DefaultTransport themselves, because
// Install() replaces it with a UA wrapper.
func NewStreamingClient(headerTimeout time.Duration) *http.Client {
	tr := CloneDefault()
	tr.ResponseHeaderTimeout = headerTimeout
	return &http.Client{Transport: Wrap(tr)}
}

// CloneDefault returns a fresh *http.Transport with Go's sensible default
// tuning, independent of whatever http.DefaultTransport currently points
// at (so wrapping DefaultTransport doesn't recurse). Exported for call
// sites that need transport tuning (e.g. ResponseHeaderTimeout, forced
// HTTP/1.1) and therefore can't type-assert http.DefaultTransport
// themselves — after Install() it's a uaTransport, and asserting *http.Transport
// there panics.
func CloneDefault() *http.Transport {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	// DefaultTransport was already replaced (e.g. Install ran). Build a
	// plain transport with the same defaults Go ships with.
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}
