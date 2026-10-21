package httpclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/buildinfo"
)

// Install swaps http.DefaultTransport, so plain &http.Client{} (nil
// Transport) and http.DefaultClient pick up the branded UA at request
// time — that's the bulk of the process's outbound calls. A caller-set
// User-Agent (QQ/Feishu SDKs, web_fetch) must be left untouched.
func TestInstallBrandsDefaultClients(t *testing.T) {
	Install()

	var got, gotExplicit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/explicit" {
			gotExplicit = r.UserAgent()
		} else {
			got = r.UserAgent()
		}
	}))
	defer srv.Close()

	if _, err := (&http.Client{}).Get(srv.URL); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.HasPrefix(got, "fluctio-agent/") {
		t.Fatalf("User-Agent = %q, want fluctio-agent/ prefix", got)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/explicit", nil)
	req.Header.Set("User-Agent", "caller/1.0")
	if _, err := http.DefaultClient.Do(req); err != nil {
		t.Fatalf("get explicit: %v", err)
	}
	if gotExplicit != "caller/1.0" {
		t.Fatalf("User-Agent = %q, want caller-set value untouched", gotExplicit)
	}
}

// Web content fetches must not self-identify as bots — the browser shape
// is the anti-block contract; the trailing Fluctio/<version> keeps us
// identifiable.
func TestFetchUserAgentBrowserShaped(t *testing.T) {
	ua := FetchUserAgent()
	if !strings.HasPrefix(ua, "Mozilla/5.0") {
		t.Fatalf("FetchUserAgent = %q, want Mozilla/5.0 prefix", ua)
	}
	if !strings.Contains(ua, "Chrome/") || !strings.Contains(ua, "Safari/") {
		t.Fatalf("FetchUserAgent = %q, want Chrome+Safari tokens", ua)
	}
	if !strings.HasSuffix(ua, " Fluctio/"+buildinfo.Version) {
		t.Fatalf("FetchUserAgent = %q, want Fluctio/<version> suffix", ua)
	}
}

// SafeClient is the arbitrary-URL content-fetch path (web_fetch, KB
// ingestion, attachments) — it must send the browser-shaped UA by
// default, with caller-set headers still winning.
func TestSafeClientSendsBrowserUA(t *testing.T) {
	// httptest serves on 127.0.0.1, which SafeDialContext blocks without
	// the documented test escape hatch.
	t.Setenv("FLUCTIO_ALLOW_UNSAFE_LOOPBACK_FETCH", "1")

	var got, gotExplicit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/explicit" {
			gotExplicit = r.UserAgent()
		} else {
			got = r.UserAgent()
		}
	}))
	defer srv.Close()

	resp, err := SafeClient(0).Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if !strings.HasPrefix(got, "Mozilla/5.0") || !strings.Contains(got, " Fluctio/") {
		t.Fatalf("User-Agent = %q, want browser-shaped Fluctio UA", got)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/explicit", nil)
	req.Header.Set("User-Agent", "caller/1.0")
	resp, err = SafeClient(0).Do(req)
	if err != nil {
		t.Fatalf("get explicit: %v", err)
	}
	resp.Body.Close()
	if gotExplicit != "caller/1.0" {
		t.Fatalf("User-Agent = %q, want caller-set value untouched", gotExplicit)
	}
}
