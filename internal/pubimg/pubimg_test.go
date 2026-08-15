package pubimg

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestImage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "img.png")
	// Minimal valid PNG (1x1 transparent), enough for ServeFile content checks.
	if err := os.WriteFile(path, []byte{
		0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', // PNG magic
		0, 0, 0, 0x0d, 'I', 'H', 'D', 'R',
	}, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPublicizeAndServe(t *testing.T) {
	s := NewStore("https://fluctio.example.com")
	img := newTestImage(t)

	url, err := s.Publicize(img)
	if err != nil {
		t.Fatalf("Publicize: %v", err)
	}
	const wantPrefix = "https://fluctio.example.com/api/pubimg/"
	if !strings.HasPrefix(url, wantPrefix) {
		t.Errorf("url = %q, want prefix %q", url, wantPrefix)
	}
	token := strings.TrimPrefix(url, wantPrefix)
	if len(token) != 64 { // 32 bytes hex
		t.Errorf("token len = %d, want 64", len(token))
	}

	r := httptest.NewRequest(http.MethodGet, "/api/pubimg/"+token, nil)
	r.SetPathValue("token", token)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if w.Body.Len() != 16 {
		t.Errorf("body len = %d, want 16", w.Body.Len())
	}
}

func TestServeUnknownAndExpiredTokens(t *testing.T) {
	s := NewStore("https://x.example")
	img := newTestImage(t)
	url, err := s.Publicize(img)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(url, "https://x.example/api/pubimg/")

	// Unknown token → 404.
	r := httptest.NewRequest(http.MethodGet, "/api/pubimg/deadbeef", nil)
	r.SetPathValue("token", "deadbeef")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown token status = %d, want 404", w.Code)
	}

	// Expired token → 404 (age it in place).
	s.mu.Lock()
	e := s.items[token]
	e.expires = time.Now().Add(-time.Second)
	s.items[token] = e
	s.mu.Unlock()
	r2 := httptest.NewRequest(http.MethodGet, "/api/pubimg/"+token, nil)
	r2.SetPathValue("token", token)
	w2 := httptest.NewRecorder()
	s.ServeHTTP(w2, r2)
	if w2.Code != http.StatusNotFound {
		t.Errorf("expired token status = %d, want 404", w2.Code)
	}
}

func TestDisabledStore(t *testing.T) {
	s := NewStore("")
	if s.Enabled() {
		t.Error("empty base should be disabled")
	}
	if _, err := s.Publicize("/tmp/x.png"); err == nil {
		t.Error("Publicize on disabled store should error")
	}
}

func TestPublicizeMissingFile(t *testing.T) {
	s := NewStore("https://x.example")
	if _, err := s.Publicize(filepath.Join(t.TempDir(), "nope.png")); err == nil {
		t.Error("missing file should error")
	}
}

func TestEvictionCap(t *testing.T) {
	// Build a store with a tiny cap by direct manipulation: register
	// maxEntries+1 files and confirm the table never exceeds maxEntries.
	s := NewStore("https://x.example")
	dir := t.TempDir()
	for i := 0; i <= maxEntries; i++ {
		p := filepath.Join(dir, "i.png")
		if i == 0 {
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Publicize(p); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
	s.mu.Lock()
	n := len(s.items)
	s.mu.Unlock()
	if n != maxEntries {
		t.Errorf("items = %d, want capped at %d", n, maxEntries)
	}
}
