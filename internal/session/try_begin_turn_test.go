package session

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TryBeginTurn is the concurrency primitive the failure sweep's replay
// path relies on: exactly one caller may claim a turn when none is in
// flight. These tests pin the exclusive-claim and release semantics —
// the sweep's probe-then-dispatch race (a message landing between the
// TurnInFlight probe and the turn start) is exactly the regression this
// guards against.

func TestTryBeginTurnExclusiveClaim(t *testing.T) {
	s := &Session{}
	if !s.TryBeginTurn() {
		t.Fatal("first TryBeginTurn on an idle session should win")
	}
	if s.TryBeginTurn() {
		t.Fatal("second TryBeginTurn while a turn is in flight must lose")
	}
	// Depth accounting: EndTurn releases the claim taken by TryBeginTurn.
	s.EndTurn()
	if !s.TryBeginTurn() {
		t.Fatal("TryBeginTurn after EndTurn should win again")
	}
	s.EndTurn()
}

func TestTryBeginTurnConcurrentSingleWinner(t *testing.T) {
	s := &Session{}
	const contenders = 16
	var winners atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all contenders in the same tick
			if s.TryBeginTurn() {
				winners.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("expected exactly 1 winner among %d contenders, got %d", contenders, got)
	}
	// The winner's EndTurn makes the session reusable.
	s.EndTurn()
	if !s.TryBeginTurn() {
		t.Fatal("session should be reusable after the winner ends its turn")
	}
	s.EndTurn()
}

func TestTryBeginTurnNestedDepthMatchesBeginTurn(t *testing.T) {
	s := &Session{}
	// Depth accounting must match BeginTurn so a mixed pattern (out-of-band
	// replay claiming via TryBeginTurn, then the real turn beginning via
	// BeginTurn) unwinds cleanly through EndTurn.
	if !s.TryBeginTurn() {
		t.Fatal("TryBeginTurn should win on an idle session")
	}
	s.BeginTurn() // nested — depth 2
	s.EndTurn()   // depth 1
	if s.TryBeginTurn() {
		t.Fatal("depth 1 is still an active turn — TryBeginTurn must lose")
	}
	s.EndTurn() // depth 0
	if !s.TryBeginTurn() {
		t.Fatal("session idle again after the outer EndTurn")
	}
	s.EndTurn()
}
