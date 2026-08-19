package setup

import (
	"net"
	"net/http"
	"time"
)

// Login brute-force protection for POST /api/login: at most
// loginMaxAttempts failures per (client IP, login) in a sliding
// loginWindow, then 429 until the window drains. In-memory — every
// supported deployment today is a single process. A successful login
// clears its own window so a run of typos doesn't lock the real user out.
const (
	loginMaxAttempts = 10
	loginWindow      = 5 * time.Minute
)

func (s *Server) loginAllowed(ip, login string) bool {
	key := ip + "|" + login
	cutoff := time.Now().Add(-loginWindow)
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.loginFails == nil {
		s.loginFails = map[string][]time.Time{}
	}
	ts := s.loginFails[key]
	start := 0
	for start < len(ts) && ts[start].Before(cutoff) {
		start++
	}
	ts = ts[start:]
	s.loginFails[key] = ts
	return len(ts) < loginMaxAttempts
}

func (s *Server) loginFailed(ip, login string) {
	key := ip + "|" + login
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.loginFails == nil {
		s.loginFails = map[string][]time.Time{}
	}
	s.loginFails[key] = append(s.loginFails[key], time.Now())
}

func (s *Server) loginSucceeded(ip, login string) {
	key := ip + "|" + login
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	delete(s.loginFails, key)
}

// clientIP returns the TCP peer's host. X-Forwarded-For is deliberately
// NOT trusted: a directly-exposed gateway would let an attacker mint a
// fresh bucket per request by spoofing the header. Behind the operator's
// own reverse proxy every client shares the proxy IP, which is fine —
// the login half of the key still separates users.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
