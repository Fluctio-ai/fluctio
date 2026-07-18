package agent

import "testing"

func TestClassifyToolError(t *testing.T) {
	cases := []struct{ in, wantCat, wantHintSub string }{
		{"exec: bash: executable file not found in %PATH%\n[Analyze...]", "env_missing", "替代"},
		{"open /x/y: no such file or directory", "env_missing", "替代"},
		{"'/bin/sh: foo: command not found'", "env_missing", "替代"},
		{"open /etc/shadow: permission denied", "permission", ""},
		{"createfile access is denied", "permission", ""},
		{"upstream_error: 503 Service busy", "external", "重试"},
		{"HTTP 500 internal server error", "external", "重试"},
		{"context deadline exceeded (timeout)", "external", "重试"},
		{"invalid argument: missing required field", "logic", "参数"},
		{"just some normal success result text", "", ""}, // 非失败 → 不分类
	}
	for _, c := range cases {
		gotCat, gotHint := classifyToolError(c.in)
		if gotCat != c.wantCat {
			t.Errorf("classifyToolError(%q) category = %q, want %q", c.in, gotCat, c.wantCat)
		}
		if c.wantHintSub != "" && gotHint == "" {
			t.Errorf("classifyToolError(%q) hint empty, want containing %q", c.in, c.wantHintSub)
		}
	}
}
