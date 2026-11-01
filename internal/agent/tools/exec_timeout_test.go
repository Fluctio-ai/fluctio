package tools

import "testing"

// TestClampExecTimeout verifies the model-reported exec timeout stays
// bounded: a self-reported 3600 would park the turn — and any steered
// user message waiting between tool rounds — for an hour. Long jobs
// have run_in_background (host) / tmux (sandbox) as the intended path.
func TestClampExecTimeout(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, 120},    // unset → default
		{-5, 120},   // garbage → default
		{1, 1},      // small values pass through
		{300, 300},  // camoufox cold-start ceiling still fits
		{600, 600},  // exact cap
		{3600, 600}, // runaway self-report clamped
	}
	for _, c := range cases {
		if got := clampExecTimeout(c.in); got != c.want {
			t.Errorf("clampExecTimeout(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
