package tools

import (
	"context"
	"testing"
	"time"
)

type fakeExecutor struct{}

func (fakeExecutor) Exec(context.Context, string, time.Duration) (string, error) { return "", nil }
func (fakeExecutor) ReadFile(context.Context, string) (string, error)            { return "", nil }
func (fakeExecutor) WriteFile(context.Context, string, string) (string, error)   { return "", nil }
func (fakeExecutor) ListDir(context.Context, string) (string, error)             { return "", nil }
func (fakeExecutor) Backend() string                                             { return "test" }
func (fakeExecutor) Close() error                                                { return nil }

func TestFluctioInternalPathsIncludeRootAndEnvHome(t *testing.T) {
	t.Setenv("FLUCTIO_HOME", "/srv/fluctio")

	cases := []string{
		"~/.fluctio",
		"~/.fluctio/workspaces",
		"/root/.fluctio",
		"/root/.fluctio/workspaces",
		"/srv/fluctio",
		"/srv/fluctio/workspaces",
	}
	for _, path := range cases {
		if !isFluctioInternalPath(path) {
			t.Fatalf("isFluctioInternalPath(%q) = false, want true", path)
		}
		if got, ok := hostHomePath(path); ok || got != "" {
			t.Fatalf("hostHomePath(%q) = (%q, %v), want denied", path, got, ok)
		}
	}
}

func TestRouteForDoesNotExposeFluctioInternalsViaHostFS(t *testing.T) {
	r := &Registry{executor: fakeExecutor{}}
	r.SetCallerHostTrusted(true)

	if got := r.routeFor("/root/.fluctio/workspaces", OpList); got != RouteSandbox {
		t.Fatalf("routeFor Fluctio internal path = %v, want RouteSandbox", got)
	}
	if got := r.routeFor("/Users/maxwell/Documents/report.txt", OpRead); got != RouteHostFS {
		t.Fatalf("routeFor explicit host document = %v, want RouteHostFS", got)
	}
}

// Host-scope paths are an operator-only reach: an untrusted chatter
// (anonymous IM guest — note callerIsAdmin stays true on single-user
// installs while host trust doesn't) referencing the operator's home
// must stay inside the sandbox, where the path simply doesn't exist.
func TestRouteForBlocksHostScopeForUntrustedCaller(t *testing.T) {
	r := &Registry{executor: fakeExecutor{}}
	r.SetCallerIsAdmin(true) // single-user loosening — must NOT grant host FS
	r.SetCallerHostTrusted(false)

	if got := r.routeFor("/Users/maxwell/Documents/report.txt", OpRead); got != RouteSandbox {
		t.Fatalf("routeFor host document for untrusted caller = %v, want RouteSandbox", got)
	}
	if got := r.routeFor("/home/maxwell/.ssh/id_rsa", OpRead); got != RouteSandbox {
		t.Fatalf("routeFor /home/.ssh for untrusted caller = %v, want RouteSandbox", got)
	}
}
