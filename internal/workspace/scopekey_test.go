package workspace

import "testing"

// ScopeKeyPrefix is the single source of the scope→prefix mapping; S3.key
// and S3.scopePrefix build their object keys from it, and file_shares stores
// agent-root keys joined with it. Lock the four scopes down.
func TestScopeKeyPrefix(t *testing.T) {
	cases := []struct {
		pid, sid, want string
	}{
		{"", "", ""},
		{"", "s-1", "sessions/s-1"},
		{"proj_1", "", "projects/proj_1"},
		{"proj_1", "s-2", "projects/proj_1/s-2"},
	}
	for _, c := range cases {
		if got := ScopeKeyPrefix(c.pid, c.sid); got != c.want {
			t.Errorf("ScopeKeyPrefix(%q, %q) = %q, want %q", c.pid, c.sid, got, c.want)
		}
	}
}
