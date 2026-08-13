package gateway

import (
	"testing"
)

// AC3 — env parsing drives the sweep gate: empty uses the default, "0"/negative
// disables that state, and a bogus value disables (returns -1) rather than
// running with a garbage window. Defaults are 168h (7d) success / 720h (30d)
// failed.
func TestWorkflowRetentionHours(t *testing.T) {
	cases := []struct {
		name                  string
		successEnv, failedEnv string
		wantS, wantF          int
	}{
		{"defaults", "", "", 168, 720},
		{"both zero", "0", "0", 0, 0},
		{"both negative", "-1", "-5", -1, -5},
		{"bogus disables", "garbage", "x", -1, -1},
		{"mixed", "0", "", 0, 720},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("FLUCTIO_WORKFLOW_RETENTION_SUCCESS_HOURS", c.successEnv)
			t.Setenv("FLUCTIO_WORKFLOW_RETENTION_FAILED_HOURS", c.failedEnv)
			gotS, gotF := workflowRetentionHours()
			if gotS != c.wantS || gotF != c.wantF {
				t.Errorf("got (%d,%d), want (%d,%d)", gotS, gotF, c.wantS, c.wantF)
			}
		})
	}
}
