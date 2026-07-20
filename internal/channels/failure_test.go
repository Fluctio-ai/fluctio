package channels

import (
	"testing"
	"time"
)

func TestFailureCounter_HitThreshold(t *testing.T) {
	fc := NewFailureCounter(3, time.Second, 10*time.Second)
	// First two hits below threshold.
	if fc.Hit() {
		t.Fatalf("Hit() = true at failure 1, want false (threshold 3)")
	}
	if fc.Hit() {
		t.Fatalf("Hit() = true at failure 2, want false")
	}
	// Third hit trips the threshold.
	if !fc.Hit() {
		t.Fatalf("Hit() = false at failure 3, want true")
	}
	if fc.Count() != 3 {
		t.Fatalf("Count() = %d, want 3", fc.Count())
	}
}

func TestFailureCounter_Reset(t *testing.T) {
	fc := NewFailureCounter(2, time.Second, 10*time.Second)
	fc.Hit()
	fc.Hit()
	if fc.Count() != 2 {
		t.Fatalf("Count() = %d, want 2", fc.Count())
	}
	fc.Reset()
	if fc.Count() != 0 {
		t.Fatalf("Count() after Reset = %d, want 0", fc.Count())
	}
	// After reset, threshold should trip again from scratch.
	if fc.Hit() {
		t.Fatalf("Hit() = true right after Reset, want false")
	}
	if !fc.Hit() {
		t.Fatalf("Hit() = false on second failure after Reset, want true")
	}
}

func TestFailureCounter_Backoff(t *testing.T) {
	fc := NewFailureCounter(10, time.Second, 8*time.Second)
	// failure 1 → initial (1s)
	fc.Hit()
	if got := fc.Backoff(); got != time.Second {
		t.Fatalf("Backoff after 1 failure = %v, want 1s", got)
	}
	// failure 2 → 2s
	fc.Hit()
	if got := fc.Backoff(); got != 2*time.Second {
		t.Fatalf("Backoff after 2 failures = %v, want 2s", got)
	}
	// failure 3 → 4s
	fc.Hit()
	if got := fc.Backoff(); got != 4*time.Second {
		t.Fatalf("Backoff after 3 failures = %v, want 4s", got)
	}
	// failure 4 → would be 8s (= max, not 16s)
	fc.Hit()
	if got := fc.Backoff(); got != 8*time.Second {
		t.Fatalf("Backoff after 4 failures = %v, want 8s (capped)", got)
	}
	// failure 5 → still max
	fc.Hit()
	if got := fc.Backoff(); got != 8*time.Second {
		t.Fatalf("Backoff after 5 failures = %v, want 8s (capped)", got)
	}
}
