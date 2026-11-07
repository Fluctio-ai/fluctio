package setup

import (
	"net"
	"net/http"
	"time"
)

// Login brute-force protection for POST /api/login. Two sliding-window
// buckets, both in-memory (every supported deployment today is a single
// process):
//   - per (client IP, login): at most loginMaxAttempts failures per
//     loginWindow — targets single-account password guessing.
//   - per client IP: at most loginIPMaxAttempts failures per
//     loginWindow — targets password spraying, where the attacker
//     rotates the login value every request and the per-login bucket
//     never fills.
//
// The per-(ip,login) check is "consume": the attempt is booked under the
// same lock that validates it, so N concurrent requests can't all slip
// through the check before any of them records a failure. A successful
// login clears its own bucket, so a run of typos doesn't lock the real
// user out.
const (
	loginMaxAttempts    = 10
	loginIPMaxAttempts  = 50
	loginWindow         = 5 * time.Minute
	loginMaxMapKeys     = 4096
	loginMaxLoginLength = 128
)

// pruneLocked drops timestamps older than cutoff from ts and returns the
// compacted slice. Caller must hold s.loginMu.
func pruneLocked(ts []time.Time, cutoff time.Time) []time.Time {
	start := 0
	for start < len(ts) && ts[start].Before(cutoff) {
		start++
	}
	if start == 0 {
		return ts
	}
	// Copy to a fresh backing array: the reslice would otherwise keep
	// the pruned head of the original array alive for as long as the
	// key stays hot.
	return append([]time.Time(nil), ts[start:]...)
}

// gcLocked bounds memory when an attacker floods /api/login with random
// login values: once the map grows past loginMaxMapKeys, sweep every key
// whose window has fully drained. Caller must hold s.loginMu.
func (s *Server) gcLocked(cutoff time.Time) {
	if len(s.loginFails) <= loginMaxMapKeys {
		return
	}
	for k, ts := range s.loginFails {
		if len(ts) == 0 || ts[len(ts)-1].Before(cutoff) {
			delete(s.loginFails, k)
		}
	}
}

func (s *Server) loginKeys(ip, login string) (string, string) {
	// Cap the login value so garbage input can't balloon map keys; the
	// separator is fine here because the value length is bounded.
	if len(login) > loginMaxLoginLength {
		login = login[:loginMaxLoginLength]
	}
	return ip + "|" + login, ip
}

func (s *Server) loginAllowed(ip, login string) bool {
	key, ipKey := s.loginKeys(ip, login)
	cutoff := time.Now().Add(-loginWindow)
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.loginFails == nil {
		s.loginFails = map[string][]time.Time{}
	}
	s.gcLocked(cutoff)
	// Consume one attempt slot for the (ip,login) bucket atomically.
	ts := pruneLocked(s.loginFails[key], cutoff)
	if len(ts) >= loginMaxAttempts {
		s.loginFails[key] = ts
		return false
	}
	ts = append(ts, time.Now())
	s.loginFails[key] = ts
	// Same consume for the per-IP spray bucket.
	ips := pruneLocked(s.loginFails[ipKey], cutoff)
	if len(ips) >= loginIPMaxAttempts {
		s.loginFails[ipKey] = ips
		return false
	}
	s.loginFails[ipKey] = append(ips, time.Now())
	return true
}

func (s *Server) loginFailed(ip, login string) {
	// Failure timestamps were already booked by loginAllowed's consume;
	// this hook is kept for symmetry with loginSucceeded.
}

func (s *Server) loginSucceeded(ip, login string) {
	key, ipKey := s.loginKeys(ip, login)
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	delete(s.loginFails, key)
	// One success doesn't clear the per-IP bucket (other logins from the
	// same IP may still be under attack); drop exactly the one slot this
	// successful attempt consumed.
	if ts := s.loginFails[ipKey]; len(ts) > 0 {
		s.loginFails[ipKey] = ts[1:]
	}
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
