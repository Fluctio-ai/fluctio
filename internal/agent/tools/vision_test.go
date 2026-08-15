package tools

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestMapWorkspaceImagePath(t *testing.T) {
	root := filepath.Join("workspaces", "agt_1", "projects", "proj_1")
	r := NewRegistry("sys", root)

	cases := []struct{ in, want string }{
		{"/workspace", root},
		{"/workspace/ocr_pages/p6.png", filepath.Join(root, "ocr_pages", "p6.png")},
		{"/workspace/xjxk_p5.png", filepath.Join(root, "xjxk_p5.png")},
		// Absolute host paths (e.g. the server's /www/fluctio/...) pass
		// through unchanged — only the sandbox's logical prefix maps.
		{"/www/fluctio/workspaces/a.png", "/www/fluctio/workspaces/a.png"},
		{"relative.png", "relative.png"},
	}
	for _, c := range cases {
		if got := mapWorkspaceImagePath(r, c.in); got != c.want {
			t.Errorf("mapWorkspaceImagePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// No bound user root: leave the path untouched rather than guessing.
	rEmpty := NewRegistry("sys", "")
	if got := mapWorkspaceImagePath(rEmpty, "/workspace/a.png"); got != "/workspace/a.png" {
		t.Errorf("empty root: got %q, want unchanged path", got)
	}
}

func TestIsHTTP4xx(t *testing.T) {
	cases := []struct {
		err  string
		want bool
	}{
		{"openai/agnes-2.5-flash: openai HTTP 400: {\"error\":{\"code\":\"invalid_request\"}}", true},
		{"openai HTTP 422: malformed image_url", true},
		{"openai HTTP 429: rate limited", true}, // coarse by design: costs one doomed retry
		{"openai HTTP 500: upstream boom", false},
		{"openai HTTP 503: overloaded", false},
		{"openai vision request: dial tcp: timeout", false},
		{"", false},
	}
	for _, c := range cases {
		var err error
		if c.err != "" {
			err = fmt.Errorf("%s", c.err)
		}
		if got := isHTTP4xx(err); got != c.want {
			t.Errorf("isHTTP4xx(%q) = %v, want %v", c.err, got, c.want)
		}
	}
	if isHTTP4xx(nil) {
		t.Error("isHTTP4xx(nil) = true, want false")
	}
}
