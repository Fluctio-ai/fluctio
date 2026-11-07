package setup

import (
	"fmt"
	"testing"
)

func TestLoginRateLimiter(t *testing.T) {
	// Fresh Server: loginAllowed runs before any loginFailed ever created
	// the map — the first call must not panic on a nil map (regression
	// guard for the bug the 12-12:34 gateway panics caught).
	s := &Server{}
	if !s.loginAllowed("1.2.3.4", "u") {
		t.Fatal("first attempt denied")
	}
	// loginAllowed consumes a slot per attempt (check and record are
	// atomic), so loginMaxAttempts - 1 further attempts fill the bucket
	// booked by the first call.
	for i := 0; i < loginMaxAttempts-1; i++ {
		if !s.loginAllowed("1.2.3.4", "u") {
			t.Fatalf("attempt %d denied early", i+2)
		}
		s.loginFailed("1.2.3.4", "u")
	}
	if s.loginAllowed("1.2.3.4", "u") {
		t.Fatal("attempt beyond the cap allowed")
	}
	if !s.loginAllowed("1.2.3.4", "other") {
		t.Fatal("a different login shares the bucket")
	}
	if !s.loginAllowed("5.6.7.8", "u") {
		t.Fatal("a different ip shares the bucket")
	}
	s.loginSucceeded("1.2.3.4", "u")
	if !s.loginAllowed("1.2.3.4", "u") {
		t.Fatal("successful login should clear its window")
	}
}

func TestLoginRateLimiterIPSprayCap(t *testing.T) {
	// Password spraying: rotating the login value must not bypass the
	// per-IP bucket.
	s := &Server{}
	ip := "9.9.9.9"
	for i := 0; i < loginIPMaxAttempts; i++ {
		if !s.loginAllowed(ip, fmt.Sprintf("user%d", i)) {
			t.Fatalf("per-ip attempt %d denied early", i+1)
		}
	}
	if s.loginAllowed(ip, "one-more") {
		t.Fatal("per-IP spray cap not enforced")
	}
	// A different IP is unaffected.
	if !s.loginAllowed("8.8.8.8", "user0") {
		t.Fatal("per-IP cap leaked to another address")
	}
}
