// Package pubimg serves short-lived, unauthenticated image URLs for
// external vision endpoints that only accept public http(s) URLs.
//
// Some vision providers (e.g. Agnes 2.5 Flash) reject inline data: URLs and
// fetch the image from a URL instead. The gateway is normally fully
// authenticated, so workspace files are not reachable by an anonymous
// fetcher. This package bridges that gap: the vision tool registers the
// local file under a random token and hands the provider
// <PublicBaseURL>/api/pubimg/<token>, which this package serves without
// auth until the entry expires.
//
// Configure with FLUCTIO_PUBLIC_BASE_URL (e.g. https://fluctio.example.com).
// Empty = disabled: local images keep going out as data: URLs and the
// bridge is never mounted. The operator setting the env var is asserting
// that this base URL resolves to this instance from the public internet.
package pubimg

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// ttl bounds how long a registered URL stays fetchable. A vision call
	// has a 60s HTTP timeout; 15 minutes comfortably covers a chain retry
	// or two while keeping the anonymous window short.
	ttl = 15 * time.Minute
	// maxEntries caps the token table so a busy instance can't grow it
	// unboundedly between sweeps. Eviction drops the oldest first.
	maxEntries = 256
	// maxFileBytes caps what Publicize is willing to expose. Vision
	// inputs are downscale-capped at 1568px anyway; 20MB leaves generous
	// headroom for high-res photos.
	maxFileBytes = 20 << 20
)

type entry struct {
	path    string
	expires time.Time
	seq     int64 // insertion order, for oldest-first eviction
}

// Store maps random tokens to local image paths for a bounded time.
type Store struct {
	mu    sync.Mutex
	base  string
	items map[string]entry
	next  int64
}

// NewStore returns a store that mints URLs under base (e.g.
// "https://fluctio.example.com"). Empty base = disabled store.
func NewStore(base string) *Store {
	return &Store{base: strings.TrimRight(base, "/"), items: make(map[string]entry)}
}

// Enabled reports whether a public base URL was configured.
func (s *Store) Enabled() bool { return s != nil && s.base != "" }

// Publicize registers path and returns its short-lived public URL.
// The file must exist and be within maxFileBytes; the URL is fetchable
// without auth until ttl elapses or maxEntries pressure evicts it.
func (s *Store) Publicize(path string) (string, error) {
	if !s.Enabled() {
		return "", fmt.Errorf("pubimg: disabled (no FLUCTIO_PUBLIC_BASE_URL)")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("pubimg: resolve %q: %w", path, err)
	}
	if fi, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("pubimg: stat %q: %w", path, err)
	} else if fi.Size() > maxFileBytes {
		return "", fmt.Errorf("pubimg: %q exceeds %d bytes", path, maxFileBytes)
	}
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return "", fmt.Errorf("pubimg: token: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(time.Now())
	// Cap pressure check after sweeping; if still full, evict oldest.
	for len(s.items) >= maxEntries {
		var oldestKey string
		var oldest entry
		for k, e := range s.items {
			if oldestKey == "" || e.seq < oldest.seq {
				oldestKey, oldest = k, e
			}
		}
		delete(s.items, oldestKey)
	}
	s.next++
	token := hex.EncodeToString(tok)
	s.items[token] = entry{path: abs, expires: time.Now().Add(ttl), seq: s.next}
	return s.base + "/api/pubimg/" + token, nil
}

// ServeHTTP implements GET /api/pubimg/{token} without authentication.
// Expired or unknown tokens return 404 with no existence signal.
func (s *Store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := r.PathValue("token")
	s.mu.Lock()
	e, ok := s.items[token]
	if ok && time.Now().After(e.expires) {
		delete(s.items, token)
		ok = false
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Token URLs are single-purpose secrets; never let a shared proxy or
	// browser cache extend their lifetime.
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, e.path)
}

// sweepLocked drops expired entries. Caller holds s.mu.
func (s *Store) sweepLocked(now time.Time) {
	for k, e := range s.items {
		if now.After(e.expires) {
			delete(s.items, k)
		}
	}
}

// defaultStore is the process-wide instance wired at boot by Configure
// and read by registerAgentToolChains when building agents. Like the
// tool-provider registry, it is process-singleton state: one HTTP endpoint,
// one token table.
var defaultStore = NewStore("")

// Configure sets the public base URL for the process-wide store. Called
// once at boot from the FLUCTIO_PUBLIC_BASE_URL env var; empty leaves the
// bridge disabled.
func Configure(base string) { defaultStore = NewStore(base) }

// Default returns the process-wide store.
func Default() *Store { return defaultStore }

// Mount registers the token route on mux when the store is enabled.
func Mount(mux *http.ServeMux) {
	if defaultStore.Enabled() {
		mux.Handle("GET /api/pubimg/{token}", defaultStore)
	}
}

// Enabled reports whether the process-wide bridge is configured.
func Enabled() bool { return defaultStore.Enabled() }

// Publicize registers path with the process-wide store.
func Publicize(path string) (string, error) { return defaultStore.Publicize(path) }
