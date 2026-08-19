package setup

import "testing"

func TestLoginRateLimiter(t *testing.T) {
	// Fresh Server: loginAllowed runs before any loginFailed ever created
	// the map — the first call must not panic on a nil map (regression
	// guard for the bug the 12-12:34 gateway panics caught).
	s := &Server{}
	if !s.loginAllowed("1.2.3.4", "u") {
		t.Fatal("first attempt denied")
	}
	for i := 0; i < loginMaxAttempts; i++ {
		if !s.loginAllowed("1.2.3.4", "u") {
			t.Fatalf("attempt %d denied early", i+1)
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
