package httpclient

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

// SSRF-safe outbound fetching, shared by every path that fetches a
// chatter/LLM-chosen URL: the built-in web_fetch tool, the "direct"
// webfetch tool-provider chain, KB URL ingestion / bookmarks, and
// attachment URL downloads. The check runs at DIAL time, after DNS has
// resolved, so a hostname pointing at 169.254.169.254 (cloud metadata),
// an RFC1918 address, or a DNS-rebinding trick all get stopped; we then
// dial the resolved IP directly instead of letting net.Dial re-resolve,
// closing the TOCTOU between validation and connection. Redirects stay
// inside the same transport, so every hop re-validates.

// IsBlockedAddr reports whether ip is a target outbound fetches must
// never reach: loopback, link-local (incl. 169.254.169.254 cloud
// metadata), private ranges, CGNAT, multicast, unspecified. Loopback is
// exempt when FLUCTIO_ALLOW_UNSAFE_LOOPBACK_FETCH=1 — the explicit
// escape hatch for operators who genuinely need fetches to reach a
// local intranet service, and for tests running against httptest
// servers on 127.0.0.1. Everything else stays blocked regardless.
func IsBlockedAddr(ip net.IP) bool {
	if ip.IsLoopback() && loopbackFetchAllowed() {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip.IsPrivate() { // 10/8, 172.16/12, 192.168/16, fc00::/7
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// 100.64.0.0/10 — CGNAT, can route to internal infra at some providers
		if ip4[0] == 100 && ip4[1]&0xc0 == 0x40 {
			return true
		}
		// 169.254/16 is covered by IsLinkLocalUnicast, but AWS/GCP metadata
		// uses 169.254.169.254 specifically; spelled out as a guard for
		// readers and as belt-and-suspenders if a future Go release ever
		// narrows IsLinkLocalUnicast.
		if ip4[0] == 169 && ip4[1] == 254 {
			return true
		}
	}
	return false
}

// loopbackFetchAllowed is read per-call (not cached at init) so tests
// can toggle it with t.Setenv.
func loopbackFetchAllowed() bool {
	v, _ := strconv.ParseBool(os.Getenv("FLUCTIO_ALLOW_UNSAFE_LOOPBACK_FETCH"))
	return v
}

// SafeDialContext resolves host, rejects every resolved address that
// IsBlockedAddr, and dials the first validated IP.
func SafeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses for %s", host)
	}
	for _, ip := range ips {
		if IsBlockedAddr(ip.IP) {
			return nil, fmt.Errorf("blocked address %s for host %s", ip.IP, host)
		}
	}
	// Dial the first IP we already validated; passing host:port back to
	// net.Dialer would do a second resolution and an attacker controlling
	// authoritative DNS could swap in 169.254.169.254 between our check
	// and the dial.
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}

// SafeClient returns an *http.Client whose dialer and redirect policy
// apply the SSRF guards above, with the branded User-Agent via Wrap.
// timeout is the whole-request cap; 0 means no cap (caller supplies a
// context deadline).
func SafeClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: Wrap(&http.Transport{
			DialContext:           SafeDialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			IdleConnTimeout:       60 * time.Second,
		}),
		// Cap redirect chains so an attacker can't follow a public URL
		// into an internal one — each redirect target goes back through
		// SafeDialContext, and bounded depth keeps the request finite.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
}
